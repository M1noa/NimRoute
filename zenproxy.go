package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const proxyListURL = "https://proxies.minoa.cat/list?format=json&sort=response&limit=0&response_max=400"

// zenBlockedCountries are api-reported countries whose exits zen geo-blocks
// for muse-spark (RegionError "not available in your country"). from the
// scripts/probe_zen_proxies.py sweep: PK 4/4 blocked, RU/VE/HK blocked with 0 ok,
// BY/TJ/IQ/MM blocked with 0 ok.
var (
	zenBlockedMu        sync.RWMutex
	zenBlockedCountries = map[string]bool{
		"PK": true, "RU": true, "VE": true, "HK": true,
		"BY": true, "TJ": true, "IQ": true, "MM": true,
	}
)

func zenBlocked(cc string) bool {
	zenBlockedMu.RLock()
	defer zenBlockedMu.RUnlock()
	return zenBlockedCountries[cc]
}

var (
	zenProxiesMu sync.RWMutex
	zenProxies   []string
	zenNetErrMu  sync.Mutex
	zenNetErrs   int
	zenGeoMu     sync.Mutex
	zenGeoErrs   int
	zenBlockMu   sync.Mutex
	zenBlockErrs int
	// api-reported proxy -> country, rebuilt on each refresh.
	zenProxyCountryMu sync.RWMutex
	zenProxyCountry   = map[string]string{}
	// api-reported proxy -> response ms, rebuilt on each refresh.
	zenLatencyMu sync.RWMutex
	zenLatency   = map[string]int{}
	// api-reported proxy -> reliability 0..1, rebuilt on each refresh.
	zenReliMu sync.RWMutex
	zenReli   = map[string]float64{}
	// live first-byte rtt per exit (ewma ms), from real zen sends. refines
	// the api-reported latency in proxyWeight: exits that answer fast win
	// more draws, stalled ones sink without waiting for a slow strike.
	zenLiveMu sync.RWMutex
	zenLive   = map[string]float64{}
	// geo/user blocks per country, for hardcoding bad regions.
	zenCountryBlockMu sync.Mutex
	zenCountryBlocks  = map[string]int{}
	// pool stats for /status: last refresh time, candidates seen,
	// total dropped exits. guarded by zenProxiesMu.
	zenPoolRefreshed time.Time
	zenPoolCands     int
	zenPoolDropped   int
)

// zenVersion is the opencode release tag reported in the User-Agent. Defaults
// to 1.18.31 and is refreshed from GitHub on startup and every 6h after.
// guarded by a mutex: the refresher goroutine writes it while request
// goroutines read it for every zen request.
var (
	zenVersionMu sync.RWMutex
	zenVersion   = "1.18.31"
)

func zenVersionStr() string {
	zenVersionMu.RLock()
	defer zenVersionMu.RUnlock()
	return zenVersion
}

// the ai-sdk suffix the real client appends to its own User-Agent: the
// provider-utils version it runs and the js runtime underneath it.
const (
	zenSDKVersion = "4.0.23"
	zenRuntime    = "bun/1.3.14"
)

func zenUserAgent() string {
	return "opencode/" + zenVersionStr() + " ai-sdk/provider-utils/" + zenSDKVersion + " runtime/" + zenRuntime
}

// fetchOpenCodeVersion pulls the latest opencode release tag from the GitHub
// API, falling back to the default on any error.
func fetchOpenCodeVersion() {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get("https://api.github.com/repos/sst/opencode/releases/latest")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return
	}
	v := strings.TrimPrefix(strings.TrimSpace(r.TagName), "v")
	if v != "" {
		zenVersionMu.Lock()
		changed := v != zenVersion
		zenVersion = v
		zenVersionMu.Unlock()
		if changed {
			log.Printf("  opencode version: %s", v)
		}
	}
}

// watchOpenCodeVersion refreshes the User-Agent version every 6h.
func watchOpenCodeVersion() {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for range t.C {
		fetchOpenCodeVersion()
	}
}

type proxyEntry struct {
	IP             string   `json:"ip"`
	Port           int      `json:"port"`
	Country        string   `json:"country"`
	HTTPS          bool     `json:"https"`
	Protocols      []string `json:"protocols"`
	Anonymity      string   `json:"anonymity"`
	Reliability    float64  `json:"reliability"`
	Quality        float64  `json:"quality"`
	ResponseTimeMs int      `json:"response_time_ms"`
}

func schemeFor(protocols []string) string {
	has := map[string]bool{}
	for _, p := range protocols {
		has[p] = true
	}
	if has["socks4"] {
		return "socks4"
	}
	if has["socks5"] {
		return "socks5"
	}
	if has["http"] {
		return "http"
	}
	return ""
}

