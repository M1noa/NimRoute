package pii

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func testCfg() Config {
	return Config{
		Enabled: true, Mode: "realistic", DetectSecrets: true, DetectPII: true,
		VaultTTLHours: 24, VaultMaxSize: 4096,
	}
}

// sessions must not share vaults: a surrogate minted in one session
// restores only there.
func TestCrossSessionIsolation(t *testing.T) {
	a := ForRequest(testCfg(), "iso-a")
	b := ForRequest(testCfg(), "iso-b")
	masked := string(a.MaskBody([]byte(`{"content":"contact jane.doe@example.com now"}`)))
	if strings.Contains(masked, "jane.doe@example.com") {
		t.Fatalf("not masked: %s", masked)
	}
	back := string(b.RestoreBody([]byte(masked)))
	if strings.Contains(back, "jane.doe@example.com") {
		t.Fatalf("cross-session restore leaked original: %s", back)
	}
}

// config change rebuilds guards: new entities apply to live sessions.
func TestConfigChangeRebuildsGuard(t *testing.T) {
	c1 := testCfg()
	g1 := ForRequest(c1, "iso-reload")
	up1 := string(g1.MaskBody([]byte(`{"content":"hello Zebulon"}`)))
	if !strings.Contains(up1, "Zebulon") {
		t.Fatalf("unexpected mask before entity: %s", up1)
	}
	c2 := testCfg()
	c2.Entities = []CustomTerm{{Name: "Zebulon", Type: "name"}}
	g2 := ForRequest(c2, "iso-reload")
	up2 := string(g2.MaskBody([]byte(`{"content":"hello Zebulon"}`)))
	if strings.Contains(up2, "Zebulon") {
		t.Fatalf("stale guard survived config change: %s", up2)
	}
}

// expired vault entries must not restore.
func TestExpiredVaultNoRestore(t *testing.T) {
	v := NewVaultMode([]byte("test-key-0123456789abcdef"), time.Millisecond, 4096, "realistic")
	surr := v.For(LEmail, "gone@example.com")
	time.Sleep(5 * time.Millisecond)
	if _, ok := v.Lookup(surr); ok {
		t.Fatal("expired entry still restores")
	}
	if len(v.Surrogates()) != 0 {
		t.Fatalf("expired surrogate listed: %v", v.Surrogates())
	}
}

// duplicate lines anchor to their own occurrence, not the first.
func TestDuplicateLineAnchoring(t *testing.T) {
	c := testCfg()
	c.Entities = []CustomTerm{{Name: "Zebulon", Type: "name"}}
	g := ForRequest(c, "iso-dup")
	body := `{"a":"line Zebulon one","b":"line Zebulon two"}`
	masked := string(g.MaskBody([]byte(body)))
	if strings.Contains(masked, "Zebulon") {
		t.Fatalf("duplicate-line PII leaked: %s", masked)
	}
	back := string(g.RestoreBody([]byte(masked)))
	if strings.Count(back, "Zebulon") != 2 {
		t.Fatalf("want both occurrences restored: %s", back)
	}
}

// natural surrogate collisions stay untouched on restore.
func TestRestoreWordBoundary(t *testing.T) {
	v := NewVaultMode([]byte("test-key-0123456789abcdef"), time.Hour, 4096, "realistic")
	surr := v.For(LGivenName, "Xabier Etxebarria")
	_ = surr
	// model independently writes a common name that happens to equal a
	// surrogate shape: without boundaries it would inject the original.
	text := restoreText("Marcus Aurelius said hi", v)
	if strings.Contains(text, "Xabier") {
		t.Fatalf("natural occurrence replaced: %s", text)
	}
}

// concurrent mask/restore must not race (run with -race).
func TestConcurrentMaskRestore(t *testing.T) {
	g := ForRequest(testCfg(), "iso-race")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			g.MaskBody([]byte(`{"content":"call jane.doe@example.com or 555-123-4567"}`))
		}()
		go func() {
			defer wg.Done()
			g.RestoreBody([]byte(`{"content":"nothing masked here"}`))
		}()
	}
	wg.Wait()
}

// duplicate Replacement values are refused, not aliased.
func TestSeedFixedDuplicateRefused(t *testing.T) {
	v := NewVaultMode([]byte("test-key-0123456789abcdef"), time.Hour, 4096, "realistic")
	if !v.SeedFixed("REDACTED", "Alice", LCustom) {
		t.Fatal("first seed must succeed")
	}
	if v.SeedFixed("REDACTED", "Bob", LCustom) {
		t.Fatal("duplicate replacement must be refused")
	}
	if orig, _ := v.Lookup("REDACTED"); orig != "Alice" {
		t.Fatalf("first owner lost: %q", orig)
	}
}
