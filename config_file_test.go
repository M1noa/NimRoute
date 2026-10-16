package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// captureLog points the standard logger at a buffer for the duration of f.
// the suite logs plenty; tests that assert on output scope theirs.
func captureLog(f func()) string {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	f()
	return buf.String()
}

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

// owner: loadConfigFile. a top-level key the schema does not know is a typo:
// yaml.v3 ignores it, so the user thinks they enabled something they did not.
// the warning must name the key, and nested keys stay quiet (they are the
// section owner's business, and a warning per leaf is noise).
func TestLoadConfigFileWarnsUnknownTopKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name string
		body string
		want []string // keys the warning must name; empty = no warning at all
	}{
		{"typo at top level", "always_prox: true\nserver:\n  port: 5419\n", []string{"always_prox"}},
		{"null typo still named", "always_prox:\n", []string{"always_prox"}},
		{"two typos", "always_prox: true\nzen_proxies: []\n", []string{"always_prox", "zen_proxies"}},
		{"schema keys are silent", "server:\n  port: 5419\nnvidia_keys:\n  main: k\nmodels:\n  nudge: []\n", nil},
		{"nested typo stays quiet", "zen:\n  always_proxy: true\n", nil},
		{"empty file", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := write(tc.name+".yml", tc.body)
			var c *appConfig
			out := captureLog(func() {
				var err error
				c, err = loadConfigFile(p)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
			})
			if c == nil {
				t.Fatal("nil config")
			}
			if len(tc.want) == 0 {
				if strings.Contains(out, "unknown top-level key") {
					t.Fatalf("unexpected warning: %q", out)
				}
				return
			}
			for _, k := range tc.want {
				if !strings.Contains(out, k) {
					t.Fatalf("want a warning naming %q, got %q", k, out)
				}
			}
		})
	}
}

// owner: loadConfigFile. the typo must not do what it looks like: a top-level
// always_prox is not zen.always_proxy, and the rest of the file still applies.
func TestLoadConfigFileUnknownKeyIsInert(t *testing.T) {
	p := filepath.Join(t.TempDir(), "typo.yml")
	body := "always_prox: true\nserver:\n  port: 5419\n"
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfigFile(p)
	if err != nil {
		t.Fatalf("a typo must not fail the load: %v", err)
	}
	if c.Zen.AlwaysProxy {
		t.Fatal("top-level always_proxy must not enable zen always_proxy")
	}
	if c.Server.Port != 5419 {
		t.Fatalf("server.port = %d, want 5419", c.Server.Port)
	}
}

// owner: knownTopKeys. config.yml.example is the schema source of truth, so a
// section it documents but defaultConfig() has not caught up with is known,
// not a typo; and the shipped example names nothing outside the schema.
func TestKnownTopKeysFollowsExample(t *testing.T) {
	ahead := knownTopKeys([]byte("future_section:\n  knob: 1\n"))
	for _, k := range []string{"server", "zen", "future_section"} {
		if !ahead[k] {
			t.Fatalf("%q must be known", k)
		}
	}
	structOnly := knownTopKeys(nil)
	if !structOnly["server"] || !structOnly["nvidia_keys"] {
		t.Fatal("struct tags must be known without the example")
	}
	if structOnly["future_section"] {
		t.Fatal("example-only key must not leak into the struct tag set")
	}
	ex, err := os.ReadFile("config.yml.example")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(ex, &doc); err != nil {
		t.Fatalf("parse example: %v", err)
	}
	for _, k := range topLevelKeys(&doc) {
		if !structOnly[k] {
			t.Fatalf("example key %q is not an appConfig yaml tag", k)
		}
	}
}
