package main

import "testing"

// zen limits the free tier per exit+model, so a 429 on one model must never
// write the exit off for the rest: measured 2026-11-03, one exit answered the
// cap for glm and deepseek while serving nemotron and beam-free in the same
// minute.
func TestExit429MarksOnlyTheLimitedModel(t *testing.T) {
	p := "http://test-429-pair:1080"
	markExitRateLimited(p, "glm-5.3-flash-free", "test")
	if exitUsableFor(p, "glm-5.3-flash-free") {
		t.Fatal("the limited pair must not be usable")
	}
	if !exitUsableFor(p, "deepseek-v4.1-flash-free") {
		t.Fatal("a 429 on one model must not write off the exit for another")
	}
}

// the free-tier 429 carries a retry-after that counts down to utc midnight from
// every exit, so it says nothing about the ip's other models: the cap marks the
// pair and leaves the exit drawable.
func TestTierCapKeepsTheExitForOtherModels(t *testing.T) {
	p := "http://test-tier-cap:1080"
	noteExitTierCap(p, "glm-5.3-flash-free", 22079)
	if !exitUsableFor(p, "deepseek-v4.1-flash-free") {
		t.Fatal("a cap on one model must not park the exit for the rest")
	}
}

// RegionError is country-scoped per model: the gated pair is skipped, the same
// exit keeps serving other models, and exits in another country keep the model.
func TestRegionGateSparesOtherModelsAndRegions(t *testing.T) {
	p, q := "http://test-geo-a:1080", "http://test-geo-b:1080"
	setProxyCountries(map[string]string{p: "SG", q: "US"})
	defer setProxyCountries(map[string]string{})

	noteZenRegionBlock(p, "beam-free")
	if exitUsableFor(p, "beam-free") {
		t.Error("the gated model/country pair must not be usable")
	}
	if !exitUsableFor(p, "glm-5.3-flash-free") {
		t.Error("another model on the same exit and region must stay usable")
	}
	if !exitUsableFor(q, "beam-free") {
		t.Error("another region must stay usable for the gated model")
	}

	clearZenRegionGates()
	if !exitUsableFor(p, "beam-free") {
		t.Error("a refresh must re-check the gate instead of keeping it forever")
	}
}
