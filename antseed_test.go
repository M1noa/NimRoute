package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAntPollDHTFallbackAndModels(t *testing.T) {
	stats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"peers":[{"peerId":"1d90f467689d499dc435e5744b4613c3203eb0aa","providers":[{"provider":"openai","services":["glm-5.3-flash","paid-model","image-model"],"defaultPricing":{"inputUsdPerMillion":1,"outputUsdPerMillion":1},"servicePricing":{"glm-5.3-flash":{"inputUsdPerMillion":0,"outputUsdPerMillion":0},"image-model":{"inputUsdPerMillion":0,"outputUsdPerMillion":0}},"serviceApiProtocols":{"glm-5.3-flash":["openai-chat-completions"],"image-model":["openai-images"]}}]}]}`))
	}))
	defer stats.Close()

	dht, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dht.Close()
	go func() {
		var buf [2048]byte
		n, addr, err := dht.ReadFrom(buf[:])
		if err != nil || !bytes.Contains(buf[:n], []byte("get_peers")) {
			return
		}
		dht.WriteTo([]byte("d1:rd2:id20:aaaaaaaaaaaaaaaaaaaa6:valuesl6:\x08\x08\x08\x08\x1a\xe46:\x01\x01\x01\x01\x1a\xe5ee1:t2:aa1:y1:re"), addr)
	}()

	c := testConfig()
	c.Antseed.StatsURL = stats.URL
	useConfig(t, c)
	offers, models := antPollWithLookup(func(peerID string) []string {
		return antDHTLookupAt(peerID, []string{dht.LocalAddr().String()})
	})
	if len(offers) != 1 || offers[0].hostPort != "8.8.8.8:6884" || len(offers[0].fallback) != 1 || offers[0].fallback[0] != "1.1.1.1:6885" || offers[0].service != "glm-5.3-flash" {
		t.Fatalf("offers = %+v", offers)
	}
	if len(models) != 1 || models[0] != "glm-5.3-flash" {
		t.Fatalf("models = %v", models)
	}
}

// dht-found peers merge into the offer loop under the same free/proto
// gates: a zero-price dht-only service routes even when the snapshot
// omits the seller entirely.
func TestAntPollMergesDHTPeers(t *testing.T) {
	stats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"peers":[]}`))
	}))
	defer stats.Close()

	c := testConfig()
	c.Antseed.StatsURL = stats.URL
	useConfig(t, c)
	antDHTMu.Lock()
	oldPeers, oldSeen, oldTried, oldBan, oldBusy := antDHTPeers, antDHTSeen, antDHTTried, antDHTBan, antDHTBusy
	antDHTPeers, antDHTSeen, antDHTTried, antDHTBan, antDHTBusy = nil, time.Time{}, time.Time{}, time.Time{}, false
	antDHTMu.Unlock()
	t.Cleanup(func() {
		antDHTMu.Lock()
		antDHTPeers, antDHTSeen, antDHTTried, antDHTBan, antDHTBusy = oldPeers, oldSeen, oldTried, oldBan, oldBusy
		antDHTMu.Unlock()
	})

	extra := []antPeer{{
		PeerID: "73b4c9335fa239f9c6df3d28d5bf5d3cdf4de736",
		Addr:   "203.0.113.9:6882",
		Providers: []antProvider{{
			Provider: "openai",
			Services: []string{"deepseek-v4-flash", "paid-model"},
			Default:  antTokenPricing{},
			PerService: map[string]antTokenPricing{
				"paid-model": {Input: 1, Output: 1},
			},
			Protocols: map[string][]string{
				"deepseek-v4-flash": {"openai-chat-completions"},
			},
		}},
	}}
	offers, models := antPollWithPeers(func(string) []string { return nil }, extra)
	if len(offers) != 1 || offers[0].service != "deepseek-v4-flash" || offers[0].hostPort != "203.0.113.9:6882" {
		t.Fatalf("offers = %+v", offers)
	}
	if len(models) != 1 || models[0] != "deepseek-v4-flash" {
		t.Fatalf("models = %v", models)
	}
}

