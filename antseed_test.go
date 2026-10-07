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

func TestAntStatusStartsDiscovery(t *testing.T) {
	stats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"peers":[{"peerId":"1d90f467689d499dc435e5744b4613c3203eb0aa","publicAddress":"127.0.0.1:6882","providers":[{"provider":"openai","services":["glm-5.3-flash"],"defaultPricing":{"inputUsdPerMillion":0,"outputUsdPerMillion":0}}]}]}`))
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
