package main

import (
	"bytes"
	"context"
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
	"sync/atomic"
	"time"
)

// proxyListURL is the third-party exit list. a var, not a const, so the test
// suite can point it at a dead local port: a stray refresh trip during a test
// used to fetch the real list and fill the pool with live internet exits.
//
// the list's own response-time figure is unreliable (a "5ms" entry is as
// likely to be dead as a "1500ms" one), so the fetch asks for everything and
// lets verification decide. capping the list at 400ms was what starved the
// pool: only ~160 https-capable entries survived, and about one in ten of
// those dials, leaving fewer exits than lanes.
//
// response_max mirrors zen.max_response_ms: it is the site's own filter, so
// leaving it below the config value truncates the list before the client
// filter ever sees it. keep the two equal.
var proxyListURL = "https://proxies.minoa.cat/list?format=json&sort=response&limit=0&response_max=30000"

// proxyListFallback is the json the site proxies, fetched straight from the
// repo. the site is the filtered view of it; on a line where the site is
// unreachable (comcast advanced security serves a block page for the domain)
// the sweep would otherwise find no exits at all. same schema, and the
// client-side filters below do what the site's query parameters do.
var proxyListFallback = "https://raw.githubusercontent.com/M1noa/proxypool/refs/heads/output/proxies.json"

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
// to 1.19.3 and is refreshed from GitHub on startup and every 6h after.
// guarded by a mutex: the refresher goroutine writes it while request
// goroutines read it for every zen request.
var (
	zenVersionMu sync.RWMutex
	zenVersion   = "1.19.3"
)

func zenVersionStr() string {
	zenVersionMu.RLock()
	defer zenVersionMu.RUnlock()
	return zenVersion
}

// zenUserAgent is the whole User-Agent the client sends: nothing else is
// appended. v1.19.3 has `const USER_AGENT = opencode/${InstallationVersion}`
// in session/llm/request.ts; there is no ai-sdk or runtime suffix.
func zenUserAgent() string {
	return "opencode/" + zenVersionStr()
}

// fetchOpenCodeVersion pulls the latest opencode release tag from the GitHub
// API, falling back to the default on any error.
func fetchOpenCodeVersion() {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get("https://api.github.com/repos/anomalyco/opencode/releases/latest")
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

// poolTarget is how many verified exits a sweep aims to hold: eight per
// lane. an exit IP serves ~50-100 free-tier requests before zen caps it, so
// a lane burns through its bench in a couple of minutes of agent traffic; a
// pool sized at three per lane parked itself after a handful of requests and
// had nothing to rotate onto. never more than the configured pool_size,
// never less than 24.
func poolTarget() int {
	t := 8 * laneCap()
	if t < 24 {
		t = 24
	}
	if n := cfg().Zen.PoolSize; n > 0 && t > n {
		t = n
	}
	return t
}

// fetchProxyList pulls the exit list, falling back to the upstream json when
// the site itself is unreachable. both answer the same schema; the fallback
// returns the unfiltered list, which costs a pass over a few thousand more
// entries and yields the same pool once the filters and verify sweep run.
func fetchProxyList(cl *http.Client) ([]proxyEntry, error) {
	var firstErr error
	for _, u := range []string{proxyListURL, proxyListFallback} {
		resp, err := cl.Get(u)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// a block page or an error page is not a list: try the other
		// source instead of decoding html into an empty result.
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: upstream %d", u, resp.StatusCode)
			}
			continue
		}
		// cap the third-party body: a compromised list server must not feed
		// unbounded allocation into the hourly refresh.
		var list []proxyEntry
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&list)
		resp.Body.Close()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// an empty answer is a filtered-out list, not a pool: the fallback
		// source is the same json unfiltered, so it still has candidates.
		if len(list) == 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: empty list", u)
			}
			continue
		}
		if u == proxyListFallback {
			log.Printf("  zen proxies: list site unreachable, using fallback source")
		}
		return list, nil
	}
	return nil, firstErr
}

