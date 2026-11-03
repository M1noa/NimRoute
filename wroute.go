package main

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// wroute is route's wide sibling, folded by model line rather than by exact
// version: route/ pins a version (route/deepseek-v4.1-flash and
// route/deepseek-v4-flash are two routes), wroute/ folds every patch of one
// line into one route. the id is the longest version the row's models share:
// deepseek v4 and v4.1 fall back to wroute/deepseek-v4-flash, a row of 2.6
// builds lists as wroute/mimo-v2.6-flash, and glm-4.7 stays out of glm-5*.
// a different name (glm-5.3-flash vs glm-5.3) never merges.

// wrouteEnabled rides on route.enabled: one switch for both names.
func wrouteEnabled() bool { return routeEnabled() }

// wrouteMinModels is how many exact models a name needs to count as wide: one
// model is just route/<key> again.
const wrouteMinModels = 2

// wrouteTokens splits an id into lowercase tokens. dots stay inside a token so
// v4.1 reads as one revision rather than as v4 plus 1.
func wrouteTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(strings.TrimSpace(s)), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.')
	})
}

// wrouteIsVersion reports whether a token versions a model rather than naming
// it: v4, v4.1, 5.5, 4.6v, 0731. qwen3.6 and 26b are part of a name.
func wrouteIsVersion(t string) bool {
	t = strings.TrimSuffix(strings.TrimPrefix(t, "v"), "v")
	if t == "" {
		return false
	}
	digits := 0
	for _, r := range t {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '.':
		default:
			return false
		}
	}
	return digits > 0
}

// wrouteIsPrice reports the ad-supported twin's suffix, which says nothing
// about which model this is.
func wrouteIsPrice(t string) bool {
	switch t {
	case "free", "trial", "promo", "basic":
		return true
	}
	return false
}

// wrouteMajor reduces a version token to its line: v4.1 -> v4, 5.3 -> 5, 4.6v
// -> 4. a dated build (0731, 2512) belongs to no line and drops out of the
// name, the same way a price suffix does.
func wrouteMajor(t string) (string, bool) {
	if !wrouteIsVersion(t) {
		return "", false
	}
	v, core := "", t
	if strings.HasPrefix(core, "v") {
		v, core = "v", core[1:]
	}
	core = strings.TrimSuffix(core, "v")
	if i := strings.IndexByte(core, '.'); i >= 0 {
		core = core[:i]
	} else if len(core) >= 3 {
		return "", true
	}
	if core == "" {
		return "", true
	}
	return v + core, true
}

// wrouteName is the wide route name of one exact id: the last path segment
// (vendor prefixes are catalog noise) without price suffixes and with every
// version cut to its line. so v4, v4.1 and v4-0731 meet at deepseek-v4-flash,
// and glm-5.3 lands on glm-5-flash, never on glm-4-flash.
func wrouteName(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	var out []string
	named := false
	for _, t := range wrouteTokens(id) {
		if wrouteIsPrice(t) {
			continue
		}
		line, version := wrouteMajor(t)
		if !version {
			out = append(out, t)
			named = true
			continue
		}
		if line != "" {
			out = append(out, line)
		}
	}
	if !named {
		return "" // "v4", "0731": a version alone names no model
	}
	return strings.Join(out, "-")
}

// wrouteUnder reports whether name is prefix itself or a token extension of it.
func wrouteUnder(name, prefix string) bool {
	return name == prefix || strings.HasPrefix(name, prefix+"-")
}

// wrouteTokenScore rates one name token against one id token: 1 exact, 1.1
// when the id extends it (v4 -> v4.1, a newer revision), 0.75 when the id
// names a coarser one, 0 not the same token.
func wrouteTokenScore(k, id string) float64 {
	if k == id {
		return 1
	}
	if strings.HasPrefix(id, k) {
		return 1.1
	}
	if strings.HasPrefix(k, id) {
		return 0.75
	}
	return 0
}

// wrouteScore ranks one id inside its name's row: name tokens matched in
// order, then the qualifiers the id carries (a version bump or a dated build
// reads newer, a -free twin is the same model bought for free) and finally
// length, so "smartest to dumbest" lands on the most specific spelling first.
func wrouteScore(name, id string) float64 {
	kt := strings.Split(name, "-")
	it := wrouteTokens(id)
	if len(kt) == 0 || len(it) == 0 {
		return 0
	}
	used := make([]bool, len(it))
	pos, parts, matched := 0, 0.0, 0
	for _, k := range kt {
		for j := pos; j < len(it); j++ {
			s := wrouteTokenScore(k, it[j])
			if s == 0 {
				continue
			}
			used[j], pos = true, j+1
			parts += s
			matched++
			break
		}
	}
	if matched == 0 {
		return 0
	}
	score := parts / float64(len(kt))
	for i, t := range it {
		if used[i] {
			continue
		}
		switch {
		case wrouteIsVersion(t):
			// a dated build or a version bump: newer and more specific
			score += 0.15
		case wrouteIsPrice(t):
			// the ad-supported twin of the same model: served last
			score -= 0.2
		case len(t) > 3:
			// a qualifier (preview, abliterated...) the name did not ask for
			score += 0.05
		}
	}
	return score + 0.005*float64(len(id))
}

