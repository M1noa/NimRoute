package pii

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// custom terms: config entities + legacy terms as a span detector with
// exact + fuzzy (levenshtein) matching. keeps `minoa`-style personal
// entries working that no model would catch.

type CustomTerm struct {
	Name        string
	Type        string
	Variations  []string
	Replacement string
}

func customLabel(t CustomTerm) Label {
	switch strings.ToLower(t.Type) {
	case "name", "person", "user", "username":
		return LCustom
	case "email":
		return LEmail
	case "phone":
		return LPhone
	default:
		return LCustom
	}
}

func fuzzyEligible(typ, s string) bool {
	n := utf8.RuneCountInString(s)
	switch strings.ToLower(typ) {
	case "name", "username", "user", "person", "other", "":
		return n >= 4
	}
	return n >= 6
}

type customDetector struct {
	spellings []string
	threshold float64
	// source tags entity spans apart from term spans so entity
	// matches win overlaps (config documents entities win).
	source string
}

func spellList(strs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range strs {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func NewCustomDetector(terms []string, entities []CustomTerm, threshold float64) Detector {
	if threshold <= 0 {
		threshold = 0.95
	}
	var spells []string
	for _, e := range entities {
		spells = append(spells, e.Name)
		spells = append(spells, e.Variations...)
	}
	ent := &customDetector{spellList(spells), threshold, "custom-entity"}
	trm := &customDetector{spellList(terms), threshold, "custom-term"}
	return func(text string) []Span {
		return append(ent.detect(text), trm.detect(text)...)
	}
}

// fuzzyWorkBudget caps lines x spellings per detect call: the pass is
// quadratic-ish per window and unbounded entities x multi-MB bodies burn
// seconds of cpu on the request path. prefilter lines cheaply first.
const fuzzyWorkBudget = 200000

// lowerMap lowercases text and records, per lowercased byte, the raw byte it
// came from. strings.ToLower can change byte length (İ U+0130 -> "i",
// K U+212A -> "k"), so an index into the lowercased copy does not address
// the original: slicing raw text with it masks the wrong bytes.
func lowerMap(raw string) (string, []int) {
	var b strings.Builder
	b.Grow(len(raw))
	off := make([]int, 0, len(raw))
	for i := 0; i < len(raw); {
		r, size := decodeRune(raw[i:])
		var buf [4]byte
		n := encodeRune(buf[:], unicode.ToLower(r))
		b.Write(buf[:n])
		for k := 0; k < n; k++ {
			off = append(off, i)
		}
		i += size
	}
	return b.String(), off
}

func (d *customDetector) detect(text string) []Span {
	var out []Span
	// exact pass, case-insensitive with case-preserving restore: "Minoa"
	// must not bypass entity "minoa". offsets walk forward per spelling,
	// so duplicate lines anchor to their own occurrence, not the first.
	lower, off := lowerMap(text)
	rawEnd := func(i int) int {
		if i < len(off) {
			return off[i]
		}
		return len(text)
	}
	for _, sp := range d.spellings {
		lsp := strings.ToLower(sp)
		start := 0
		for {
			i := strings.Index(lower[start:], lsp)
			if i < 0 {
				break
			}
			lo := start + i
			st, en := off[lo], rawEnd(lo+len(lsp))
			out = append(out, Span{st, en, LCustom, 1, d.source, text[st:en]})
			start = lo + len(lsp)
		}
	}
	// fuzzy pass per line for near-miss spellings. long lines run on
	// chunked windows instead of skipping (a single-line 10MB json would
	// otherwise bypass fuzzy entirely).
	budget := fuzzyWorkBudget
	base := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		llen := len(line)
		if strings.TrimSpace(line) == "" {
			base += llen
			continue
		}
		for _, sp := range d.spellings {
			if budget <= 0 {
				break
			}
			budget -= llen
			if !fuzzyEligible(d.entityType(sp), sp) {
				continue
			}
			// cheap prefilter: skip lines sharing no trigram-ish signal.
			if !linePrefilter(line, sp) {
				continue
			}
			for off := 0; off < llen; off += 4096 {
				end := off + 4096
				if end > llen {
					end = llen
				}
				hit := fuzzFind(line[off:end], sp, d.threshold)
				if hit == "" {
					continue
				}
				i := strings.Index(line[off:end], hit)
				if i < 0 {
					continue
				}
				st := base + off + i
				out = append(out, Span{st, st + len(hit), LCustom, 0.9, d.source, hit})
				break
			}
		}
		if budget <= 0 {
			break
		}
		base += llen
	}
	return out
}

// entityType recovers the spelling's entity type for fuzzy eligibility.
// spellings are stored flat, so phone/email spellings get their stricter
// bar via a digits/@ sniff instead of the name default.
func (d *customDetector) entityType(sp string) string {
	digits := 0
	for i := 0; i < len(sp); i++ {
		if sp[i] >= '0' && sp[i] <= '9' {
			digits++
		}
	}
	if strings.Contains(sp, "@") {
		return "email"
	}
	if digits >= 6 {
		return "phone"
	}
	return "name"
}

// linePrefilter is a cheap gate before the quadratic fuzzy pass: the line
// must contain the spelling's first and last rune (case-insensitive), or
// share at least half its distinct letters.
func linePrefilter(line, sp string) bool {
	ll := strings.ToLower(line)
	ls := strings.ToLower(sp)
	if len(ls) == 0 {
		return false
	}
	rs := []rune(ls)
	if !strings.ContainsRune(ll, rs[0]) || !strings.ContainsRune(ll, rs[len(rs)-1]) {
		return false
	}
	seen := map[rune]bool{}
	hit := 0
	for _, r := range rs {
		if !seen[r] {
			seen[r] = true
			if strings.ContainsRune(ll, r) {
				hit++
			}
		}
	}
	return hit*2 >= len(seen)
}

func fuzzFind(s, variation string, threshold float64) string {
	sr := []rune(s)
	vr := []rune(variation)
	n := len(vr)
	if n == 0 || len(sr) < n {
		return ""
	}
	for _, w := range []int{n - 1, n, n + 1} {
		if w < 3 || w > len(sr) {
			continue
		}
		for i := 0; i+w <= len(sr); i++ {
			if similarity(sr[i:i+w], vr) >= threshold {
				return string(sr[i : i+w])
			}
		}
	}
	return ""
}

func similarity(a, b []rune) float64 {
	m, n := len(a), len(b)
	if m == 0 && n == 0 {
		return 1
	}
	if m == 0 || n == 0 {
		return 0
	}
	prev := make([]int, n+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= m; i++ {
		cur := make([]int, n+1)
		cur[0] = i
		for j := 1; j <= n; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			c0 := prev[j] + 1
			c1 := cur[j-1] + 1
			c2 := prev[j-1] + cost
			if c1 < c0 {
				c0 = c1
			}
			if c2 < c0 {
				c0 = c2
			}
			cur[j] = c0
		}
		prev = cur
	}
	mx := m
	if n > mx {
		mx = n
	}
	return 1 - float64(prev[n])/float64(mx)
}
