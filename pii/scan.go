package pii

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// scanner: paragraph-chunked detection with an lru span cache, scope-limited
// masking for oai + anthropic bodies, and streaming restore.

type cacheEntry struct {
	key   string
	spans []Span
	elem  *list.Element
	// lastAccess stamps the last hit/put, so the sweep can evict
	// entries idle past the scan-cache ttl (vault entries live longer).
	lastAccess time.Time
}

type ScanCache struct {
	mu    sync.Mutex
	max   int
	items map[string]*cacheEntry
	lru   *list.List
	Hits  int
	Miss  int
}

func NewScanCache(max int) *ScanCache {
	if max <= 0 {
		max = 2048
	}
	return &ScanCache{max: max, items: map[string]*cacheEntry{}, lru: list.New()}
}

func cacheKey(text, ver string) string {
	h := sha256.New()
	h.Write([]byte(ver))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

// regexSample is the fixed synthetic text for the startup benchmark:
// pii-dense prose repeated past 1KB so us/KB is stable across runs.
var regexSample = strings.Repeat("contact alex rivera at alex@example.com ssn 472-81-0094 ip 10.0.0.1 card 4111 1111 1111 1111 url https://example.com/x. ", 16)

// RegexUSPerKB runs the heuristic detectors over the sample 3 times and
// returns microseconds per KB (median). call once at startup, never per
// request: it costs ~milliseconds.
func RegexUSPerKB() float64 {
	const runs = 3
	ds := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		start := time.Now()
		detectHeuristics(regexSample)
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	kb := float64(len(regexSample)) / 1024
	if kb == 0 {
		return 0
	}
	return float64(ds[len(ds)/2].Microseconds()) / kb
}

func (c *ScanCache) get(key string) ([]Span, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.lru.MoveToFront(e.elem)
		e.lastAccess = time.Now()
		c.Hits++
		return e.spans, true
	}
	c.Miss++
	return nil, false
}

func (c *ScanCache) put(key string, spans []Span) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.spans = spans
		e.lastAccess = time.Now()
		c.lru.MoveToFront(e.elem)
		return
	}
	e := &cacheEntry{key: key, spans: spans, lastAccess: time.Now()}
	e.elem = c.lru.PushFront(key)
	c.items[key] = e
	for c.lru.Len() > c.max {
		back := c.lru.Back()
		if back == nil {
			break
		}
		delete(c.items, back.Value.(string))
		c.lru.Remove(back)
	}
}

// sweepIdle evicts entries untouched for longer than ttl. the vault keeps
// its own (longer) ttl: evicting a scan entry never evicts vault entries.
func (c *ScanCache) sweepIdle(ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, e := range c.items {
		if e.lastAccess.Before(cutoff) {
			delete(c.items, k)
			c.lru.Remove(e.elem)
			n++
		}
	}
	return n
}

type Detector func(text string) []Span

type Scanner struct {
	Detectors []Detector
	Cache     *ScanCache
	Keep      map[Label]bool
	Ver       string
	// Replacements maps lowercased entity spellings to a fixed
	// surrogate, bypassing the vault when set.
	Replacements map[string]string
}

// surrogateFor returns a fixed replacement for entity spellings, else "".
func (s *Scanner) surrogateFor(orig string) string {
	if len(s.Replacements) == 0 {
		return ""
	}
	return s.Replacements[strings.ToLower(strings.TrimSpace(orig))]
}

// splitParagraphs splits on blank lines, recording each paragraph's raw
// offset and its separator's real length (\r\n and 3+ newline runs broke
// the old fixed +2 stride, misaligning every later span).
func splitParagraphs(s string) (paras []string, bases []int) {
	base := 0
	for len(s) > 0 {
		i := strings.Index(s, "\n\n")
		if i < 0 {
			paras = append(paras, s)
			bases = append(bases, base)
			break
		}
		// extend over \r and extra newlines: the separator is the full
		// blank run starting at i.
		j := i
		for j < len(s) && (s[j] == '\n' || s[j] == '\r') {
			j++
		}
		paras = append(paras, s[:i])
		bases = append(bases, base)
		base += j
		s = s[j:]
	}
	return paras, bases
}

// detect runs detectors per paragraph with cache; offsets shifted back.
func (s *Scanner) detect(text string) []Span {
	var out []Span
	paras, bases := splitParagraphs(text)
	for pi, para := range paras {
		base := bases[pi]
		if strings.TrimSpace(para) == "" {
			continue
		}
		key := cacheKey(para, s.Ver)
		spans, ok := s.Cache.get(key)
		if !ok {
			for _, d := range s.Detectors {
				spans = append(spans, d(para)...)
			}
			s.Cache.put(key, spans)
		}
		for _, sp := range spans {
			sp.Start += base
			sp.End += base
			sp.Text = text[sp.Start:sp.End]
			out = append(out, sp)
		}
	}
	return out
}