func refreshZenProxies() {
	cl := &http.Client{Timeout: 15 * time.Second}
	resp, err := cl.Get(proxyListURL)
	if err != nil {
		log.Printf("  zen proxies: fetch failed: %v", err)
		return
	}
	defer resp.Body.Close()
	// cap the third-party body: a compromised list server must not feed
	// unbounded allocation into the hourly refresh.
	var list []proxyEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&list); err != nil {
		log.Printf("  zen proxies: decode failed: %v", err)
		return
	}
	c := cfg() // caps live in config.yml zen section
	var fast []proxyEntry
	for _, e := range list {
		if e.Port == 0 || e.IP == "" {
			continue
		}
		if schemeFor(e.Protocols) == "" {
			continue
		}
		if c.Zen.MaxResponseMs > 0 && e.ResponseTimeMs > c.Zen.MaxResponseMs {
			continue
		}
		if zenBlocked(strings.ToUpper(strings.TrimSpace(e.Country))) {
			continue
		}
		if !e.HTTPS && schemeFor(e.Protocols) == "http" {
			continue // https-only zen, no-tls http proxies can't connect
		}
		fast = append(fast, e)
	}
	sort.Slice(fast, func(i, j int) bool { return fast[i].ResponseTimeMs < fast[j].ResponseTimeMs })
	if n := c.Zen.PoolSize; n > 0 && len(fast) > n {
		fast = fast[:n]
	}
	cands := make([]string, 0, len(fast))
	countries := make(map[string]string, len(fast))
	lats := make(map[string]int, len(fast))
	relis := make(map[string]float64, len(fast))
	for _, e := range fast {
		u := schemeFor(e.Protocols) + "://" + e.IP + ":" + strconv.Itoa(e.Port)
		if exitTLSBanned(u) {
			continue // mitm exit: banned, never re-verify it
		}
		cands = append(cands, u)
		countries[u] = strings.ToUpper(strings.TrimSpace(e.Country))
		lats[u] = e.ResponseTimeMs
		relis[u] = e.Reliability
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	verified := make([]string, 0, len(cands))
	verifyConc := 40
	if n := c.Zen.VerifyConcurrency; n > 0 {
		verifyConc = n
	}
	// cold sweeps verify wide: the pool is empty and every lane waits, so
	// fan out past the hourly-refresh cap. warm refreshes keep the cap.
	zenProxiesMu.RLock()
	coldSweep := len(zenProxies) == 0
	zenProxiesMu.RUnlock()
	if coldSweep && verifyConc < 200 {
		verifyConc = 200
	}
	// cold-start publish: when the live pool is empty, exits become usable as
	// soon as they verify instead of after the whole sweep, so the first
	// request waits on the fastest single exit, not on all cands. a pool that
	// is already serving is left alone: the final applyVerifiedPool below owns
	// that swap, so an hourly refresh never churns exits mid-sweep.
	cold := coldSweep
	lastPub, warmed := time.Time{}, false
	publish := func() {
		if !cold {
			return
		}
		mu.Lock()
		if time.Since(lastPub) < 500*time.Millisecond {
			mu.Unlock()
			return
		}
		lastPub = time.Now()
		snap := append([]string(nil), verified...)
		mu.Unlock()
		if len(snap) == 0 {
			return
		}
		kc, kl, kr := map[string]string{}, map[string]int{}, map[string]float64{}
		for _, v := range snap {
			if co, ok := countries[v]; ok {
				kc[v] = co
			}
			if l, ok := lats[v]; ok {
				kl[v] = l
			}
			if r, ok := relis[v]; ok {
				kr[v] = r
			}
		}
		// setters replace wholesale, safe here only: publish runs while the
		// live pool is still empty, so there is no serving set to clobber.
		setProxyCountries(kc)
		setProxyLatency(kl)
		setProxyReliability(kr)
		zenProxiesMu.Lock()
		zenProxies = snap
		zenPoolRefreshed = time.Now()
		zenPoolCands = len(cands)
		zenProxiesMu.Unlock()
		if !warmed {
			warmed = true
			// lanes + transports on the first usable exits, so the
			// earliest request skips lane creation and tls setup.
			ensureSharedLanes()
			warmZenTransports(snap)
			log.Printf("  zen proxies: %d verified (pool warming, %d candidates left)", len(snap), len(cands)-len(snap))
		}
	}
	sem := make(chan struct{}, verifyConc)
	for _, u := range cands {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if verifyProxy(u) {
				mu.Lock()
				verified = append(verified, u)
				mu.Unlock()
				publish()
			}
		}(u)
	}
	wg.Wait()

	// metadata commits only with a real pool: a failed sweep must not
	// clobber the live country/latency/reliability maps for the old exits.
	if len(verified) > 0 {
		kc, kl, kr := map[string]string{}, map[string]int{}, map[string]float64{}
		for _, v := range verified {
			if c, ok := countries[v]; ok {
				kc[v] = c
			}
			if l, ok := lats[v]; ok {
				kl[v] = l
			}
			if r, ok := relis[v]; ok {
				kr[v] = r
			}
		}
		setProxyCountries(kc)
		setProxyLatency(kl)
		setProxyReliability(kr)
		saveZenPool(verified, kc, kl, kr)
	}
	applyVerifiedPool(verified)
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now()
	zenPoolCands = len(cands)
	zenProxiesMu.Unlock()
	// pre-open the shared lanes now that exits exist: cold starts spread
	// from the first request instead of serializing on lane creation.
	// also pre-build transports so the first send skips tls/dial setup.
	ensureSharedLanes()
	warmZenTransports(verified)
	log.Printf("  zen proxies: %d verified of %d candidates", len(verified), len(cands))
}

// savedPool is the last verified pool, loaded at boot so requests serve
// instantly while the fresh sweep re-verifies in the background.
type savedPool struct {
	Proxies []string           `json:"proxies"`
	Country map[string]string  `json:"country"`
	Latency map[string]int     `json:"latency"`
	Reli    map[string]float64 `json:"reli"`
}

