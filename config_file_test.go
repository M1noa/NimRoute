package main

import (
	"os"
	"path/filepath"
	"testing"
)

// loadOrCreateConfig must never destroy a present-but-broken config: it
// holds every secret, and the old code truncated it with the example.
func TestLoadOrCreateConfigKeepsMalformed(t *testing.T) {
	dir := t.TempDir()
	// absent: seeds from example.
	missing := filepath.Join(dir, "missing.yml")
	c := loadOrCreateConfig(missing)
	if c == nil {
		t.Fatal("want non-nil config for absent file")
	}
	if _, err := os.Stat(missing); err != nil {
		t.Fatalf("absent file must be seeded: %v", err)
	}
	// malformed: bytes untouched, defaults running. (duplicate top-level
	// key: yaml enforces uniqueKeys, the freebuff_login failure shape.)
	bad := filepath.Join(dir, "bad.yml")
	secret := "nvidia_keys:\n  main: \"nvapi-SECRET\"\nserver:\n  port: 5419\nserver:\n  port: 5420\n"
	if err := os.WriteFile(bad, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	c = loadOrCreateConfig(bad)
	if c == nil {
		t.Fatal("want non-nil config for malformed file")
	}
	after, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != secret {
		t.Fatalf("malformed config was rewritten:\n%s", after)
	}
	// null enabled: counts as absent (default-true survives).
	nul := filepath.Join(dir, "null.yml")
	if err := os.WriteFile(nul, []byte("nvidia:\n  enabled:\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cn, err := loadConfigFile(nul)
	if err != nil {
		t.Fatalf("null-enabled file must parse: %v", err)
	}
	if !cn.Nvidia.Enabled {
		t.Fatal("null enabled: must read as default-true, not false")
	}
}

// owner: loadConfigFile. defaults are pre-seeded from defaultConfig() and
// yaml.v3 leaves absent keys untouched, so an explicit false must survive
// while an absent section keeps the default. this is the invariant the old
// raw-doc presence checks claimed to protect; there are none now.
func TestLoadConfigFileEnabledDefaults(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name    string
		body    string
		enabled bool // zen.enabled
	}{
		{"absent section", "server:\n  port: 5419\n", true},
		{"empty section", "zen: {}\n", true},
		{"explicit false", "zen:\n  enabled: false\n", false},
		{"explicit true", "zen:\n  enabled: true\n", true},
		{"null", "zen:\n  enabled:\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadConfigFile(write(tc.name+".yml", tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if c.Zen.Enabled != tc.enabled {
				t.Fatalf("zen.enabled = %v, want %v", c.Zen.Enabled, tc.enabled)
			}
			if !c.Status.ShowUsage {
				t.Fatal("status.show_usage must stay default-true")
			}
			if c.Guardrails.Threshold != 0.6 {
				t.Fatalf("guardrails.threshold = %v, want default 0.6", c.Guardrails.Threshold)
			}
		})
	}
}
