package pii

import (
	"regexp"
	"strings"
)

// deterministic recognizers: port of rampart heuristics.ts + validators.ts,
// plus secret shapes (gitleaks-style: keyword prefilter + regex).
// RE2 has no lookaheads, so boundary rules use char classes instead.

func luhnValid(digits string) bool {
	sum, dbl := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if dbl {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		dbl = !dbl
	}
	return sum%10 == 0
}

func validSSN(d string) bool {
	if len(d) != 9 {
		return false
	}
	area, group, serial := d[:3], d[3:5], d[5:]
	if area == "000" || area == "666" || area >= "900" {
		return false
	}
	return group != "00" && serial != "0000"
}

var digitRunRe = regexp.MustCompile(`\d(?:[ .-]?\d)*`)

// maxDigitWindows caps the per-rule window slide inside one digit run.
const maxDigitWindows = 4096

type digitRule struct {
	label   Label
	lengths []int
	check   func(string) bool
}

var digitRules = []digitRule{
	{LCreditCard, []int{19, 16, 15, 14, 13}, luhnValid},
	{LSSN, []int{9}, validSSN},
}

func detectDigitEntities(raw string) []Span {
	type run struct {
		digits string
		idx    []int
	}
	var runs []run
	for _, loc := range digitRunRe.FindAllStringIndex(raw, -1) {
		var d strings.Builder
		var ix []int
		for i := loc[0]; i < loc[1]; i++ {
			if c := raw[i]; c >= '0' && c <= '9' {
				d.WriteByte(c)
				ix = append(ix, i)
			}
		}
		if d.Len() > 0 {
			runs = append(runs, run{d.String(), ix})
		}
	}
	var out []Span
	for _, r := range runs {
		for _, rule := range digitRules {
			// slide rule-length windows inside longer runs: an ssn glued
			// to an extra digit (or a spaced card) must not bypass. the
			// window scan is capped: a pathological all-digit body (up to
			// the 10MB body limit) would otherwise slide millions of
			// windows per rule on the request path, and real identifiers
			// sit near the start of their run.
			best, bestS := -1, -1
			for _, l := range rule.lengths {
				if len(r.digits) < l {
					continue
				}
				limit := len(r.digits) - l + 1
				if limit > maxDigitWindows {
					limit = maxDigitWindows
				}
				for s := 0; s < limit; s++ {
					if rule.check(r.digits[s : s+l]) {
						best, bestS = l, s
						break
					}
				}
				if best > 0 {
					break
				}
			}
			if best <= 0 {
				continue
			}
			st := r.idx[bestS]
			en := r.idx[bestS+best-1] + 1
			out = append(out, Span{st, en, rule.label, 1, "heuristic", raw[st:en]})
			break
		}
	}
	return out
}