// poolCachePath sits next to the config file so test instances keep their
// own cache (same rule as configPath: CONFIG_FILE overrides).
func poolCachePath() string {
	d := filepath.Dir(configPath())
	if d == "" {
		d = "."
	}
	return filepath.Join(d, ".zenpool.json")
}

// saveZenPool persists the verified set best-effort; a stale cache still
// beats an empty pool at boot.
func saveZenPool(verified []string, kc map[string]string, kl map[string]int, kr map[string]float64) {
	out, err := json.Marshal(savedPool{Proxies: verified, Country: kc, Latency: kl, Reli: kr})
	if err != nil {
		return
	}
	os.WriteFile(poolCachePath(), out, 0o600)
}

// loadZenPool restores the last verified pool with its metadata. returns
// false when there is no usable cache.
func loadZenPool() bool {
	raw, err := os.ReadFile(poolCachePath())
	if err != nil {
		return false
	}
	var sp savedPool
	if err := json.Unmarshal(raw, &sp); err != nil || len(sp.Proxies) == 0 {
		return false
	}
	kc := map[string]string{}
	for _, p := range sp.Proxies {
		if c, ok := sp.Country[p]; ok {
			kc[p] = c
		}
	}
	kl := map[string]int{}
	for _, p := range sp.Proxies {
		if l, ok := sp.Latency[p]; ok {
			kl[p] = l
		} else {
			kl[p] = 350
		}
	}
	kr := map[string]float64{}
	for _, p := range sp.Proxies {
		if r, ok := sp.Reli[p]; ok {
			kr[p] = r
		} else {
			kr[p] = 0.5
		}
	}
	setProxyCountries(kc)
	setProxyLatency(kl)
	setProxyReliability(kr)
	zenProxiesMu.Lock()
	zenProxies = append([]string(nil), sp.Proxies...)
	zenPoolRefreshed = time.Now()
	zenProxiesMu.Unlock()
	return true
}

// verifyProxy confirms a proxy can reach the zen API via a lightweight GET.
// 5s is plenty: working exits answer in under a second, dead ones either
// refuse instantly or hang to the timeout and hold a verify worker.
func verifyProxy(proxyURL string) bool {
	cl := zenClient(proxyURL)
	cl.Timeout = 5 * time.Second
	req, err := http.NewRequest("GET", "https://opencode.ai/zen/v1/models", nil)
	if err != nil {
		return false
	}
	setZenHeaders(req, zenSession())
	resp, err := cl.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if err != nil {
		return false
	}
	return strings.Contains(string(body), `"object":"list"`)
}

// setProxyLatency replaces the proxy->response-ms map after a refresh.
func setProxyLatency(m map[string]int) {
	zenLatencyMu.Lock()
	defer zenLatencyMu.Unlock()
	zenLatency = m
}

// proxyLatency returns api-reported ms, big default when unknown.
func proxyLatency(proxyURL string) int {
	zenLatencyMu.RLock()
	defer zenLatencyMu.RUnlock()
	if ms := zenLatency[proxyURL]; ms > 0 {
		return ms
	}
	return 10000
}

// setProxyReliability replaces the proxy->reliability map after a refresh.
func setProxyReliability(m map[string]float64) {
	zenReliMu.Lock()
	defer zenReliMu.Unlock()
	zenReli = m
}

// proxyReliability returns api-reported 0..1, neutral default when unknown.
func proxyReliability(proxyURL string) float64 {
	zenReliMu.RLock()
	defer zenReliMu.RUnlock()
	if r, ok := zenReli[proxyURL]; ok {
		return r
	}
	return 0.5
}

// proxyWeight scores an exit for sampling: faster + more reliable wins.
// live first-byte rtt halves the api-reported latency in the mix, so exits
// that answer fast in practice win more draws while the whole pool stays
// in play. weight = reliability / effective_ms.
func proxyWeight(proxyURL string) float64 {
	ms := float64(proxyLatency(proxyURL))
	if ms <= 0 {
		ms = 10000
	}
	zenLiveMu.RLock()
	live, ok := zenLive[proxyURL]
	zenLiveMu.RUnlock()
	if ok && live > 0 {
		ms = (float64(ms) + live) / 2
	}
	return (proxyReliability(proxyURL) + 0.1) / ms
}

// noteExitRTT folds a first-byte sample (ms) into the exit's live ewma.
func noteExitRTT(proxy string, ms float64) {
	if proxy == "" || ms <= 0 {
		return
	}
	if ms > 60000 {
		ms = 60000
	}
	zenLiveMu.Lock()
	prev, ok := zenLive[proxy]
	if !ok {
		zenLive[proxy] = ms
	} else {
		zenLive[proxy] = prev*0.7 + ms*0.3
	}
	zenLiveMu.Unlock()
}

// sampleWeighted draws up to n distinct proxies, weighted by proxyWeight.
func sampleWeighted(pool []string, n int) []string {
	if len(pool) <= n {
		return pool
	}
	weights := make([]float64, len(pool))
	total := 0.0
	for i, p := range pool {
		weights[i] = proxyWeight(p)
		total += weights[i]
	}
	var out []string
	seen := map[string]bool{}
	for len(out) < n && len(seen) < len(pool) {
		r := rand.Float64() * total
		pick := pool[len(pool)-1]
		for i, w := range weights {
			r -= w
			if r <= 0 {
				pick = pool[i]
				break
			}
		}
		if !seen[pick] {
			seen[pick] = true
			out = append(out, pick)
		}
	}
	return out
}