// routeBackendRank is a backend's position in the provider priority order.
func routeBackendRank(backend string) int {
	for i, b := range routeBackends {
		if b == backend {
			return i
		}
	}
	return len(routeBackends)
}

// wrouteOrder sorts one row's attempts: provider blocks in route priority
// order, then free rows before paid ones and the cheapest paid row first
// inside a provider, with name confidence breaking the tie. provider rank is
// the heavy term: freepi is last even though every row it serves is free, so
// a paid uno row is always asked before an ad-supported twin.
func wrouteOrder(ms []routeModel, name string) []string {
	type cand struct {
		id    string
		rank  int
		class int
		price float64
		score float64
	}
	cs := make([]cand, 0, len(ms))
	seen := map[string]bool{}
	for _, m := range ms {
		full := m.backend + "/" + m.id
		if seen[full] {
			continue
		}
		seen[full] = true
		class, price := routeRank(full)
		cs = append(cs, cand{
			id:    full,
			rank:  routeBackendRank(m.backend),
			class: class,
			price: price,
			score: wrouteScore(name, m.id),
		})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].rank != cs[j].rank {
			return cs[i].rank < cs[j].rank
		}
		if cs[i].class != cs[j].class {
			return cs[i].class < cs[j].class
		}
		if cs[i].class == 2 && cs[i].price != cs[j].price {
			return cs[i].price < cs[j].price
		}
		if cs[i].score != cs[j].score {
			return cs[i].score > cs[j].score
		}
		return cs[i].id < cs[j].id
	})
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.id)
	}
	return out
}

// wrouteVersionParts reads an id's version as numbers: v4.1 -> [4 1], 5.3 ->
// [5 3], 4.6v -> [4 6]. a dated build (0731, 2512) is not a version line and
// reads as none.
func wrouteVersionParts(id string) []int {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	for _, t := range wrouteTokens(id) {
		line, version := wrouteMajor(t)
		if !version || line == "" {
			continue
		}
		core := strings.TrimSuffix(strings.TrimPrefix(t, "v"), "v")
		parts := make([]int, 0, 2)
		for _, s := range strings.Split(core, ".") {
			n, err := strconv.Atoi(s)
			if err != nil {
				return nil
			}
			parts = append(parts, n)
		}
		return parts
	}
	return nil
}

// wrouteCommonVersion is the longest version every member of a row shares: all
// 2.6 builds keep 2.6, while v4 and v4.1 together fall back to v4.
func wrouteCommonVersion(ms []routeModel) []int {
	var common []int
	for i, m := range ms {
		p := wrouteVersionParts(m.id)
		if i == 0 {
			common = p
			continue
		}
		n := 0
		for n < len(common) && n < len(p) && common[n] == p[n] {
			n++
		}
		common = common[:n]
	}
	return common
}

// wrouteLabel names a row after the version its models share: a row of 2.6
// builds lists as wroute/mimo-v2.6-flash, a row of v4 and v4.1 as
// wroute/deepseek-v4-flash. ms must be in a stable order.
func wrouteLabel(ms []routeModel) string {
	if len(ms) == 0 {
		return ""
	}
	common := wrouteCommonVersion(ms)
	if len(common) == 0 {
		return wrouteName(ms[0].id)
	}
	parts := make([]string, len(common))
	for i, n := range common {
		parts[i] = strconv.Itoa(n)
	}
	ver := strings.Join(parts, ".")
	id := ms[0].id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	var out []string
	named, put := false, false
	for _, t := range wrouteTokens(id) {
		if wrouteIsPrice(t) {
			continue
		}
		line, version := wrouteMajor(t)
		if !version {
			out = append(out, t)
			named = true
			continue
		}
		if line == "" || put {
			continue // a dated build, or a second version in one name
		}
		v := ""
		if strings.HasPrefix(t, "v") {
			v = "v"
		}
		out = append(out, v+ver)
		put = true
	}
	if !named || !put {
		return wrouteName(ms[0].id)
	}
	return strings.Join(out, "-")
}

// wrouteIndex is one built listing: rows maps a line to its ordered attempts,
// labels maps it to the id the row is listed under (the version every member
// shares), cat keeps the catalog for the fallback that resolves a name too
// small to list.
type wrouteIndex struct {
	rows   map[string][]string
	labels map[string]string
	cat    []routeModel
}