// refreshZenProxies sweeps the list and swaps in the verified set. fresh
// skips re-verifying the exits already in the pool: the dry-pool path uses
// it to go looking for exits other than the ones that just rate-limited.
// full re-checks every exit, marks and live pool members included: it is the
// out-of-exits recovery, where the marks are what a sweep would otherwise skip.
func refreshZenProxies(fresh bool) {
	full := zenRefreshFull.Swap(false)
	cl := &http.Client{Timeout: 15 * time.Second}
	list, err := fetchProxyList(cl)
	if err != nil {
		// keep the full intent: the next sweep still has to re-probe the
		// whole list, and a failed fetch must not downgrade it to a fresh one.
		if full {
			zenRefreshFull.Store(true)
		}
		log.Printf("  zen proxies: fetch failed: %v", err)
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
	// dial order by claimed latency. the list's reliability figure looks
	// tempting but is not a filter: measured against the top-200 by
	// reliability, not one candidate passed the probe, and 15% were already
	// free-tier capped (a high-reliability exit is a popular one, and
	// popular means burned). yield is about one in two hundred either way,
	// so what raises the pool is dialing more of the list, not reordering
	// it.
	sort.Slice(fast, func(i, j int) bool { return fast[i].ResponseTimeMs < fast[j].ResponseTimeMs })
	if n := c.Zen.PoolSize; n > 0 && len(fast) > n {
		fast = fast[:n]
	}
	cands := make([]string, 0, len(fast))
	countries := make(map[string]string, len(fast))
	lats := make(map[string]int, len(fast))
	relis := make(map[string]float64, len(fast))
	seenCand := map[string]bool{}
	// rate-limited and IP-gated exits stay out of a fresh sweep's candidate
	// list: the sweep is looking for clean replacements, not re-verifying the
	// exits that just failed. a full sweep clears only rate-limit and dead
	// marks, never IP gate parks (an IP that hit its daily cap is still capped).
	for _, e := range fast {
		u := schemeFor(e.Protocols) + "://" + e.IP + ":" + strconv.Itoa(e.Port)
		if exitGateCooling(u) || (!full && exitDead(u)) {
			continue
		}
		if seenCand[u] {
			continue // list servers repeat entries: never check the same exit twice
		}
		seenCand[u] = true
		cands = append(cands, u)
		countries[u] = strings.ToUpper(strings.TrimSpace(e.Country))
		lats[u] = e.ResponseTimeMs
		relis[u] = e.Reliability
	}
	// order the sweep: a periodic refresh re-checks the exits it is already
	// serving first, so the pool it replaces keeps its known-good members and
	// the early stop below spends most of the budget there. a fresh sweep
	// (dry pool) skips re-verifying the current set and merges any surviving
	// existing members into the result so a sweep never wipes working pins.
	zenProxiesMu.RLock()
	existing := append([]string(nil), zenProxies...)
	zenProxiesMu.RUnlock()
	existingSet := make(map[string]bool, len(existing))
	for _, u := range existing {
		existingSet[u] = true
	}
	if full || !fresh {
		if len(existing) > 0 {
			seen := make(map[string]bool, len(cands)+len(existing))
			order := make([]string, 0, len(cands)+len(existing))
			for _, u := range existing {
				if seen[u] {
					continue
				}
				seen[u] = true
				order = append(order, u)
			}
			for _, u := range cands {
				if seen[u] {
					continue
				}
				seen[u] = true
				order = append(order, u)
			}
			cands = order
		}
	} else if len(existingSet) > 0 {
		filtered := make([]string, 0, len(cands))
		for _, u := range cands {
			if !existingSet[u] {
				filtered = append(filtered, u)
			}
		}
		cands = filtered
	}
	// target is the pool the sweep aims for, budget the candidate ceiling:
	// one exit per lane plus a rotation set, and a bounded number of dials so
	// a list with thousands of entries cannot turn one refresh into minutes.
	// the early stop below halts the sweep the moment the target is met.
	target := poolTarget()
	budget := target * 16
	if c.Zen.PoolSize <= 0 {
		budget = len(cands) // pool_size 0 = no cap: dial the whole filtered list
	}
	if budget > len(cands) {
		budget = len(cands)
	}
	cands = cands[:budget]

	var wg sync.WaitGroup
	var mu sync.Mutex
	verified := make([]string, 0, len(cands))
	verifyConc := 40
	if n := c.Zen.VerifyConcurrency; n > 0 {
		verifyConc = n
	}
	// cold or dry sweeps verify wide: lanes may be waiting for an exit, so
	// fan out past the hourly-refresh cap. warm refreshes keep the cap.
	zenProxiesMu.RLock()
	coldSweep := len(zenProxies) == 0
	zenProxiesMu.RUnlock()
	if (coldSweep || fresh || full || c.Zen.PoolSize <= 0) && verifyConc < 200 {
		verifyConc = 200 // an uncapped pool dials the whole list: fan out to match
	}
	// incremental publish: when the live pool is empty or a fresh/full refill
	// is underway for waiting lanes, publish new exits as soon as they verify
	// so laneProxyWait wakes in seconds instead of waiting for all 3000 dials.
	cold := coldSweep || fresh || full
	lastPub := time.Time{}
	var warmOnce sync.Once
	mergeWithLive := func(snap []string) []string {
		if !fresh || full {
			return snap
		}
		zenProxiesMu.RLock()
		cur := append([]string(nil), zenProxies...)
		zenProxiesMu.RUnlock()
		seen := make(map[string]bool, len(cur)+len(snap))
		out := make([]string, 0, len(cur)+len(snap))
		for _, u := range cur {
			if exitDead(u) || exitGateCooling(u) || seen[u] {
				continue
			}
			seen[u] = true
			out = append(out, u)
		}
		for _, u := range snap {
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
		return out
	}
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
		merged := mergeWithLive(snap)
		kc, kl, kr := map[string]string{}, map[string]int{}, map[string]float64{}
		for _, v := range merged {
			if co, ok := countries[v]; ok {
				kc[v] = co
			} else if co := proxyCountry(v); co != "" && co != "??" {
				kc[v] = co
			}
			if l, ok := lats[v]; ok {
				kl[v] = l
			} else {
				kl[v] = proxyLatency(v)
			}
			if r, ok := relis[v]; ok {
				kr[v] = r
			} else {
				kr[v] = proxyReliability(v)
			}
		}
		setProxyCountries(kc)
		setProxyLatency(kl)
		setProxyReliability(kr)
		zenProxiesMu.Lock()
		zenProxies = merged
		zenPoolRefreshed = time.Now()
		zenPoolCands = len(cands)
		zenProxiesMu.Unlock()
		// lanes + transports on the first usable exits, so the earliest
		// request skips lane creation and tls setup. sync.Once: every
		// verify goroutine calls publish concurrently.
		warmOnce.Do(func() {
			ensureSharedLanes()
			warmZenTransports(merged)
			log.Printf("  zen proxies: %d verified (pool warming, %d candidates left)", len(merged), len(cands)-len(snap))
		})
	}
	// stop closes once the target is verified. every worker re-checks it
	// after taking a verify slot, so the sweep ends within one wave of the
	// target being met instead of dialing the rest of the list.
	//
	// exits parked by the free-tier gate do not count: a 200 on GET /models
	// proves connectivity, not that the IP may serve a completion, and a pool
	// filled with gated exits would otherwise satisfy the target and stop the
	// sweep before it ever reaches a fresh candidate.
	stop := make(chan struct{})
	var stopOnce sync.Once
	hitTarget := func() {
		mu.Lock()
		n := 0
		for _, u := range verified {
			if !exitGateCooling(u) {
				n++
			}
		}
		mu.Unlock()
		if n >= target {
			stopOnce.Do(func() { close(stop) })
		}
	}
	var mix verifyMix
	sem := make(chan struct{}, verifyConc)
	for _, u := range cands {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			select {
			case <-stop:
				return
			default:
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			select {
			case <-stop:
				return // the wave ahead of us filled the pool
			default:
			}
			r := verifyProxy(u)
			mu.Lock()
			mix.add(r)
			ok := r == verifyOK || r == verifyCapped
			if ok {
				verified = append(verified, u)
			}
			mu.Unlock()
			if ok {
				publish()
				hitTarget()
			}
		}(u)
	}
	wg.Wait()

	// metadata commits only with a real pool: a failed sweep must not
	// clobber the live country/latency/reliability maps for the old exits.
	// on a fresh sweep, merge surviving existing pool members so we never
	// replace 20 working pins with just the 6 new exits found in the rest of the list.
	finalPool := mergeWithLive(verified)
	if len(finalPool) > 0 {
		kc, kl, kr := map[string]string{}, map[string]int{}, map[string]float64{}
		for _, v := range finalPool {
			if c, ok := countries[v]; ok {
				kc[v] = c
			} else if c := proxyCountry(v); c != "" && c != "??" {
				kc[v] = c
			}
			if l, ok := lats[v]; ok {
				kl[v] = l
			} else {
				kl[v] = proxyLatency(v)
			}
			if r, ok := relis[v]; ok {
				kr[v] = r
			} else {
				kr[v] = proxyReliability(v)
			}
		}
		setProxyCountries(kc)
		setProxyLatency(kl)
		setProxyReliability(kr)
		saveZenPool(finalPool, kc, kl, kr)
	}
	applyVerifiedPool(finalPool)
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now()
	zenPoolCands = len(cands)
	zenProxiesMu.Unlock()
	// pre-open the shared lanes now that exits exist: cold starts spread
	// from the first request instead of serializing on lane creation.
	// also pre-build transports so the first send skips tls/dial setup.
	ensureSharedLanes()
	warmZenTransports(finalPool)
	// acclog, not log: stderr lands in nimroute.err.log and nothing reads it.
	// the mix rides the summary so an upstream that starts answering 429/403
	// to everything shows up next to the pool size with a reason attached.
	acclog.Printf("  zen proxies: %d verified (%d total pool) of %d candidates dialed (target %d) mix: cap=%d 429=%d 403=%d other=%d dialerr=%d", len(verified), len(finalPool), len(cands), target, mix.capped, mix.rateLimited, mix.freeTier, mix.other, mix.dialErr)
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

// verifyResult classifies one verification probe so a sweep can tally why
// exits fall out. it never decides the outcome: only verifyOK verifies, and
// every other answer is the same rejection it always was.
type verifyResult int

const (
	verifyOK          verifyResult = iota // 200 and a model-list body
	verifyCapped                          // 429 free-usage answer: reachable, rotation's problem
	verifyRateLimited                     // 429 aimed at this exit
	verifyFreeTier                        // 403 FreeTierError on GET /models
	verifyOther                           // any other rejection
	verifyDialErr                         // no response came back at all
)

// verifyMix is the per-sweep tally of why candidates fell out, logged with
// the sweep summary. statuses and counts only: never a proxy url, so no
// credentials can reach the log.
type verifyMix struct {
	capped      int
	rateLimited int
	freeTier    int
	other       int
	dialErr     int
}

func (m *verifyMix) add(r verifyResult) {
	switch r {
	case verifyOK:
	case verifyCapped:
		m.capped++
	case verifyRateLimited:
		m.rateLimited++
	case verifyFreeTier:
		m.freeTier++
	case verifyDialErr:
		m.dialErr++
	default: // verifyOther
		m.other++
	}
}

// verifyProbeBody is the zero-cost POST payload that checks whether an exit
// IP passes zen's free-tier IP/reputation gate without invoking upstream
// inference or consuming any of the IP's ~50-100 daily requests. zen runs the
// IP/abuse gate before message role validation: an IP-blocked exit answers 403
// FreeTierError, while a clean exit passes the gate and fails fast with 400
// invalid_request_error on the invalid role. includes the full 11-tool
// opencode 1.19.3 CLI toolset (zenOCDefsJSON) so tool-fingerprint checks pass.
var verifyProbeBody = []byte(`{"model":"big-pickle","stream":true,"messages":[{"role":"invalid_role","content":"hi"}],"tools":` + zenOCDefsJSON + `}`)

// verifyProxy confirms a proxy can reach the zen API AND passes the per-IP
// free-tier gate via a zero-cost validation probe (verifyProbeBody). GET
// /models does not run the IP reputation gate at all, so verifying with GET
// admitted flagged datacenter proxies that 403'd every real completion.
func verifyProxy(proxyURL string) verifyResult {
	cl := zenClient(proxyURL)
	// 10s, not the ~2s most exits take: a slow exit that still clears the gate
	// is worth keeping, and cutting the probe short only shrinks the pool.
	cl.Timeout = 10 * time.Second
	req, err := http.NewRequest("POST", OpencodeBase+"/chat/completions", bytes.NewReader(verifyProbeBody))
	if err != nil {
		return verifyDialErr
	}
	setZenHeaders(req, zenSession())
	resp, err := cl.Do(req)
	if err != nil {
		return verifyDialErr
	}
	defer resp.Body.Close()
	// any parsed answer means the request cleared the free-tier gate: the
	// probe body is invalid on purpose, so 400 (role rejected) is a pass.
	switch resp.StatusCode {
	case http.StatusOK, http.StatusBadRequest:
		return verifyOK
	case http.StatusTooManyRequests:
		// a 429 answers after the gate: the exit dialed and zen parsed the
		// body, it just will not serve this request. rotation handles that
		// (noteExit429 marks the pair for a cooldown), so excluding the exit
		// here is what emptied the pool and left lanes pinless while the
		// exits were all reachable.
		if b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10)); zenFreeUsageCap(b) {
			return verifyCapped
		}
		return verifyRateLimited
	case http.StatusForbidden:
		return verifyFreeTier
	default:
		return verifyOther
	}
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

// applyVerifiedPool swaps in a fresh verified set. empty verifies keep
// the old pool: a failed sweep must never wipe working exits. lanes keep
// their pins while the exit is still usable, so refresh can never clear
// every lane at once; a lane whose exit is gone or marked is re-pinned to
// a spare instead of falling back to direct.
//
// a completed sweep is a cycle boundary: the 429 marks from the last cycle
// are dropped here, together with their per-cycle counters, so an exit that
// was merely busy this cycle is usable again. dead marks survive until
// their ttl (a dead exit must verify again before it is used).
func applyVerifiedPool(verified []string) {
	if len(verified) == 0 {
		log.Printf("  zen proxies: verify found 0, keeping old pool")
		clearExitRateLimits()
		ensureLanePins()
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
	clearExitRateLimits()
	ensureLanePins()
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
	left := 0
	for i, p := range zenProxies {
		if p == proxyURL {
			if len(zenProxies) <= 1 {
				zenProxiesMu.Unlock()
				log.Printf("  zen proxies: keeping last exit %s (pool would drain)", redactProxyUserinfo(proxyURL))
				return
			}
			zenProxies = append(zenProxies[:i], zenProxies[i+1:]...)
			found = true
			left = len(zenProxies)
			zenPoolDropped++
			break
		}
	}
	zenProxiesMu.Unlock()
	if found {
		// logged outside the pool lock: an io writer must not stall every
		// picker and snapshot behind it.
		log.Printf("  zen proxies: dropped %s (left %d)", redactProxyUserinfo(proxyURL), left)
		closeZenTransport(proxyURL)
		// a dropped exit is proven dead: evacuate every lane pinned to
		// it onto spares, so no lane keeps sending where the pool won't.
		evacuateProxyPins(proxyURL)
	}
}

// evacuateProxyPins moves every lane pinned to a dropped exit onto a spare.
// a lane with no unique spare ends up with no pin (laneReproxy clears it):
// two lanes never share an exit, and a pin on an exit the pool dropped is
// worse than "" (direct, or a handoff to a lane that owns one).
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
			break
		}
		// "" model: evacuation outranks per-model burns; the next real
		// send re-rotates if the spare is burned for its model. the dead
		// exit is out of the pool, so the pick can never hand it back.
		if laneReproxy(victim, "") == dead {
			break // empty pool: no unique spare to move to, stop trying
		}
	}
	// a lane left without an exit cannot send: the table goes back down to
	// the pool instead of leaving it pinless.
	trimLanesToPool()
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