// probeProxyFull verifies the full path to zen's front door through the
// proxy: socks dials opencode.ai:443, http issues a CONNECT. unused (the
// verifier does GET /v1/models and dead exits die on real sends); kept as
// the documented full-path probe if verification ever needs to go deeper.
func probeProxyFull(proxyURL string, timeout time.Duration) bool {
	if proxyURL == "" {
		return true
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	switch pu.Scheme {
	case "socks4":
		c, err := socks4Dial(ctx, "tcp", "opencode.ai:443", pu.Host)
		if err != nil {
			return false
		}
		c.Close()
		return true
	case "socks5":
		c, err := socks5Dial(ctx, "tcp", "opencode.ai:443", pu)
		if err != nil {
			return false
		}
		c.Close()
		return true
	default:
		return probeHTTPConnect(ctx, pu, "opencode.ai:443")
	}
}

// probeHTTPConnect dials the proxy and issues CONNECT target, true on 200.
func probeHTTPConnect(ctx context.Context, pu *url.URL, target string) bool {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", pu.Host)
	if err != nil {
		return false
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
		defer c.SetDeadline(time.Time{})
	}
	var sb strings.Builder
	sb.WriteString("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n")
	if pu.User != nil {
		pw, _ := pu.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(pu.User.Username() + ":" + pw))
		sb.WriteString("Proxy-Authorization: Basic " + cred + "\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := io.WriteString(c, sb.String()); err != nil {
		return false
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.Contains(line, " 200")
}

// sampleProxies returns up to n distinct pool proxies, weighted toward low
// api-reported latency: the fastest quartile wins ~3/4 of draws. kept for
// tests; the lane picker uses sampleWeighted (live rtt aware).
func sampleProxies(n int) []string {
	zenProxiesMu.RLock()
	pool := append([]string(nil), zenProxies...)
	zenProxiesMu.RUnlock()
	if len(pool) == 0 {
		return nil
	}
	sort.Slice(pool, func(i, j int) bool { return proxyLatency(pool[i]) < proxyLatency(pool[j]) })
	q := len(pool) / 4
	if q < 1 {
		q = 1
	}
	var out []string
	seen := map[string]bool{}
	for len(out) < n && len(seen) < len(pool) {
		u := pool[rand.Intn(len(pool))]
		if rand.Intn(4) < 3 {
			u = pool[rand.Intn(q)]
		}
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// applyVerifiedPool swaps in a fresh verified set. empty verifies keep
// the old pool: a failed sweep must never wipe working exits. lanes keep
// their pins on refresh: an old exit keeps serving until a real send
// proves it dead, so refresh can never clear all lanes at once.
func applyVerifiedPool(verified []string) {
	if len(verified) == 0 {
		log.Printf("  zen proxies: verify found 0, keeping old pool")
		return
	}
	zenProxiesMu.Lock()
	zenProxies = verified
	left := len(verified)
	zenProxiesMu.Unlock()
	// transports, rtt and health for vanished exits die with the pool:
	// all three only ever insert and would grow forever on hourly churn.
	keep := map[string]bool{}
	for _, v := range verified {
		keep[v] = true
	}
	pruneDeadExits(keep)
	log.Printf("  zen proxies: %d verified", left)
}

// dropProxy removes a dead proxy from the pool so it is not picked again.
// the last exit is never dropped and lanes keep the pin until they rotate:
// removing a working exit before its replacement exists empties the pool
// and leaves every lane with no proxy. a dead last exit at least attempts
// until the next refresh refills the pool.
func dropProxy(proxyURL string) {
	if proxyURL == "" {
		return
	}
	zenProxiesMu.Lock()
	found := false
	for i, p := range zenProxies {
		if p == proxyURL {
			if len(zenProxies) <= 1 {
				zenProxiesMu.Unlock()
				log.Printf("  zen proxies: keeping last exit %s (pool would drain)", redactProxyUserinfo(proxyURL))
				return
			}
			zenProxies = append(zenProxies[:i], zenProxies[i+1:]...)
			found = true
			zenPoolDropped++
			log.Printf("  zen proxies: dropped %s (left %d)", redactProxyUserinfo(proxyURL), len(zenProxies))
			break
		}
	}
	zenProxiesMu.Unlock()
	if found {
		closeZenTransport(proxyURL)
		// a dropped exit is proven dead: evacuate every lane pinned to
		// it onto spares, so no lane keeps sending where the pool won't.
		evacuateProxyPins(proxyURL)
	}
}

// evacuateProxyPins moves every lane pinned to a dropped exit onto a spare.
// a lane with no spare keeps the dead pin: "" never sends, so the exit
// still attempts until a replacement exists (same rule as laneReproxy).
func evacuateProxyPins(dead string) {
	for {
		lanesMu.Lock()
		var victim *zenLane
		for _, l := range lanes {
			if l.proxy == dead {
				victim = l
				break
			}
		}
		lanesMu.Unlock()
		if victim == nil {
			return
		}
		// "" model: evacuation outranks per-model burns; the next real
		// send re-rotates if the spare is burned for its model.
		if laneReproxy(victim, "") == dead {
			return // dry pool: keep the pin, stop trying
		}
	}
}

// isTLSFailure reports whether a transport error means the exit is
// intercepting tls with a bad cert (mitm), rather than merely down. those
// exits pass verify intermittently and fail every real send, so they earn
// a ban instead of a plain drop.
func isTLSFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "x509") ||
		strings.Contains(s, "certificate") ||
		strings.Contains(s, "tls:") ||
		strings.Contains(s, "handshake")
}

// tls burns: a mitm exit is out for every model for an hour. a plain drop
// is not enough: the next refresh re-admits it (it passes verify
// intermittently) and other lanes' pins keep using it.
var (
	tlsBurnMu sync.Mutex
	tlsBurned = map[string]time.Time{}
)

// tlsBurnTTL is the mitm ban length. an hour outlasts a refresh cycle and
// still forgives a transient.
func tlsBurnTTL() time.Duration { return time.Hour }

// banTLSExit bans a mitm exit, drops it from the pool, and evacuates every
// lane pinned to it onto spares.
func banTLSExit(proxy string) {
	if proxy == "" {
		return
	}
	tlsBurnMu.Lock()
	tlsBurned[proxy] = time.Now().Add(tlsBurnTTL())
	tlsBurnMu.Unlock()
	acclog.Printf("  zen exit %s tls-banned 1h (mitm cert)", redactProxyUserinfo(proxy))
	dropProxy(proxy)
	evacuateProxyPins(proxy)
}

// exitTLSBanned reports whether an exit is under a mitm ban.
func exitTLSBanned(proxy string) bool {
	if proxy == "" {
		return false
	}
	tlsBurnMu.Lock()
	defer tlsBurnMu.Unlock()
	until, ok := tlsBurned[proxy]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(tlsBurned, proxy)
		return false
	}
	return true
}

// skipTLSBanned filters banned exits out of refresh candidates: a mitm
// exit passes verify intermittently, so the candidate filter is the gate.
func skipTLSBanned(cands []string) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if !exitTLSBanned(c) {
			out = append(out, c)
		}
	}
	return out
}