// buildWrouteIndex groups the catalog by line: every patch of one line meets
// at the line's name (deepseek-v4-flash) and nothing else is merged in. so
// glm-5 and glm-4 list separately, glm-5-flash never reaches glm-5, and
// claude-sonnet never becomes claude just because claude-haiku sits beside
// it. resolve still widens under a name for callers.
func buildWrouteIndex(cat []routeModel) *wrouteIndex {
	byName := map[string][]routeModel{}
	for _, m := range cat {
		if name := wrouteName(m.id); name != "" {
			byName[name] = append(byName[name], m)
		}
	}
	ix := &wrouteIndex{rows: map[string][]string{}, labels: map[string]string{}, cat: cat}
	for name, ms := range byName {
		if len(ms) < wrouteMinModels {
			continue
		}
		sort.SliceStable(ms, func(i, j int) bool {
			if ms[i].id != ms[j].id {
				return ms[i].id < ms[j].id
			}
			return ms[i].backend < ms[j].backend
		})
		ix.rows[name] = wrouteOrder(ms, name)
		ix.labels[name] = wrouteLabel(ms)
	}
	return ix
}

// resolve maps one requested key onto attempts: the row for its name, else
// every id under that name (a family too small to list, e.g. a single tuned
// build).
func (ix *wrouteIndex) resolve(key string) []string {
	name := wrouteName(key)
	if name == "" {
		return nil
	}
	if row, ok := ix.rows[name]; ok {
		return row
	}
	var sub []routeModel
	for _, m := range ix.cat {
		n := wrouteName(m.id)
		if n == "" || !wrouteUnder(n, name) {
			continue
		}
		// never widen into a family that has its own row: glm must not reach
		// glm-flash just because the plain glm family is too small to list.
		if n != name {
			if _, own := ix.rows[n]; own {
				continue
			}
		}
		sub = append(sub, m)
	}
	if len(sub) == 0 {
		return nil
	}
	return wrouteOrder(sub, name)
}

// wrouteCache holds the last built index per pool. /status polls at 1hz and
// every build walks the catalog, so it is reused for routeTableTTL. the entry
// is pinned to the config pointer, so a hot reload rebuilds at once.
var wrouteCache struct {
	sync.Mutex
	ix  *wrouteIndex
	cfg *appConfig
	p   *Pool
	at  time.Time
}

// wrouteIndexFor returns the cached index over the backends that can serve now.
func wrouteIndexFor(p *Pool) *wrouteIndex {
	c := cfg()
	wrouteCache.Lock()
	if ix := wrouteCache.ix; ix != nil && wrouteCache.p == p && wrouteCache.cfg == c && time.Since(wrouteCache.at) < routeTableTTL {
		wrouteCache.Unlock()
		return ix
	}
	wrouteCache.Unlock()
	cat, _ := routeCatalogAndTable(p)
	live := make([]routeModel, 0, len(cat))
	for _, m := range cat {
		if routeBackendLive(m.backend, p) {
			live = append(live, m)
		}
	}
	ix := buildWrouteIndex(live)
	wrouteCache.Lock()
	wrouteCache.ix, wrouteCache.p, wrouteCache.cfg, wrouteCache.at = ix, p, c, time.Now()
	wrouteCache.Unlock()
	return ix
}

// wrouteCandidates resolves wroute/<key> to exact "backend/id" attempts.
func wrouteCandidates(p *Pool, key string) []string {
	if !wrouteEnabled() {
		return nil
	}
	// the row already carries the ask order (wrouteOrder): provider ladder
	// first, then free/price/confidence inside each provider.
	return wrouteIndexFor(p).resolve(key)
}

// wrouteRow is one /status wroute entry: the id and the exact models behind
// it, in attempt order.
type wrouteRow struct {
	ID     string   `json:"id"`
	Models []string `json:"models"`
}

// wrouteSummary is the /status view behind status.show_wroute.
type wrouteSummary struct {
	Enabled bool        `json:"enabled"`
	Routes  []wrouteRow `json:"routes,omitempty"`
	Count   int         `json:"count"`
}

// wrouteRedundant reports whether a wide row only repeats an exact route row:
// same key, same models. that route id already covers the case, so a second
// id for it is listing noise.
func wrouteRedundant(models []string, exact map[string]string) bool {
	if len(models) == 0 || len(exact) == 0 || len(models) != len(exact) {
		return false
	}
	for _, b := range routeBackends {
		id, ok := exact[b]
		if !ok {
			continue
		}
		found := false
		for _, m := range models {
			if m == b+"/"+id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// wrouteSnapshot lists every name that actually widens, with the models it
// fans out over.
func wrouteSnapshot(p *Pool) *wrouteSummary {
	sum := &wrouteSummary{Enabled: wrouteEnabled()}
	if sum.Enabled {
		_, t := routeCatalogAndTable(p)
		ix := wrouteIndexFor(p)
		names := make([]string, 0, len(ix.rows))
		for name := range ix.rows {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			// the row states the attempt order, so it shows the same ordering
			// the request takes.
			models := ix.rows[name]
			label := ix.labels[name]
			if label == "" {
				label = name
			}
			if len(models) < wrouteMinModels || wrouteRedundant(models, t[label]) {
				continue
			}
			sum.Routes = append(sum.Routes, wrouteRow{ID: "wroute/" + label, Models: models})
		}
		sum.Count = len(sum.Routes)
	}
	return sum
}