// maskText replaces redactable spans with vault surrogates, right to left.
func (s *Scanner) maskText(text string, v *Vault) string {
	spans := applyPolicy(s.detect(text), s.Keep)
	for _, sp := range spans {
		surr := s.surrogateFor(text[sp.Start:sp.End])
		if surr == "" {
			surr = v.For(sp.Label, text[sp.Start:sp.End])
		}
		text = text[:sp.Start] + surr + text[sp.End:]
	}
	return text
}

// restoreText swaps surrogates back, longest first. replacements happen
// only on word boundaries: a model that independently writes "Marcus" in
// an unrelated answer must not receive another user's original name.
// esc form handles json string values.
func restoreText(text string, v *Vault) string {
	ss := v.Surrogates()
	sort.Slice(ss, func(i, j int) bool { return len(ss[i]) > len(ss[j]) })
	for _, surr := range ss {
		orig, ok := v.Lookup(surr)
		if !ok {
			continue
		}
		if strings.Contains(text, surr) {
			text = replaceBounded(text, surr, orig)
		}
		je, jo := jsonEscape(surr), jsonEscape(orig)
		if je != surr && strings.Contains(text, je) {
			text = replaceBounded(text, je, jo)
		}
	}
	return text
}

// replaceBounded replaces needle with repl only where both sides sit on a
// non-word boundary (start/end of text or a non [A-Za-z0-9_] byte).
// surrogate shapes with word chars inside still match whole.
func replaceBounded(text, needle, repl string) string {
	if needle == "" {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	i := 0
	for {
		j := strings.Index(text[i:], needle)
		if j < 0 {
			b.WriteString(text[i:])
			break
		}
		j += i
		if wordCharAt(text, j-1) || wordCharAt(text, j+len(needle)) {
			b.WriteString(text[i : j+len(needle)])
			i = j + len(needle)
			continue
		}
		b.WriteString(text[i:j])
		b.WriteString(repl)
		i = j + len(needle)
	}
	return b.String()
}

func wordCharAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}

// maskBody masks pii spans directly in the raw bytes so the body stays
// byte-identical apart from the replacements (key order, spacing kept).
// structured scope: skip the top-level model id and image/base64 blobs by
// pre-marking their ranges as no-scan zones.
func (s *Scanner) maskBody(body []byte, v *Vault, includeSystem bool) []byte {
	text := string(body)
	zones := noScanZones(text, includeSystem)
	spans := s.detectZoned(text, zones)
	spans = applyPolicy(spans, s.Keep)
	for _, sp := range spans {
		orig := text[sp.Start:sp.End]
		surr := s.surrogateFor(orig)
		if surr == "" {
			surr = v.For(sp.Label, orig)
		}
		// the body is json, so the surrogate must be json-escaped: a custom
		// `replacement` or a maskShape-preserved backslash would otherwise
		// ship raw bytes that break the document (and trip the fail-open in
		// MaskBody, leaking the original). spans are right-to-left, so
		// earlier offsets stay valid.
		text = text[:sp.Start] + jsonEscape(surr) + text[sp.End:]
	}
	return []byte(text)
}

// noScanZones returns byte ranges to exclude: image/base64 blobs always,
// system content unless includeSystem, plus the top-level model id (so a
// detector hit there can't trigger a full-body remarshal). only the FIRST
// occurrence of each skip string is zoned: it sits at its structural
// position, while identical user text later in the body stays scannable.
// residual: a user string placed before its system twin still zones wrong;
// the common layout (system first) is correct.
func noScanZones(text string, includeSystem bool) [][2]int {
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return nil
	}
	var zones [][2]int
	zoneFirst := func(s string) {
		if s == "" {
			return
		}
		if i := strings.Index(text, s); i >= 0 {
			zones = append(zones, [2]int{i, i + len(s)})
		}
	}
	if md, ok := m["model"].(string); ok {
		zoneFirst(`"model":"` + md + `"`)
		zoneFirst(`"model": "` + md + `"`)
	}
	var strs []string
	collectSkips(m, includeSystem, &strs)
	for _, s := range strs {
		if len(s) < 24 {
			continue
		}
		zoneFirst(s)
	}
	return zones
}

func collectSkips(x any, includeSystem bool, out *[]string) {
	switch t := x.(type) {
	case map[string]any:
		typ, _ := t["type"].(string)
		role, _ := t["role"].(string)
		if typ == "image" || typ == "image_url" || typ == "input_image" || typ == "input_file" || typ == "document" {
			if s, ok := t["url"].(string); ok {
				*out = append(*out, s)
			}
			if src, ok := t["source"].(map[string]any); ok {
				if d, ok := src["data"].(string); ok {
					*out = append(*out, d)
				}
			}
			if u, ok := t["image_url"]; ok {
				if s, ok := u.(string); ok {
					*out = append(*out, s)
				} else if mm, ok := u.(map[string]any); ok {
					if s, ok := mm["url"].(string); ok {
						*out = append(*out, s)
					}
				}
			}
			return
		}
		if (role == "system" || role == "developer") && !includeSystem {
			if c, ok := t["content"]; ok {
				flattenStrings(c, out)
			}
			if in, ok := t["input"]; ok {
				flattenStrings(in, out)
			}
			// still walk tool_calls (live args mask even in system msgs)
			if tc, ok := t["tool_calls"]; ok {
				collectSkips(tc, includeSystem, out)
			}
			return
		}
		for _, v := range t {
			collectSkips(v, includeSystem, out)
		}
	case []any:
		for _, v := range t {
			collectSkips(v, includeSystem, out)
		}
	}
}