// maybeRefreshNetErr counts a network failure and fires a pool refresh once
// the threshold is crossed, at most once per cooldown window (same gate as
// the 429 path: without it every lane burst fires a full verify sweep).
// returns true when it fired.
func maybeRefreshNetErr() bool {
	zenNetErrMu.Lock()
	zenNetErrs++
	hit := zenNetErrs >= zenErrorThreshold()
	if hit {
		zenNetErrs = 0
	}
	zenNetErrMu.Unlock()
	if !hit {
		return false
	}
	if !poolRefreshDue() {
		zenProxiesMu.RLock()
		ago := time.Since(zenPoolRefreshed)
		zenProxiesMu.RUnlock()
		dbglog.Printf("  zen: %d net errors, refresh skipped (last %v ago, cooldown %v)", zenErrorThreshold(), ago.Round(time.Second), poolRefreshCooldown())
		return false
	}
	acclog.Printf("  %d+ consecutive network errors, refreshing proxy pool", zenErrorThreshold())
	// stamp at fire time (same as the 429 path): the completion stamp lands
	// after verify, so a trip during an in-flight refresh would double-fire.
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now()
	zenProxiesMu.Unlock()
	refreshZenProxiesAsync()
	return true
}

// resetZenNetErrs clears the net-error run: any http response (even a 429)
// proves the pool dials, only transport failures count.
func resetZenNetErrs() {
	zenNetErrMu.Lock()
	defer zenNetErrMu.Unlock()
	zenNetErrs = 0
}

// zenErrorThreshold is the consecutive-error count that marks the pool
// stale. 0/unset = 3.
func zenErrorThreshold() int {
	if n := cfg().Zen.ErrorThreshold; n > 0 {
		return n
	}
	return 3
}

// zenBackoff sleeps step*retry capped at max: defaults 0, 150ms, 300ms.
func zenBackoff(retry int) {
	zenBackoffCtx(context.Background(), retry)
}

// zenBackoffCtx is zenBackoff abortable on client disconnect: no point
// sleeping between retries when nobody waits for the answer.
func zenBackoffCtx(ctx context.Context, retry int) {
	step := 150 * time.Millisecond
	if ms := cfg().Zen.RetryBackoffMs; ms > 0 {
		step = time.Duration(ms) * time.Millisecond
	}
	max := 300 * time.Millisecond
	if ms := cfg().Zen.RetryBackoffMaxMs; ms > 0 {
		max = time.Duration(ms) * time.Millisecond
	}
	d := time.Duration(retry) * step
	if d > max {
		d = max
	}
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
}