// zenDialDead reports whether a transport error happened while dialing
// (to the exit itself, or upstream when direct): connection
// refused/unreachable/no route/dial timeout. a re-dial of the very same
// exit inside one request cannot succeed, so the retry ladders stop
// instead of walking every remaining hop over a corpse when rotation has
// nothing new to hand out. response-stage faults (read/write frame,
// http2 header wait) can recover on a re-send and are deliberately not
// dial-dead.
func zenDialDead(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "read tcp") || strings.Contains(s, "write tcp") ||
		strings.Contains(s, "http2:") {
		return false
	}
	// socks dialers report their own text: a refused CONNECT or a failed
	// handshake is the same corpse a "dial tcp" error names for http exits.
	return strings.Contains(s, "dial tcp") || strings.Contains(s, "proxyconnect") ||
		strings.Contains(s, "socks4:") || strings.Contains(s, "socks5:")
}

// banTLSExit marks a mitm exit dead, drops it from the pool, and evacuates
// every lane pinned to it onto spares. a plain drop is not enough: the next
// refresh re-admits it (it passes verify intermittently) and other lanes'
// pins keep using it. the dead mark expires with deadTTL, and the exit only
// comes back if a later sweep verifies it again.
func banTLSExit(proxy string) {
	markExitDead(proxy, "mitm cert")
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
	refreshZenProxiesAsync(false)
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
// hedge_after_ms -1 disables hedging, 0 means the 800ms default: slow
// exits stall every attempt, so the second lane must fire early. the extra
// inference spend only lands on sends with no first byte yet.
// raceAnnounced tracks the off->on edge of the lane race so the idle-pool
// message logs on transition, not once per request while idle.
var raceAnnounced atomic.Bool

func hedgeDelay() time.Duration {
	if share, threshold := lanesIdleEnough(); share < threshold {
		ms := cfg().Zen.RaceHedgeAfterMs
		if ms < 0 {
			ms = 0
		}
		if raceAnnounced.CompareAndSwap(false, true) {
			acclog.Printf("  opencode lane race on (busy share %.0f%% < %.0f%%)", share*100, threshold*100)
		}
		return time.Duration(ms) * time.Millisecond
	}
	raceAnnounced.Store(false)
	ms := cfg().Zen.HedgeAfterMs
	if ms < 0 {
		return 0 // -1 disables hedging entirely
	}
	if ms == 0 {
		return 800 * time.Millisecond
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
	// zenRefreshFresh makes the next sweep skip the exits already in the
	// pool. sticky until a sweep consumes it, so a fresh request that lands
	// while a periodic refresh is running is not lost.
	zenRefreshFresh atomic.Bool
	// zenRefreshFull makes the next sweep ignore every mark and re-probe the
	// whole list. sticky like zenRefreshFresh: a full recheck that lands during
	// another sweep must not degrade into a fresh one.
	zenRefreshFull atomic.Bool
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

// zenGateIPBlocked reports whether a FreeTierError 403 is zen's per-IP
// refusal — the daily cap or the abusive-traffic block — rather than a
// request-fingerprint rejection. both answer 403 FreeTierError, but only the
// IP one is a fact about the exit: a fingerprint 403 repeats on every exit,
// so parking exits for it just empties the pool.
func zenGateIPBlocked(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "current network") || strings.Contains(s, "abusive traffic")
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

// recheckEveryExit is the out-of-exits recovery: forget every mark in the exit
// registry and re-verify the whole list, the exits already in the pool first.
// the marks are what keep a written-off exit out of a sweep, so a sweep that
// honours them can only find exits the pool has not tried yet — and a pool
// that hands out nothing has already tried them all. the probe decides who
// comes back, so nothing is admitted on a cleared mark alone.
func recheckEveryExit(reason string) {
	clearAllExitMarks()
	zenRefreshFull.Store(true)
	acclog.Printf("  zen pool out: %s, rechecking every exit", reason)
	refreshZenProxiesAsync(true)
}

// refreshZenProxiesAsync triggers a pool refresh without blocking the request
// path. concurrent triggers collapse into one in-flight refresh. fresh asks
// the sweep to skip the exits already in the pool (the dry-pool path); it is
// sticky, so a periodic trigger that lands while a dry sweep is in flight
// does not cancel the fresh intent.
func refreshZenProxiesAsync(fresh bool) {
	if fresh {
		zenRefreshFresh.Store(true)
	}
	go func() {
		if !zenRefreshMu.TryLock() {
			return
		}
		defer zenRefreshMu.Unlock()
		refreshZenProxies(zenRefreshFresh.Swap(false))
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
	// State is working, rate_limited, or dead. a marked exit is listed even
	// when it is not in the pool, so the section explains a thin pool
	// instead of just showing a short list.
	State  string `json:"state,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// poolSummary is the /status view of the proxy pool.
type poolSummary struct {
	Verified    int        `json:"verified"`
	Cands       int        `json:"candidates"`
	Dropped     int        `json:"dropped_total"`
	RateLimited int        `json:"rate_limited"`
	Dead        int        `json:"dead"`
	Refresh     string     `json:"last_refresh_ago"`
	NextRefresh string     `json:"next_refresh_in"`
	Exits       []poolExit `json:"exits,omitempty"`
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
	out.RateLimited = countRateLimited()
	out.Dead = countDeadExits()
	if !refreshed.IsZero() {
		out.Refresh = time.Since(refreshed).Round(time.Second).String()
		if iv := poolInterval(); iv > 0 {
			if left := iv - time.Since(refreshed); left > 0 {
				out.NextRefresh = left.Round(time.Second).String()
			} else {
				out.NextRefresh = "due"
			}
		}
	}
	inPool := map[string]bool{}
	for _, p := range pool {
		inPool[p] = true
		state := exitStateOf(p)
		if state == "" {
			state = "working"
		}
		out.Exits = append(out.Exits, poolExit{
			Proxy: redactProxyUserinfo(p), Country: proxyCountry(p),
			Latency: proxyLatency(p), AvgMs: proxyLiveMs(p), Pinned: pinned[p],
			State: state,
		})
	}
	for _, m := range markedExits() {
		if inPool[m.Proxy] {
			continue // already listed above with its state
		}
		out.Exits = append(out.Exits, poolExit{
			Proxy: redactProxyUserinfo(m.Proxy), Country: proxyCountry(m.Proxy),
			State: m.State, Reason: m.Reason,
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

// proxyUnavailableMsg is the client-facing text for a request that could not
// be placed on any exit. it names the pool and its state, never an upstream
// status: the caller's remedy is to retry shortly, not to change anything.
func proxyUnavailableMsg(model string) string {
	zenProxiesMu.RLock()
	verified := len(zenProxies)
	zenProxiesMu.RUnlock()
	return fmt.Sprintf("opencode zen: no usable exit for %s — %d verified, %d rate-limited, %d dead, %d free-tier parked. proxy capacity, not your request; retry shortly.",
		model, verified, countRateLimited(), countDeadExits(), countGateParked())
}

// zenAnswered is the zenPost sentinel for "the caller already has its
// response": the free-tier failover wrote it, so the ladder stops without
// touching w again (a nil return would draw a second, 503 error body).
var zenAnswered = &http.Response{}

// exhaustedExits is the retry ladder's last word: every attempt landed on an
// exit that could not serve the request (rate limit, transport, overload).
// the client gets a 503 that names the proxy pool as the problem and never a
// 429 echoed back — a rate limit is this proxy's business to absorb, and a
// caller that sees one retries against us instead of waiting us out. the
// status that ended the ladder is in the body reason and the log, not in the
// http status.
func exhaustedExits(model string, status int) *http.Response {
	reason := "every exit rate-limited"
	if status != http.StatusTooManyRequests && status != 529 {
		reason = fmt.Sprintf("every exit failed (last upstream %d)", status)
	}
	msg := fmt.Sprintf("%s [%s, %d attempts]", proxyUnavailableMsg(model), reason, zenMaxRetries())
	eb, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "proxy_exhausted", "message": msg}})
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", "5")
	acclog.Printf("!! opencode exits exhausted model=%s attempts=%d last=%d, synthesizing 503", model, zenMaxRetries(), status)
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(bytes.NewReader(eb)), Header: h}
}

// zenFreeUntil is when zen's anonymous free budget refills, in unix seconds.
// the 429 body is FreeUsageLimitError with a retry-after that lands on 00:00
// utc, and every exit answers it: measured on 140+ IPs across four cloudflare
// colos, public proxies and a home line alike, all with the same countdown.
// one bucket sits behind `Bearer public`, so rotating exits cannot clear it
// and the only way to keep opencode/<model> answering is another backend
// (zenFreeFailover in route.go).
var zenFreeUntil atomic.Int64

// noteZenFreeCap records the refill deadline from a free-usage 429's
// retry-after, keeping the latest one seen.
func noteZenFreeCap(h http.Header) {
	secs := 0
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			secs = n
		}
	}
	if secs <= 0 {
		secs = 60
	}
	until := time.Now().Add(time.Duration(secs) * time.Second).Unix()
	for {
		cur := zenFreeUntil.Load()
		if until <= cur || zenFreeUntil.CompareAndSwap(cur, until) {
			return
		}
	}
}

// zenFreeCapped reports whether the shared budget is still spent.
func zenFreeCapped() bool { return zenFreeUntil.Load() > time.Now().Unix() }

// zenFreeSpentMsg says why a zen model cannot be served: the shared free
// budget is spent and no other backend carries the model.
func zenFreeSpentMsg(model string) string {
	until := time.Unix(zenFreeUntil.Load(), 0).UTC().Format("2006-01-02 15:04 MST")
	return fmt.Sprintf("opencode zen: %s rides zen's free tier, whose budget is shared by every exit and spent until %s; no other backend serves it.", model, until)
}

// clearZenFreeCap drops the marker: a 200 proves the budget is back.
func clearZenFreeCap() { zenFreeUntil.Store(0) }

// zenFreeUsageCap reports whether a 429 body is zen's free-usage answer
// (429 FreeUsageLimitError) rather than a limit aimed at this exit. both are
// reachability, not exclusion; the split only feeds the sweep summary.
func zenFreeUsageCap(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "freeusagelimiterror") ||
		strings.Contains(s, "free usage limit") ||
		strings.Contains(s, "free usage exceeded")
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
			// pin the cached exits before serving: lanes opened without a
			// pin wait on the first sweep (up to two minutes under
			// always_proxy), which is exactly what the cache exists to avoid.
			ensureLanePins()
			warmZenTransports(cached)
			log.Printf("  zen proxies: %d from cache (%s)", len(cached), poolCachePath())
		}
		zenRefreshMu.Lock()
		refreshZenProxies(zenRefreshFresh.Swap(false))
		zenRefreshMu.Unlock()
	}
	for {
		time.Sleep(poolInterval())
		if zenEnabled() {
			zenRefreshMu.Lock()
			refreshZenProxies(zenRefreshFresh.Swap(false))
			zenRefreshMu.Unlock()
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
	// http/1.1 only over the tunnel. http2 multiplexes every stream to zen on
	// one connection per transport, so one dead exit stalls them all and
	// ResponseHeaderTimeout fires per stream against that same connection.
	// inherited true from DefaultTransport, never chosen. the direct client
	// (no exit) keeps h2: it shows no such stalls.
	//
	// ForceAttemptHTTP2 alone is not enough: the clone also inherits
	// DefaultTransport's TLS NextProtos (["h2","http/1.1"]) but not its h2
	// round tripper, so it offers h2, the server answers in h2 frames, and the
	// h1 parser calls every response malformed. protocols http/1 + no ALPN
	// keeps the tunnel on the wire format this transport can actually read.
	tr.ForceAttemptHTTP2 = false
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true)
	if tr.TLSClientConfig != nil {
		tr.TLSClientConfig.NextProtos = nil
	}
	// dial/tls budgets allow normal proxy jitter while still failing dead exits
	// within a few seconds; response header timeout waits up to 60s for large
	// agent prompts on thinking models so healthy exits aren't killed mid-turn.
	tr.TLSHandshakeTimeout = 8 * time.Second
	tr.ResponseHeaderTimeout = 60 * time.Second
	tr.ExpectContinueTimeout = 1 * time.Second
	tr.MaxIdleConnsPerHost = 4
	dialer := &net.Dialer{Timeout: 6 * time.Second, KeepAlive: 30 * time.Second}
	switch pu.Scheme {
	case "socks4":
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			return socks4Dial(c, network, addr, pu.Host)
		}
	case "socks5":
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, cancel := context.WithTimeout(ctx, 8*time.Second)
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
	pruneExitStates(keep)
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
	}
	// credentials in url but proxy chose no-auth: proceed, the url may
	// be shared across proxies with mixed auth.

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
