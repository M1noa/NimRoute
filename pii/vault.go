package pii

import (
	"container/list"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
)

// vault maps originals to stable surrogates per session. surrogates derive
// from hmac(key, category|normalized original) so they stay identical across
// requests and restarts; collisions re-derive with a counter.

type vaultEntry struct {
	original  string
	surrogate string
	label     Label
	expires   time.Time
	elem      *list.Element
}

type Vault struct {
	mu      sync.Mutex
	key     []byte
	ttl     time.Duration
	maxSize int
	mode    string
	byOrig  map[string]*vaultEntry
	bySurr  map[string]*vaultEntry
	lru     *list.List
}

func NewVault(key []byte, ttl time.Duration, maxSize int) *Vault {
	return NewVaultMode(key, ttl, maxSize, "realistic")
}

func NewVaultMode(key []byte, ttl time.Duration, maxSize int, mode string) *Vault {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxSize <= 0 {
		maxSize = 4096
	}
	if mode == "" {
		mode = "realistic"
	}
	k := append([]byte(nil), key...)
	return &Vault{
		key: k, ttl: ttl, maxSize: maxSize, mode: mode,
		byOrig: map[string]*vaultEntry{},
		bySurr: map[string]*vaultEntry{},
		lru:    list.New(),
	}
}

// normOrig canonicalizes the lookup key (case + whitespace folded), so
// restore returns the first-seen surface form for later case variants.
// documented, not fixed: per-request forms would break cross-request
// surrogate stability, which is the vault's whole point.
func normOrig(label Label, s string) string {
	return string(label) + "|" + strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func (v *Vault) derive(label Label, original string, salt uint32) string {
	m := hmac.New(sha256.New, v.key)
	m.Write([]byte(normOrig(label, original)))
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], salt)
	m.Write(b[:])
	sum := m.Sum(nil)
	switch v.mode {
	case "variable":
		return variableFor(label, original, sum, v)
	case "label":
		return labelFor(label, sum, v)
	default:
		return fakeFor(label, original, sum)
	}
}

func (v *Vault) For(label Label, original string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := normOrig(label, original)
	if e, ok := v.byOrig[k]; ok {
		if time.Now().Before(e.expires) {
			v.lru.MoveToFront(e.elem)
			return e.surrogate
		}
		v.remove(e)
	}
	// bound the salt loop: small labels (url has 5 fakes, phone ~7M) can
	// exhaust their pool, and an unbounded loop then spins forever at 100%
	// cpu, hanging the request. after maxSurrogateSalts tries, uniquify
	// mints a free surrogate deterministically.
	var surr string
	free := false
	for salt := uint32(0); salt < maxSurrogateSalts; salt++ {
		surr = v.derive(label, original, salt)
		// never mint a surrogate equal to the original: the mask would be a
		// no-op and the "fake" a verbatim copy of the real value.
		if surr == original {
			continue
		}
		if _, taken := v.bySurr[surr]; !taken {
			free = true
			break
		}
	}
	if !free {
		surr = v.uniquify(label, original)
	}
	e := &vaultEntry{original: original, surrogate: surr, label: label, expires: time.Now().Add(v.ttl)}
	e.elem = v.lru.PushFront(k)
	v.byOrig[k] = e
	v.bySurr[surr] = e
	// expired entries linger until their original is re-added: sweep them
	// once the map is half full so dead weight can't pin live entries out.
	if len(v.byOrig) >= v.maxSize/2 {
		now := time.Now()
		for _, oe := range v.byOrig {
			if now.After(oe.expires) {
				v.remove(oe)
			}
		}
	}
	for v.lru.Len() > v.maxSize {
		back := v.lru.Back()
		if back == nil {
			break
		}
		if e2, ok := v.byOrig[back.Value.(string)]; ok {
			v.remove(e2)
		} else {
			v.lru.Remove(back)
		}
	}
	return surr
}