// hedgeDelay resolves the hedge wait for a send. when the lane race is on
// and the pool has been idle, both lanes fire at once (race_hedge_after_ms,
// 0 by default) and the first byte wins: costs a second inference, but the
// pool has the headroom. otherwise the configured hedge delay applies.
// hedge_after_ms -1 disables hedging, 0 means the 2s default.
func hedgeDelay() time.Duration {
	if share, threshold := lanesIdleEnough(); share < threshold {
		ms := cfg().Zen.RaceHedgeAfterMs
		if ms < 0 {
			ms = 0
		}
		acclog.Printf("  opencode lane race on (busy share %.0f%% < %.0f%%)", share*100, threshold*100)
		return time.Duration(ms) * time.Millisecond
	}
	ms := cfg().Zen.HedgeAfterMs
	if ms < 0 {
		return 0 // -1 disables hedging entirely
	}
	if ms == 0 {
		return 2 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

// zenGeoBlocked reports whether an upstream error body means the model is
// geo-restricted for the exit IP. Switching proxies fixes the request.
func zenGeoBlocked(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "not available in your country")
}

// zenServiceOverloaded reports whether zen returned a transient overload
// error. Those are retried on the same proxy and session, not rotated.
func zenServiceOverloaded(body []byte) bool {
	return strings.Contains(string(body), "[service_overloaded]")
}

// noteZenGeoErr counts consecutive geo-blocks and returns true once the
// threshold is crossed, signaling most of the pool sits in a blocked region.
func noteZenGeoErr() bool {
	zenGeoMu.Lock()
	defer zenGeoMu.Unlock()
	zenGeoErrs++
	return zenGeoErrs >= zenErrorThreshold()
}

func resetZenGeoErrs() {
	zenGeoMu.Lock()
	defer zenGeoMu.Unlock()
	zenGeoErrs = 0
}

// zenUserBlocked reports whether an upstream error body means the exit IP is
// banned from zen (403 [user_blocked]). Switching to another proxy fixes it.
func zenUserBlocked(body []byte) bool {
	return strings.Contains(string(body), "[user_blocked]")
}

// noteZenBlockErr counts consecutive user-blocks and returns true once the
// threshold is crossed, signaling most of the pool is banned.
func noteZenBlockErr() bool {
	zenBlockMu.Lock()
	defer zenBlockMu.Unlock()
	zenBlockErrs++
	return zenBlockErrs >= zenErrorThreshold()
}

func resetZenBlockErrs() {
	zenBlockMu.Lock()
	defer zenBlockMu.Unlock()
	zenBlockErrs = 0
}

var (
	zenFreeTierMu   sync.Mutex
	zenFreeTierErrs int
	zenRefreshMu    sync.Mutex
)

// zenFreeTierBlocked reports whether an upstream error body refuses the exit
// IP the anonymous free tier (403 FreeTierError). the flag is per-IP: the
// same request succeeds from another exit, so rotate proxies instead of
// failing the client request.
func zenFreeTierBlocked(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "FreeTierError") ||
		strings.Contains(strings.ToLower(s), "free tier can only be used from within")
}

// noteZenFreeTierErr counts consecutive free-tier blocks and returns true
// once the threshold is crossed, signaling most of the pool is flagged.
func noteZenFreeTierErr() bool {
	zenFreeTierMu.Lock()
	defer zenFreeTierMu.Unlock()
	zenFreeTierErrs++
	return zenFreeTierErrs >= zenErrorThreshold()
}

func resetZenFreeTierErrs() {
	zenFreeTierMu.Lock()
	defer zenFreeTierMu.Unlock()
	zenFreeTierErrs = 0
}

// refreshZenProxiesAsync triggers a pool refresh without blocking the request
// path. concurrent triggers collapse into one in-flight refresh.
func refreshZenProxiesAsync() {
	go func() {
		if !zenRefreshMu.TryLock() {
			return
		}
		defer zenRefreshMu.Unlock()
		refreshZenProxies()
	}()
}

// setProxyCountries replaces the proxy->country map after a refresh.
func setProxyCountries(m map[string]string) {
	zenProxyCountryMu.Lock()
	defer zenProxyCountryMu.Unlock()
	zenProxyCountry = m
}

// proxyCountry returns the api-reported country for a proxy.
func proxyCountry(proxyURL string) string {
	if proxyURL == "" {
		return "direct"
	}
	zenProxyCountryMu.RLock()
	defer zenProxyCountryMu.RUnlock()
	if c := zenProxyCountry[proxyURL]; c != "" {
		return c
	}
	return "??"
}

// noteZenCountryBlock counts a geo/user block for a country, returns total.
func noteZenCountryBlock(country string) int {
	zenCountryBlockMu.Lock()
	defer zenCountryBlockMu.Unlock()
	zenCountryBlocks[country]++
	return zenCountryBlocks[country]
}

// poolExit is one verified exit for /status. AvgMs is the live
// first-byte ewma (0 = no real sends yet); LatencyMs stays the
// api-reported verification figure for the pool picker.
type poolExit struct {
	Proxy   string `json:"proxy"`
	Country string `json:"country"`
	Latency int    `json:"latency_ms"`
	AvgMs   int    `json:"avg_ms,omitempty"`
	Pinned  bool   `json:"pinned"`
}

// poolSummary is the /status view of the proxy pool.
type poolSummary struct {
	Verified int        `json:"verified"`
	Cands    int        `json:"candidates"`
	Dropped  int        `json:"dropped_total"`
	Refresh  string     `json:"last_refresh_ago"`
	Exits    []poolExit `json:"exits,omitempty"`
}

