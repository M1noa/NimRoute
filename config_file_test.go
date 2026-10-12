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