// SeedFixed registers a fixed surrogate -> original pair (entity
// `replacement:` values). seeded entries share the vault ttl and lru
// membership like derived ones: immortal entries never expire and never
// count to maxSize. a duplicate Replacement for a different original is
// refused (reported): silently aliasing it masks to a derived surrogate
// yet restores to the first entity's original.
func (v *Vault) SeedFixed(surrogate, original string, label Label) bool {
	if surrogate == "" || original == "" {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if e, taken := v.bySurr[surrogate]; taken {
		return normOrig(e.label, e.original) == normOrig(label, original)
	}
	e := &vaultEntry{original: original, surrogate: surrogate, label: label, expires: time.Now().Add(v.ttl)}
	e.elem = v.lru.PushFront(normOrig(label, original))
	v.byOrig[normOrig(label, original)] = e
	v.bySurr[surrogate] = e
	for v.lru.Len() > v.maxSize {
		back := v.lru.Back()
		if back == nil {
			break
		}
		if e2, ok := v.byOrig[back.Value.(string)]; ok {
			v.remove(e2)
		} else {
			v.lru.Remove(back)
		}
	}
	return true
}

// maxSurrogateSalts caps the hmac salt-retry loop in Vault.For.
const maxSurrogateSalts = 64

// uniquify mints a free surrogate when derive's fake pool is exhausted:
// the base fake plus a "~n" index. deterministic for a given (label,
// original) and vault state, and the index space is unbounded so a free
// candidate always exists.
func (v *Vault) uniquify(label Label, original string) string {
	base := v.derive(label, original, 0)
	for n := 2; ; n++ {
		cand := base + "~" + fmt.Sprint(n)
		if cand == original {
			continue
		}
		if _, taken := v.bySurr[cand]; !taken {
			return cand
		}
	}
}

func (v *Vault) remove(e *vaultEntry) {
	delete(v.byOrig, normOrig(e.label, e.original))
	delete(v.bySurr, e.surrogate)
	if e.elem != nil {
		v.lru.Remove(e.elem)
	}
}

func (v *Vault) Lookup(surrogate string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.bySurr[surrogate]
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.original, true
}

func (v *Vault) Surrogates() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	out := make([]string, 0, len(v.bySurr))
	for s, e := range v.bySurr {
		// expired entries are dead placeholders: including them makes
		// the streaming holdback over-hold and restore scan garbage.
		if now.After(e.expires) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (v *Vault) MaxSurrogateLen() int {
	n := 0
	for _, s := range v.Surrogates() {
		if len(s) > n {
			n = len(s)
		}
	}
	return n
}

// global session vault registry: id -> vault. sessions expire with ttl.
var (
	vaultsMu  sync.Mutex
	vaults    = map[string]*vaultEntry2{}
	vaultsLRU = list.New()
)

// maxVaults mirrors maxGuards: the session vault registry is keyed by
// the same attacker-influenced session id.
const maxVaults = 512

type vaultEntry2 struct {
	vault *Vault
	elem  *list.Element
}

func SessionVault(id string, key []byte, ttl time.Duration, maxSize int) *Vault {
	return SessionVaultMode(id, key, ttl, maxSize, "realistic")
}

func SessionVaultMode(id string, key []byte, ttl time.Duration, maxSize int, mode string) *Vault {
	// bound an attacker-influenced key. ForRequest passes
	// "sid:<64 hex>@<64 hex>" = 133 bytes, so the cut must sit above that.
	if len(id) > 160 {
		id = id[:160]
	}
	vaultsMu.Lock()
	defer vaultsMu.Unlock()
	if e, ok := vaults[id]; ok {
		vaultsLRU.MoveToFront(e.elem)
		return e.vault
	}
	v := NewVaultMode(key, ttl, maxSize, mode)
	e := &vaultEntry2{vault: v}
	e.elem = vaultsLRU.PushFront(id)
	vaults[id] = e
	for vaultsLRU.Len() > maxVaults {
		back := vaultsLRU.Back()
		if back == nil {
			break
		}
		delete(vaults, back.Value.(string))
		vaultsLRU.Remove(back)
	}
	return v
}