// poolSnapshot builds the pool section: counts, refresh age, per-exit
// rows with pinned-lane marking. credentials stay redacted like lanes.
func poolSnapshot() poolSummary {
	zenProxiesMu.RLock()
	pool := append([]string(nil), zenProxies...)
	refreshed := zenPoolRefreshed
	cands := zenPoolCands
	dropped := zenPoolDropped
	zenProxiesMu.RUnlock()
	pinned := map[string]bool{}
	lanesMu.Lock()
	for _, l := range lanes {
		if l.proxy != "" {
			pinned[l.proxy] = true
		}
	}
	lanesMu.Unlock()
	out := poolSummary{Dropped: dropped, Cands: cands}
	out.Verified = len(pool)
	if !refreshed.IsZero() {
		out.Refresh = time.Since(refreshed).Round(time.Second).String()
	}
	for _, p := range pool {
		out.Exits = append(out.Exits, poolExit{
			Proxy: redactProxyUserinfo(p), Country: proxyCountry(p),
			Latency: proxyLatency(p), AvgMs: proxyLiveMs(p), Pinned: pinned[p],
		})
	}
	return out
}

// proxyLiveMs returns the live first-byte ewma for an exit, 0 when no
// real send has sampled it yet.
func proxyLiveMs(proxyURL string) int {
	zenLiveMu.RLock()
	defer zenLiveMu.RUnlock()
	if ms := zenLive[proxyURL]; ms > 0 {
		return int(ms)
	}
	return 0
}

// logZenNetErr logs a transport-level zen failure with model and country.
// proxy userinfo is redacted: the access log is world-readable.
func logZenNetErr(retry int, model, proxy, tag string, err error) {
	acclog.Printf("!! opencode zen error (retry %d/%d) model=%s country=%s proxy=%s %s: %v",
		retry, zenRetryLabel(), model, proxyCountry(proxy), redactProxyUserinfo(proxy), tag, err)
}

// exhausted429 synthesizes the retryable-exhausted response: JSON 503 +
// Retry-After instead of the stale empty 429. the 429 branch closes the
// upstream body before continuing, so without this the post-loop forward
// re-reads a closed body and hands the client a bodyless 429 it gives up
// on. 503 tells opencode to retry later.
func exhausted429(model string, status int) *http.Response {
	eb := []byte(`{"error":{"message":"opencode zen: all retries rate-limited (` + strconv.Itoa(status) + `), try again","type":"rate_limit_error"}}`)
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", "30")
	acclog.Printf("!! opencode retryable-exhausted model=%s last=%d, synthesizing 503", model, status)
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(bytes.NewReader(eb)), Header: h}
}

// logZenBlocked logs a geo/user block with status, snippet, and the running
// per-country block count. returns the count. proxy redacted like above.
func logZenBlocked(kind string, retry, status int, model, proxy, tag string, body []byte) int {
	n := noteZenCountryBlock(proxyCountry(proxy))
	acclog.Printf("  opencode %s-blocked %d (retry %d/%d) model=%s country=%s blocks=%d via proxy=%s %s err=%q",
		kind, status, retry, zenRetryLabel(), model, proxyCountry(proxy), n, redactProxyUserinfo(proxy), tag, errSnippet(body, 160))
	return n
}

// errSnippet squashes a body to one line, capped at n chars.
func errSnippet(b []byte, n int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func watchZenProxies() {
	if zenEnabled() {
		// cached pool serves from boot while the fresh sweep verifies in the
		// background: sub-second first requests instead of a ~30s cold pool.
		// pre-open lanes + transports so the first send has zero setup.
		if loadZenPool() {
			zenProxiesMu.RLock()
			cached := append([]string(nil), zenProxies...)
			zenProxiesMu.RUnlock()
			ensureSharedLanes()
			warmZenTransports(cached)
			log.Printf("  zen proxies: %d from cache (%s)", len(cached), poolCachePath())
		}
		refreshZenProxies()
	}
	for {
		interval := time.Hour
		if m := cfg().Zen.RefreshIntervalMinutes; m > 0 {
			interval = time.Duration(m) * time.Minute
		}
		time.Sleep(interval)
		if zenEnabled() {
			refreshZenProxies()
		}
		sweepLanes()
	}
}

// zenTransports caches one transport per proxy. building a fresh transport
// per client leaked its idle connections (and fds) until gc on every request;
// transports are safe for concurrent use, so sharing per-proxy is correct.
var (
	zenTransportMu sync.Mutex
	zenTransports  = map[string]*http.Transport{}
)

// zenTransport returns the cached transport for proxyURL, building it on
// first use. pu must be a valid parse of proxyURL.
func zenTransport(proxyURL string, pu *url.URL) *http.Transport {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	if tr, ok := zenTransports[proxyURL]; ok {
		return tr
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.ExpectContinueTimeout = 1 * time.Second
	tr.MaxIdleConnsPerHost = 4
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	switch pu.Scheme {
	case "socks4":
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			return socks4Dial(c, network, addr, pu.Host)
		}
	case "socks5":
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			return socks5Dial(c, network, addr, pu)
		}
	default:
		tr.Proxy = http.ProxyURL(pu)
		tr.DialContext = dialer.DialContext
	}
	zenTransports[proxyURL] = tr
	return tr
}

// warmZenTransports pre-builds cached transports for verified exits so
// the first real send skips transport setup. cheap: no connections open
// until first use. capped at the fastest 25 to bound fd churn.
func warmZenTransports(verified []string) {
	pool := append([]string(nil), verified...)
	sort.Slice(pool, func(i, j int) bool { return proxyWeight(pool[i]) > proxyWeight(pool[j]) })
	if len(pool) > 25 {
		pool = pool[:25]
	}
	for _, p := range pool {
		if pu, err := url.Parse(p); err == nil {
			zenTransport(p, pu)
		}
	}
}