func flattenStrings(x any, out *[]string) {
	switch t := x.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, v := range t {
			flattenStrings(v, out)
		}
	case map[string]any:
		for _, v := range t {
			flattenStrings(v, out)
		}
	}
}

// detectZoned runs detectors per paragraph, dropping spans inside no-scan
// zones. zones merge with paragraph offsets: paragraphs index raw text.
func (s *Scanner) detectZoned(text string, zones [][2]int) []Span {
	inside := func(st, en int) bool {
		for _, z := range zones {
			if st < z[1] && en > z[0] {
				return true
			}
		}
		return false
	}
	var out []Span
	paras, bases := splitParagraphs(text)
	for pi, para := range paras {
		base := bases[pi]
		if strings.TrimSpace(para) == "" {
			continue
		}
		// shift paragraph-local zone bounds
		var pz [][2]int
		for _, z := range zones {
			if z[1] > base && z[0] < base+len(para) {
				pz = append(pz, [2]int{z[0] - base, z[1] - base})
			}
		}
		key := cacheKey(para, s.Ver)
		spans, ok := s.Cache.get(key)
		if !ok {
			for _, d := range s.Detectors {
				spans = append(spans, d(para)...)
			}
			s.Cache.put(key, spans)
		}
		for _, sp := range spans {
			if inside(base+sp.Start, base+sp.End) {
				continue
			}
			// also drop paragraph-local zone hits (belt and suspenders
			// for spans found before zone shifting).
			drop := false
			for _, z := range pz {
				if sp.Start < z[1] && sp.End > z[0] {
					drop = true
					break
				}
			}
			if drop {
				continue
			}
			sp.Start += base
			sp.End += base
			sp.Text = text[sp.Start:sp.End]
			out = append(out, sp)
		}
	}
	return out
}

// restoreBody swaps surrogates back in a complete response body.
// mirrors MaskBody's guard: a restore that breaks valid json ships the
// masked body instead of corrupting the client response.
func restoreBody(body []byte, v *Vault) []byte {
	if v == nil {
		return body
	}
	s := restoreText(string(body), v)
	out := []byte(s)
	if json.Valid(body) && !json.Valid(out) {
		return body
	}
	return out
}

// revealer restores placeholders in a byte stream, holding only a suffix
// that could still be a partial surrogate so ttft is unchanged.
type Revealer struct {
	v   *Vault
	buf []byte
	max int
}

func NewRevealer(v *Vault) *Revealer {
	n := 64
	if v != nil {
		if m := v.MaxSurrogateLen(); m+8 > n {
			n = m + 8
		}
	}
	return &Revealer{v: v, max: n}
}

func (r *Revealer) Push(chunk []byte) []byte {
	if r.v == nil {
		return chunk
	}
	// the vault grows mid-stream (uniquify "~n" lengthens surrogates): a
	// snapshot max from NewRevealer would split later restores. refresh
	// cheaply: MaxSurrogateLen skips expired entries already.
	if m := r.v.MaxSurrogateLen() + 8; m > r.max {
		r.max = m
	}
	data := append(append([]byte(nil), r.buf...), chunk...)
	if len(data) <= r.max {
		r.buf = data
		return nil
	}
	cut := len(data) - r.max
	cut = unsplit(data, cut, r.v)
	// never cut mid-rune or mid-escape: chunk prefixes are arbitrary and
	// the json guard in restoreBody only covers whole bodies.
	for cut > 0 && cut < len(data) && !utf8.RuneStart(data[cut]) {
		cut--
	}
	if cut > 0 && data[cut-1] == '\\' {
		cut--
		for cut > 0 && cut < len(data) && !utf8.RuneStart(data[cut]) {
			cut--
		}
	}
	out := restoreBody(data[:cut], r.v)
	r.buf = append([]byte(nil), data[cut:]...)
	return out
}

func (r *Revealer) Flush() []byte {
	if r.v == nil || len(r.buf) == 0 {
		return nil
	}
	out := restoreBody(r.buf, r.v)
	r.buf = nil
	return out
}

func unsplit(data []byte, cut int, v *Vault) int {
	// prefix set of all surrogates: a split match ending at cut means
	// data[cut-l:cut] is a proper prefix of some surrogate. set lookup
	// per l replaces the old O(surrogates x cut) scan.
	prefixes := map[string]bool{}
	maxL := 0
	for _, surr := range v.Surrogates() {
		for l := 1; l < len(surr); l++ {
			prefixes[surr[:l]] = true
		}
		if len(surr)-1 > maxL {
			maxL = len(surr) - 1
		}
	}
	for {
		moved := false
		top := maxL
		if top > cut {
			top = cut
		}
		for l := top; l > 0; l-- {
			if prefixes[string(data[cut-l:cut])] {
				cut -= l
				moved = true
				break
			}
		}
		if !moved {
			return cut
		}
	}
}