var textRules = []struct {
	label Label
	re    *regexp.Regexp
}{
	{LEmail, regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	// exclude backslash too: bodies are raw json, so a url directly before
	// an escaped quote (https://x/y\") would otherwise swallow the escape
	// and the surrogate replacement would leave the quote unescaped,
	// producing invalid json upstream.
	{LURL, regexp.MustCompile(`https?://[^\s<>"'\\\])}]+`)},
	{LURL, regexp.MustCompile(`www\.[A-Za-z0-9.\-]+\.[A-Za-z]{2,}(?:/[^\s<>"'\\\])}]*)?`)},
	{LIPAddress, regexp.MustCompile(`(?:(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])`)},
	{LIPAddress, regexp.MustCompile(`(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}`)},
	// full + compressed ipv6 (zone ids excluded: trailing %eth0 unmasks).
	{LIPAddress, regexp.MustCompile(`(?:[0-9A-Fa-f]{1,4}:){7}[0-9A-Fa-f]{1,4}`)},
	{LIPAddress, regexp.MustCompile(`(?:[0-9A-Fa-f]{1,4}:){1,6}:[0-9A-Fa-f]{0,4}`)},
	// any three-segment base64url token, not just claim-bearing jwts.
	{LJWT, regexp.MustCompile(`eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_\-]+`)},
	{LJWT, regexp.MustCompile(`[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{LPrivateKey, regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----[\s\S]*?-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`)},
	{LIBAN, regexp.MustCompile(`(?:^|[^A-Za-z0-9])[A-Z]{2}[0-9]{2} ?(?:[0-9A-Z]{4} ?){2,6}[0-9A-Z]{0,4}(?:$|[^A-Za-z0-9])`)},
	{LPhone, regexp.MustCompile(`(?:\+?1[-. ]?)?\(?[0-9]{3}\)?[-. ][0-9]{3}[-. ][0-9]{4}`)},
	// bare 10-digit with area-code guard (2-9 lead both halves): +... math
	// without digit runs stays out, plain numbers get masked.
	{LPhone, regexp.MustCompile(`(?:^|[^0-9+])[2-9][0-9]{2}[2-9][0-9]{6}(?:$|[^0-9])`)},
	{LPhone, regexp.MustCompile(`\+[0-9][0-9 .\-()]{6,}[0-9]`)},
}

func detectTextEntities(raw string) []Span {
	var out []Span
	for _, r := range textRules {
		for _, loc := range r.re.FindAllStringIndex(raw, -1) {
			st, en := loc[0], loc[1]
			// boundary-anchored rules (iban, bare phone) consume guard
			// chars at the edges: trim back to the value alphabet so
			// masking never eats json syntax. interior chars are value
			// content and stay.
			if r.label == LIBAN {
				for st < en && !ibanByte(raw[st]) {
					st++
				}
				for en > st && !ibanByte(raw[en-1]) {
					en--
				}
			}
			if r.label == LPhone {
				for st < en && !phoneByte(raw[st]) {
					st++
				}
				for en > st && !phoneByte(raw[en-1]) {
					en--
				}
			}
			if st >= en {
				continue
			}
			out = append(out, Span{st, en, r.label, 1, "heuristic", raw[st:en]})
		}
	}
	return out
}

func ibanByte(c byte) bool {
	return '0' <= c && c <= '9' || 'A' <= c && c <= 'Z' || c == ' '
}

func phoneByte(c byte) bool {
	return '0' <= c && c <= '9' || c == '+' || c == '(' || c == ')' || c == '-' || c == '.' || c == ' '
}

// secret keyword prefilter: only run the expensive generic-key regexes when
// a keyword fires nearby (gitleaks-style port rules).
var secretKeywords = []string{
	"api_key", "apikey", "api-key", "secret", "token", "password", "passwd",
	"pwd", "auth", "credential", "private_key", "privatekey", "access_key",
	"secret_key", "client_secret", "aws_", "ghp_", "gho_", "github_token",
	"bearer", "sk-", "xox", "openai", "anthropic", "nvapi-",
}

var (
	genericKeyRe = regexp.MustCompile(`(?i)(?:api[_-]?key|secret|token|password|passwd|pwd|auth[_-]?token|access[_-]?key|client[_-]?secret)\s*[:=]\s*['"]?([A-Za-z0-9_\-./+]{12,})['"]?`)
	nvapiRe      = regexp.MustCompile(`nvapi-[A-Za-z0-9_\-]{16,}`)
	ghpRe        = regexp.MustCompile(`gh[op]_[A-Za-z0-9]{20,}`)
	ghsRe        = regexp.MustCompile(`ghs_[A-Za-z0-9]{20,}`)
	ghrRe        = regexp.MustCompile(`ghr_[A-Za-z0-9]{20,}`)
	githubPatRe  = regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)
	akiaRe       = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	skRe         = regexp.MustCompile(`sk-[A-Za-z0-9]{16,}`)
	skAntRe      = regexp.MustCompile(`sk-ant-[A-Za-z0-9\-]{16,}`)
	xoxRe        = regexp.MustCompile(`xox[bpas]-[A-Za-z0-9\-]{10,}`)
	slackRe      = regexp.MustCompile(`xox[abpos]-[0-9]+-[0-9]+-[A-Za-z0-9]+`)
)

func hasSecretKeyword(raw string) bool {
	low := strings.ToLower(raw)
	for _, k := range secretKeywords {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// direct secret shapes: matched with no keyword gate. add new shapes here,
// not behind the generic gate.
var directSecretRes = []*regexp.Regexp{
	nvapiRe, ghpRe, ghsRe, ghrRe, githubPatRe, akiaRe, skRe, skAntRe, xoxRe, slackRe,
}

func detectSecrets(raw string) []Span {
	var out []Span
	for _, re := range directSecretRes {
		for _, loc := range re.FindAllStringIndex(raw, -1) {
			out = append(out, Span{loc[0], loc[1], LAPIKey, 1, "secret", raw[loc[0]:loc[1]]})
		}
	}
	if !hasSecretKeyword(raw) {
		return out
	}
	// sliding window: a keyword and secret in adjacent paragraphs must
	// still gate together. run the generic match on overlapping 4k windows
	// striding 2k so no keyword-secret pair splits across a boundary.
	const win, stride = 4096, 2048
	seen := map[[2]int]bool{}
	for base := 0; base < len(raw); base += stride {
		end := base + win
		if end > len(raw) {
			end = len(raw)
		}
		frag := raw[base:end]
		if !hasSecretKeyword(frag) {
			if end == len(raw) {
				break
			}
			continue
		}
		for _, m := range genericKeyRe.FindAllStringSubmatchIndex(frag, -1) {
			if len(m) < 4 || m[2] < 0 {
				continue
			}
			st, en := base+m[2], base+m[3]
			if seen[[2]int{st, en}] {
				continue
			}
			seen[[2]int{st, en}] = true
			out = append(out, Span{st, en, LAPIKey, 0.9, "secret", raw[st:en]})
		}
		if end == len(raw) {
			break
		}
	}
	return out
}

func detectHeuristics(raw string) []Span {
	var out []Span
	out = append(out, detectDigitEntities(raw)...)
	out = append(out, detectTextEntities(raw)...)
	out = append(out, detectSecrets(raw)...)
	return out
}