// closeZenTransport drops a proxy's cached transport and its idle conns.
func closeZenTransport(proxyURL string) {
	zenTransportMu.Lock()
	if tr, ok := zenTransports[proxyURL]; ok {
		delete(zenTransports, proxyURL)
		tr.CloseIdleConnections()
	}
	zenTransportMu.Unlock()
}

// pruneDeadExits drops transports, live-rtt samples and health entries for
// exits no longer in the verified set. all three maps only insert;
// without this the hourly refresh grows them forever.
func pruneDeadExits(keep map[string]bool) {
	zenTransportMu.Lock()
	for u, tr := range zenTransports {
		if !keep[u] {
			delete(zenTransports, u)
			tr.CloseIdleConnections()
		}
	}
	zenTransportMu.Unlock()
	zenLiveMu.Lock()
	for u := range zenLive {
		if !keep[u] {
			delete(zenLive, u)
		}
	}
	zenLiveMu.Unlock()
	exitTrackMu.Lock()
	for u := range exitTrack {
		if !keep[u] {
			delete(exitTrack, u)
		}
	}
	exitTrackMu.Unlock()
}

// zenClient builds a client routing via proxyURL, or direct if ""/bad.
func zenClient(proxyURL string) *http.Client {
	direct := 300 * time.Second
	if s := cfg().Zen.ZenDirectTimeoutSecs; s > 0 {
		direct = time.Duration(s) * time.Second
	}
	if proxyURL == "" {
		return dbgClient(&http.Client{Timeout: direct}, "opencode")
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return dbgClient(&http.Client{Timeout: direct}, "opencode")
	}
	proxied := 120 * time.Second
	if s := cfg().Zen.ZenTimeoutSecs; s > 0 {
		proxied = time.Duration(s) * time.Second
	}
	// dbgRT wraps whatever transport the pool gave us: the proxy/socks
	// handshake stays exactly as it was, only the recording sits on top.
	return dbgClient(&http.Client{Timeout: proxied, Transport: dbgRT{base: zenTransport(proxyURL, pu), backend: "opencode"}}, "opencode")
}

// socks4Dial connects to proxyAddr and requests a SOCKS4/SOCKS4a CONNECT to addr.
func socks4Dial(ctx context.Context, network, addr string, proxyAddr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("socks4: bad port %q", portStr)
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
		defer conn.SetDeadline(time.Time{})
	}

	req := []byte{0x04, 0x01, byte(port >> 8), byte(port & 0xff)}
	if ip4 := net.ParseIP(host).To4(); ip4 != nil {
		req = append(req, ip4...)
		req = append(req, 0x00)
	} else {
		req = append(req, 0x00, 0x00, 0x00, 0x01)
		req = append(req, 0x00)
		req = append(req, host...)
		req = append(req, 0x00)
	}
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	resp := make([]byte, 8)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, err
	}
	if resp[0] != 0x00 || resp[1] != 0x5a {
		conn.Close()
		return nil, fmt.Errorf("socks4: connect failed, status=%d", resp[1])
	}
	return conn, nil
}

// socks5Dial connects via the proxy and requests a CONNECT to addr.
// credentialed urls get RFC 1929 user/pass auth; bare urls stay no-auth.
func socks5Dial(ctx context.Context, network, addr string, pu *url.URL) (net.Conn, error) {
	proxyAddr := pu.Host
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("socks5: bad port %q", portStr)
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
		defer conn.SetDeadline(time.Time{})
	}

	var user, pass string
	if pu.User != nil {
		user = pu.User.Username()
		pass, _ = pu.User.Password()
	}
	hello := []byte{0x05, 0x01, 0x00}
	if user != "" {
		hello = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if _, err := conn.Write(hello); err != nil {
		conn.Close()
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, err
	}
	if resp[0] != 0x05 || resp[1] == 0xff {
		conn.Close()
		return nil, fmt.Errorf("socks5: no acceptable auth method")
	}
	// RFC 1929 username/password auth for credentialed proxy urls.
	if resp[1] == 0x02 {
		if user == "" {
			conn.Close()
			return nil, fmt.Errorf("socks5: proxy wants auth, url has no credentials")
		}
		var b bytes.Buffer
		b.WriteByte(0x01)
		b.WriteByte(byte(len(user)))
		b.WriteString(user)
		b.WriteByte(byte(len(pass)))
		b.WriteString(pass)
		if _, err := conn.Write(b.Bytes()); err != nil {
			conn.Close()
			return nil, err
		}
		st := make([]byte, 2)
		if _, err := io.ReadFull(conn, st); err != nil {
			conn.Close()
			return nil, err
		}
		if st[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("socks5: auth failed")
		}
	} else if user != "" {
		// credentials in url but proxy chose no-auth: proceed, the url
		// may be shared across proxies with mixed auth.
	}

	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		conn.Close()
		return nil, err
	}
	if hdr[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5: connect failed, rep=%d", hdr[1])
	}
	var skip int
	switch hdr[3] {
	case 0x01:
		skip = 4 + 2
	case 0x03:
		l := []byte{0}
		if _, err := io.ReadFull(conn, l); err != nil {
			conn.Close()
			return nil, err
		}
		skip = int(l[0]) + 2
	case 0x04:
		skip = 16 + 2
	default:
		conn.Close()
		return nil, fmt.Errorf("socks5: bad atyp %d", hdr[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, skip)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