// a crawl that finds nothing still stamps the attempt (throttled by
// the cadence) but not the success stamp (last-good peers never pass
// as fresh). live udp stays out of unit tests: the crawl runs against
// a dead bootstrap with a short budget.
func TestAntDHTAttemptStampsWithoutSuccess(t *testing.T) {
	c := testConfig()
	c.Antseed.DHTBootstrap = []string{"127.0.0.1:1"}
	useConfig(t, c)
	antDHTMu.Lock()
	oldPeers, oldSeen, oldTried, oldBan, oldBusy := antDHTPeers, antDHTSeen, antDHTTried, antDHTBan, antDHTBusy
	antDHTPeers, antDHTSeen, antDHTTried, antDHTBan, antDHTBusy = nil, time.Time{}, time.Time{}, time.Time{}, false
	antDHTMu.Unlock()
	t.Cleanup(func() {
		antDHTMu.Lock()
		antDHTPeers, antDHTSeen, antDHTTried, antDHTBan, antDHTBusy = oldPeers, oldSeen, oldTried, oldBan, oldBusy
		antDHTMu.Unlock()
	})

	antDHTCrawl([]string{"antseed:*"}, 2*time.Second, func(string) *antPeer { return nil })
	antDHTMu.Lock()
	tried, seen, busy := antDHTTried, antDHTSeen, antDHTBusy
	peers := len(antDHTPeers)
	antDHTMu.Unlock()
	if tried.IsZero() {
		t.Fatal("empty crawl must stamp the attempt")
	}
	if !seen.IsZero() {
		t.Fatal("empty crawl must not stamp success")
	}
	if peers != 0 || busy {
		t.Fatalf("empty crawl left peers=%d busy=%v, want 0 false", peers, busy)
	}
}

// a 429 on one seller service must not hide the seller's other
// services; a transport death cools the whole peer.
func TestAntCooldownPerModel(t *testing.T) {
	// empty results force a live re-poll: pin the url unroutable so the
	// fixture survives it.
	c := testConfig()
	c.Antseed.StatsURL = "http://127.0.0.1:1/"
	useConfig(t, c)
	antMu2.Lock()
	oldOffers, oldCool, oldFetched := antOffers, antCool, antFetched
	antOffers = []antOffer{
		{peerID: "p1", hostPort: "127.0.0.1:1", service: "m-a", model: "m-a"},
		{peerID: "p1", hostPort: "127.0.0.1:1", service: "m-b", model: "m-b"},
	}
	antCool = map[string]time.Time{}
	antFetched = time.Now() // pin: antRoutes must not re-poll mid-test
	antMu2.Unlock()
	t.Cleanup(func() {
		antMu2.Lock()
		antOffers, antCool, antFetched = oldOffers, oldCool, oldFetched
		antMu2.Unlock()
	})

	antCoolModel("p1", "m-a")
	if got := antRoutes("m-a"); len(got) != 0 {
		t.Fatalf("cooled m-a routes = %v, want none", got)
	}
	if got := antRoutes("m-b"); len(got) != 1 {
		t.Fatalf("m-b routes = %v, want the uncooled sibling", got)
	}
	antCoolPeer("p1")
	if got := antRoutes("m-b"); len(got) != 0 {
		t.Fatalf("peer-cooled m-b routes = %v, want none", got)
	}
}

func TestAntStatusStartsDiscovery(t *testing.T) {
	stats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TEST-NET-3 stand-in seller (never dialed here): loopback and
		// private snapshot addresses are rejected by antEndpointOK.
		w.Write([]byte(`{"peers":[{"peerId":"1d90f467689d499dc435e5744b4613c3203eb0aa","publicAddress":"203.0.113.7:6882","providers":[{"provider":"openai","services":["glm-5.3-flash"],"defaultPricing":{"inputUsdPerMillion":0,"outputUsdPerMillion":0}}]}]}`))
	}))
	defer stats.Close()

	c := testConfig()
	c.Antseed.Enabled = true
	c.Antseed.StatsURL = stats.URL
	c.Status.ShowAntseed = true
	useConfig(t, c)
	antMu2.Lock()
	oldOffers, oldModels, oldFetched, oldFetching := antOffers, antModels, antFetched, antFetching
	antOffers, antModels, antFetched, antFetching = nil, nil, time.Time{}, false
	antMu2.Unlock()
	t.Cleanup(func() {
		antMu2.Lock()
		antOffers, antModels, antFetched, antFetching = oldOffers, oldModels, oldFetched, oldFetching
		antMu2.Unlock()
	})

	p := newPool(nil)
	if sr := p.StatusFor(true); sr.Antseed == nil {
		t.Fatal("enabled antseed must appear before discovery completes")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sr := p.StatusFor(true); sr.Antseed != nil && len(sr.Antseed.Models) == 1 && sr.Antseed.Models[0] == "glm-5.3-flash" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("status did not discover free models")
}

func TestAntSortedModelsUsesLocalUsage(t *testing.T) {
	c := testConfig()
	c.Usage.Enabled = true
	useConfig(t, c)
	totalsMu.Lock()
	old := totals
	totals = map[string]*usageTotals{
		"antseed/z-model": {Requests: 5},
		"antseed/a-model": {Requests: 2},
	}
	totalsMu.Unlock()
	t.Cleanup(func() {
		totalsMu.Lock()
		totals = old
		totalsMu.Unlock()
	})

	got := antSortedModels([]string{"b-model", "a-model", "z-model"})
	want := []string{"z-model", "a-model", "b-model"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
}
