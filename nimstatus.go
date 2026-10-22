// nimstatus.go: the /status renderers behind ./nim status, status-tail,
// logs and models. plain lines for scripts, a bubbletea dashboard for tail.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// styles mirror nimstatus.py's palette.
var (
	cDim    = lipgloss.NewStyle().Faint(true)
	cBold   = lipgloss.NewStyle().Bold(true)
	cGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("108"))
	cRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	cYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("221"))
)

var provColor = map[string]lipgloss.Style{
	"opencode":   lipgloss.NewStyle().Foreground(lipgloss.Color("68")),
	"freepi":     lipgloss.NewStyle().Foreground(lipgloss.Color("141")),
	"antseed":    lipgloss.NewStyle().Foreground(lipgloss.Color("67")),
	"nvidia":     lipgloss.NewStyle().Foreground(lipgloss.Color("36")),
	"nous":       lipgloss.NewStyle().Foreground(lipgloss.Color("48")),
	"uno":        lipgloss.NewStyle().Foreground(lipgloss.Color("220")),
	"openrouter": lipgloss.NewStyle().Foreground(lipgloss.Color("69")),
	"route":      lipgloss.NewStyle().Foreground(lipgloss.Color("176")),
}

func provStyle(p string) lipgloss.Style {
	if s, ok := provColor[p]; ok {
		return s
	}
	return lipgloss.NewStyle()
}

// ---- small formatters ----

func fmtInt(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, s[i])
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func fmtTok(n int) string {
	f := float64(n)
	switch {
	case n == 0:
		return "0"
	case f >= 1e9:
		return fmt.Sprintf("%.1fB", f/1e9)
	case f >= 1e6:
		return fmt.Sprintf("%.1fM", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.1fk", f/1e3)
	}
	return strconv.Itoa(n)
}

func fmtMs(ms int) string {
	switch {
	case ms <= 0:
		return "·"
	case ms >= 10000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms >= 1000:
		return fmt.Sprintf("%.2fs", float64(ms)/1000)
	}
	return strconv.Itoa(ms) + "ms"
}

func fmtPct(p float64) string {
	if p < 10 {
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%d%%", int(math.Round(p)))
}

// usageBar draws a pct meter, green under 50, yellow under 80, red above.
func usageBar(pct float64, width int) string {
	if width < 1 {
		width = 1
	}
	filled := int(math.Round(pct / 100 * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	var b strings.Builder
	for i := 0; i < width; i++ {
		if i >= filled {
			b.WriteString(cDim.Render("░"))
			continue
		}
		r := float64(i) / math.Max(float64(width-1), 1)
		switch {
		case r >= 0.8:
			b.WriteString(cRed.Render("█"))
		case r >= 0.5:
			b.WriteString(cYellow.Render("█"))
		default:
			b.WriteString(cGreen.Render("█"))
		}
	}
	return b.String()
}

func padR(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func padL(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// healthCell is the ✓/X/· marker beside a model id.
func healthCell(h map[string]modelHealth, model string) string {
	m, ok := h[model]
	if !ok {
		return cDim.Render("·")
	}
	switch m.State {
	case "working":
		return cGreen.Render("✓")
	case "rate_limited":
		return cYellow.Render("X")
	case "dead":
		return cRed.Render("X")
	}
	return cDim.Render("·")
}

// modelTotals falls back the way python did: exact, bare, then opencode/bare.
func modelTotals(bm map[string]usageTotals, model string) (usageTotals, bool) {
	if t, ok := bm[model]; ok {
		return t, true
	}
	bare := model
	if i := strings.Index(bare, "/"); i >= 0 {
		bare = bare[i+1:]
	}
	if t, ok := bm[bare]; ok {
		return t, true
	}
	t, ok := bm["opencode/"+bare]
	return t, ok
}

func benchSorted(rows []benchRow) []benchRow {
	out := make([]benchRow, len(rows))
	copy(out, rows)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Intel != out[j].Intel {
			return out[i].Intel > out[j].Intel
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func benchLine(b *benchSummary) string {
	if b == nil || !b.Enabled || len(b.Models) == 0 {
		return ""
	}
	best := benchSorted(b.Models)[0]
	return fmt.Sprintf("benchmarks %d scored (%d catalog + %d unlisted) · %d aa models · best %s %.1f",
		b.Matched+b.Served, b.Matched, b.Served, b.AAModels, best.ID, best.Intel)
}

// ---- /status fetch ----

func statusGet(port, path string) ([]byte, error) {
	req, err := http.NewRequest("GET", "http://127.0.0.1:"+port+path, nil)
	if err != nil {
		return nil, err
	}
	if t := os.Getenv("NIM_AUTH"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func nimFetchStatus(port string) (*StatusResponse, error) {
	body, err := statusGet(port, "/status")
	if err != nil {
		return nil, err
	}
	var d StatusResponse
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ---- usage jsonl ----

type nimStats struct {
	allReq, allTok     int64
	todayReq, todayTok int64
	since              string
}

// statsCache keeps the last file identity and offset so a 2s refresh reads
// only the bytes appended since the previous tick.
var statsCache struct {
	path  string
	dev   uint64
	ino   uint64
	day   string
	size  int64
	off   int64
	st    nimStats
	valid bool
}

type usageLine struct {
	TS         string `json:"ts"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
	Total      int64  `json:"total_tokens"`
}

func usageTotalsFor(path string) nimStats {
	c := &statsCache
	today := time.Now().Format("2006-01-02")
	reset := func() {
		c.path, c.off, c.day, c.st, c.valid = path, 0, today, nimStats{}, false
	}
	if !c.valid || c.path != path || c.day != today {
		reset()
		c.path, c.day, c.valid = path, today, true
	}
	f, err := os.Open(path)
	if err != nil {
		return c.st
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return c.st
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		dev, ino := uint64(st.Dev), uint64(st.Ino)
		if c.dev != 0 && (dev != c.dev || ino != c.ino) {
			c.off, c.st = 0, nimStats{}
		}
		c.dev, c.ino = dev, ino
	}
	// a shrink means the file was truncated in place.
	if fi.Size() < c.off {
		c.off, c.st = 0, nimStats{}
	}
	c.size = fi.Size()
	if _, err := f.Seek(c.off, io.SeekStart); err != nil {
		return c.st
	}
	br := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil || len(line) == 0 || line[len(line)-1] != '\n' {
			break // keep the offset before any partial line
		}
		c.off += int64(len(line))
		var u usageLine
		if json.Unmarshal(line, &u) != nil {
			continue
		}
		n := u.Total
		if n == 0 {
			n = u.Prompt + u.Completion
		}
		c.st.allReq++
		c.st.allTok += n
		c.st.todayReq++
		c.st.todayTok += n
		if c.st.since == "" && len(u.TS) >= 10 {
			c.st.since = u.TS[:10]
		}
	}
	return c.st
}

// ---- plain renderers ----

func nimPrintStatus(port string) {
	d, err := nimFetchStatus(port)
	if err != nil {
		fmt.Printf("(status unavailable: %v)\n", err)
		return
	}
	st := usageTotalsFor(filepath.Join(newNimEnv().dir, "nim-usage.jsonl"))

	line := fmt.Sprintf("NimRoute %s | up %s | load %d/%d", d.Version, d.Uptime, d.Concurrent, d.SemLimit)
	if len(d.Keys) > 0 {
		avail := 0
		for _, k := range d.Keys {
			if k.Available {
				avail++
			}
		}
		line += fmt.Sprintf(" | keys %d/%d", avail, len(d.Keys))
	}
	fmt.Println(line)

	fmt.Printf("opencode %d active · nvidia %d active · lanes %d/%d\n",
		d.ConcurrentZen, d.ConcurrentNV, len(d.ZenLanes), d.LaneCap)

	var served int
	if d.Usage != nil {
		served = d.Usage.Prompt + d.Usage.Completion
	}
	if served == 0 {
		for _, l := range d.ZenLanes {
			served += l.Prompt + l.Completion
		}
	}
	fmt.Printf("%s tok served", fmtTok(served))
	if st.allReq > 0 {
		fmt.Printf(" · %s req logged", fmtInt(int(st.allReq)))
	}
	fmt.Println()

	if d.Opencode != nil && len(d.Opencode.Models) > 0 {
		fmt.Printf("models %d free\n", len(d.Opencode.Models))
	}
	if d.Pool != nil {
		fmt.Printf("pool %d verified, %d candidates, %d dropped\n",
			d.Pool.Verified, d.Pool.Cands, d.Pool.Dropped)
	}
	if bl := benchLine(d.Benchmarks); bl != "" {
		fmt.Println(bl)
	}

	if st.allReq > 0 {
		fmt.Printf("today %s req, %s tok · all-time %s req, %s tok",
			fmtInt(int(st.todayReq)), fmtTok(int(st.todayTok)),
			fmtInt(int(st.allReq)), fmtTok(int(st.allTok)))
		if st.since != "" {
			fmt.Printf(" since %s", st.since)
		}
		fmt.Println()
	}

	if d.Freepi != nil && len(d.Freepi.Accounts) > 0 {
		for _, a := range d.Freepi.Accounts {
			l := fmt.Sprintf("freepi:%s %s req, %s tok", a.Name,
				fmtInt(a.Requests), fmtTok(int(a.Prompt+a.Completion)))
			if a.Inflight {
				l += " (streaming)"
			}
			if a.ReqToday > 0 {
				l += fmt.Sprintf(" · %d today", a.ReqToday)
			}
			fmt.Println(l)
		}
	}

	if d.Antseed != nil {
		l := fmt.Sprintf("antseed %d peers, %d offers", d.Antseed.Peers, d.Antseed.Offers)
		if len(d.Antseed.Models) > 0 {
			l += ": " + strings.Join(prefixed("antseed/", d.Antseed.Models), ", ")
		} else {
			l += " (discovering)"
		}
		if d.Antseed.StaleFor != "" {
			l += " · stale " + d.Antseed.StaleFor
		}
		fmt.Println(l)
	}

	if d.Route != nil && d.Route.Enabled {
		var lat []string
		var names []string
		for b := range d.Route.Latency {
			names = append(names, b)
		}
		sort.Strings(names)
		for _, b := range names {
			lat = append(lat, b+" "+d.Route.Latency[b]+"ms")
		}
		latstr := "no samples"
		if len(lat) > 0 {
			latstr = strings.Join(lat, ", ")
		}
		ids := make([]string, 0, len(d.Route.Routes))
		for i, r := range d.Route.Routes {
			if i == 8 {
				ids = append(ids, "…")
				break
			}
			ids = append(ids, strings.TrimPrefix(r.ID, "route/"))
		}
		fmt.Printf("route [%s] %d ids: %s\n", latstr, d.Route.Count, strings.Join(ids, ", "))
	}
}

func prefixed(prefix string, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.Contains(id, "/") {
			out = append(out, id)
		} else {
			out = append(out, prefix+id)
		}
	}
	return out
}

// ---- models ----

func nimModels(e nimEnv, n int) error {
	d, err := nimFetchStatus(e.port)
	if err != nil {
		return err
	}
	b := d.Benchmarks
	switch {
	case b == nil || !b.Enabled:
		fmt.Println("benchmarks disabled (benchmarks.enabled: false)")
		return nil
	case len(b.Models) == 0:
		msg := b.Error
		if msg == "" {
			msg = "still fetching"
		}
		fmt.Println("no benchmark scores yet: " + msg)
		return nil
	}
	rows := benchSorted(b.Models)
	limit := n
	if limit <= 0 || limit > len(rows) {
		limit = len(rows)
	}
	fmt.Printf("%-5s  %-34s %-8s %7s  %6s\n", "intel", "model", "backend", "ctx", "maxout")
	for _, r := range rows[:limit] {
		id := r.ID
		if lipgloss.Width(id) > 34 {
			id = ansi.Truncate(id, 33, "…")
		}
		row := fmt.Sprintf("%5.1f  %-34s %-8s %7s  %6s", r.Intel, id, r.Backend,
			dashZero(r.Ctx), dashZero(r.MaxOut))
		if r.Match != "" && r.Match != "exact" {
			row = cDim.Render(row)
		}
		fmt.Println(row)
	}
	if limit < len(rows) {
		fmt.Printf("%d more models · nim models %d\n", len(rows)-limit, len(rows))
	}
	if bl := benchLine(b); bl != "" {
		fmt.Println()
		fmt.Println(bl)
	}
	return nil
}

func dashZero(n int) string {
	if n == 0 {
		return "·"
	}
	return fmtInt(n)
}

// ---- logs ----

func nimLogs(e nimEnv, n int) error {
	lines, err := tailFile(e.logFile, n)
	if err != nil {
		return err
	}
	for _, ln := range lines {
		ln = proxyCredRE.ReplaceAllString(ln, "://***@")
		switch {
		case strings.Contains(ln, "!!"):
			fmt.Println(cRed.Render(ln))
		case strings.Contains(ln, "retry") || strings.Contains(ln, "429") || strings.Contains(ln, "403"):
			fmt.Println(cYellow.Render(ln))
		default:
			fmt.Println(cDim.Render(ln))
		}
	}
	return nil
}

// ---- bubbletea dashboard ----

type dashTickMsg struct{}

type dashMsg struct {
	d   *StatusResponse
	st  nimStats
	err error
}

type dashModel struct {
	port, dir string
	w, h      int
	top       int
	d         *StatusResponse
	st        nimStats
	err       error
	at        time.Time
}

func nimStatusTail(e nimEnv) {
	m := &dashModel{port: e.port, dir: e.dir}
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		nimFatal("dashboard", err)
	}
}

func dashLoad(port, dir string) tea.Cmd {
	return func() tea.Msg {
		d, err := nimFetchStatus(port)
		if err != nil {
			return dashMsg{err: err}
		}
		return dashMsg{d: d, st: usageTotalsFor(filepath.Join(dir, "nim-usage.jsonl"))}
	}
}

func dashTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return dashTickMsg{} })
}

func (m *dashModel) Init() tea.Cmd {
	return tea.Batch(dashLoad(m.port, m.dir), dashTick())
}

func (m *dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case dashTickMsg:
		return m, tea.Batch(dashLoad(m.port, m.dir), dashTick())
	case dashMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err, m.d, m.st, m.at = nil, msg.d, msg.st, time.Now()
	case tea.KeyMsg:
		page := m.bodyHeight()
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "up", "k":
			m.top--
		case "down", "j":
			m.top++
		case "pgup", "b":
			m.top -= page
		case "pgdown", "f", " ":
			m.top += page
		case "home", "g":
			m.top = 0
		case "end", "G":
			m.top = 1 << 30
		}
		if m.top < 0 {
			m.top = 0
		}
	}
	return m, nil
}

func (m *dashModel) bodyHeight() int {
	h := m.h - 3
	if h < 5 {
		h = 5
	}
	return h
}

func (m *dashModel) View() string {
	if m.err != nil {
		return cRed.Render("nimroute: "+m.err.Error()) + "\n\n" + cDim.Render("[q] quit")
	}
	if m.d == nil {
		return cDim.Render("loading…")
	}
	lines := m.render()
	body := m.bodyHeight()
	max := len(lines) - body
	if max < 0 {
		max = 0
	}
	if m.top > max {
		m.top = max
	}
	end := m.top + body
	if end > len(lines) {
		end = len(lines)
	}
	shown := make([]string, 0, body)
	for _, ln := range lines[m.top:end] {
		shown = append(shown, ansi.Truncate(ln, m.w, "…"))
	}
	foot := cDim.Render("[q] quit  [↑↓/jk] scroll  [pgup/pgdn] page  refresh 2s")
	if max > 0 {
		foot += cDim.Render(fmt.Sprintf("  %d-%d/%d", m.top+1, end, len(lines)))
	}
	if !m.at.IsZero() {
		foot += cDim.Render("  · " + m.at.Format("15:04:05"))
	}
	return strings.Join(shown, "\n") + "\n\n" + foot
}

func (m *dashModel) render() []string {
	d, st := m.d, m.st
	var out []string

	head := cBold.Render("NimRoute " + d.Version)
	head += cDim.Render("  up " + d.Uptime + "  ")
	head += fmt.Sprintf("load %d/%d", d.Concurrent, d.SemLimit)
	if len(d.Keys) > 0 {
		avail := 0
		for _, k := range d.Keys {
			if k.Available {
				avail++
			}
		}
		head += fmt.Sprintf("  keys %d/%d", avail, len(d.Keys))
	}
	out = append(out, head)

	sum := fmt.Sprintf("opencode %d active · nvidia %d active · lanes %d/%d",
		d.ConcurrentZen, d.ConcurrentNV, len(d.ZenLanes), d.LaneCap)
	var served int
	if d.Usage != nil {
		served = d.Usage.Prompt + d.Usage.Completion
	}
	if served == 0 {
		for _, l := range d.ZenLanes {
			served += l.Prompt + l.Completion
		}
	}
	sum += " · " + fmtTok(served) + " tok served"
	out = append(out, cDim.Render(sum), "")

	out = append(out, m.modelLines()...)
	out = append(out, m.laneLines()...)

	var tail []string
	if bl := benchLine(d.Benchmarks); bl != "" {
		tail = append(tail, cDim.Render(bl))
	}
	if d.Pool != nil {
		p := fmt.Sprintf("pool %d verified · %d candidates · %d dropped · %d 429 · %d dead",
			d.Pool.Verified, d.Pool.Cands, d.Pool.Dropped, d.Pool.RateLimited, d.Pool.Dead)
		if d.Pool.NextRefresh != "" {
			p += " · next refresh " + d.Pool.NextRefresh
		}
		tail = append(tail, cDim.Render(p))
	}
	if d.Freepi != nil && len(d.Freepi.Accounts) > 0 {
		for _, a := range d.Freepi.Accounts {
			l := provStyle("freepi").Render("freepi:"+a.Name) + " "
			l += fmt.Sprintf("%s req, %s tok", fmtInt(a.Requests), fmtTok(int(a.Prompt+a.Completion)))
			if a.Inflight {
				l += cGreen.Render("  streaming")
			}
			if a.Credits > 0 {
				l += cDim.Render(fmt.Sprintf("  %.0f cr", a.Credits))
			}
			if a.SpentPct > 0 {
				l += "  " + usageBar(a.SpentPct, 10) + " " + fmtPct(a.SpentPct)
			}
			tail = append(tail, l)
		}
	}
	if d.Antseed != nil {
		l := provStyle("antseed").Render("antseed")
		l += fmt.Sprintf(" %d peers, %d offers", d.Antseed.Peers, d.Antseed.Offers)
		if len(d.Antseed.Models) > 0 {
			l += cDim.Render("  " + strings.Join(d.Antseed.Models, ", "))
		} else {
			l += cDim.Render("  discovering…")
		}
		if d.Antseed.StaleFor != "" {
			l += cDim.Render("  stale " + d.Antseed.StaleFor)
		}
		tail = append(tail, l)
	}
	if st.allReq > 0 {
		l := fmt.Sprintf("today %s req, %s tok", fmtInt(int(st.todayReq)), fmtTok(int(st.todayTok)))
		l += cDim.Render(fmt.Sprintf(" · all-time %s req, %s tok", fmtInt(int(st.allReq)), fmtTok(int(st.allTok))))
		if st.since != "" {
			l += cDim.Render(" since " + st.since)
		}
		tail = append(tail, l)
	}
	if len(tail) > 0 {
		out = append(out, "")
		out = append(out, tail...)
	}
	return out
}

func (m *dashModel) modelLines() []string {
	d := m.d
	var rows [][]string
	cols := []colSpec{{"", false}, {"model", false}, {"req", true}, {"tok", true}, {"tok/s", true}}

	add := func(prov, id string) {
		t, ok := modelTotals(nil2(d), id)
		req, tok, rate := "·", "·", "·"
		if ok {
			req = fmtInt(int(t.Requests))
			tok = fmtTok(int(t.Prompt + t.Completion))
			if t.DurationMs > 0 && t.Completion > 0 {
				rate = fmtTok(int(float64(t.Completion) / (float64(t.DurationMs) / 1000)))
				rate += "/s"
			}
			if t.Errors > 0 {
				req += cDim.Render(fmt.Sprintf(" (%d!)", t.Errors))
			}
		}
		rows = append(rows, []string{
			healthCell(d.Health, id),
			provStyle(prov).Render(id),
			req, tok, rate,
		})
	}

	if d.Opencode != nil {
		for _, id := range d.Opencode.Models {
			add("opencode", "opencode/"+id)
		}
	}
	if d.Freepi != nil {
		seen := map[string]bool{}
		for _, a := range d.Freepi.Accounts {
			for _, id := range a.Models {
				if !strings.Contains(id, "/") {
					id = "freeepi/" + id
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				add("freepi", id)
			}
		}
	}
	if d.Antseed != nil {
		for _, id := range prefixed("antseed/", d.Antseed.Models) {
			add("antseed", id)
		}
	}
	if d.Nous != nil && d.Nous.Enabled {
		for _, id := range prefixed("nous/", d.Nous.Models) {
			add("nous", id)
		}
	}
	if d.Uno != nil && d.Uno.Enabled {
		for _, id := range prefixed("uno/", d.Uno.Models) {
			add("uno", id)
		}
	}
	if d.OpenRouter != nil && d.OpenRouter.Enabled {
		for _, id := range prefixed("openrouter/", d.OpenRouter.Models) {
			add("openrouter", id)
		}
	}
	if d.Route != nil && d.Route.Enabled {
		for _, r := range d.Route.Routes {
			id := r.ID
			via := cDim.Render(strings.Join(r.Via, ","))
			rows = append(rows, []string{
				cDim.Render("·"),
				provStyle("route").Render(id) + cDim.Render("  "+plainVia(r.Via)),
				"", "", via,
			})
		}
	}

	if len(rows) == 0 {
		return []string{cDim.Render("no models (pool still starting)"), ""}
	}
	out := drawTable(cols, rows)
	out = append(out, "")
	return out
}

func plainVia(via []string) string {
	return strings.Join(via, ",")
}

// nil2 returns the per-model usage totals for the current status snapshot.
func nil2(d *StatusResponse) map[string]usageTotals {
	if d.Usage == nil {
		return nil
	}
	return d.Usage.ByModel
}

type colSpec struct {
	h     string
	right bool
}

// drawTable pads every column to its widest cell and trims trailing blanks.
func drawTable(cols []colSpec, rows [][]string) []string {
	w := make([]int, len(cols))
	for i, c := range cols {
		w[i] = lipgloss.Width(c.h)
	}
	for _, r := range rows {
		for i := range cols {
			if i < len(r) {
				if n := lipgloss.Width(r[i]); n > w[i] {
					w[i] = n
				}
			}
		}
	}
	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = cell(c.h, w[i], c.right)
	}
	out := []string{cDim.Render(strings.TrimRight(strings.Join(head, "  "), " "))}
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i := range cols {
			v := ""
			if i < len(r) {
				v = r[i]
			}
			cells[i] = cell(v, w[i], cols[i].right)
		}
		out = append(out, strings.TrimRight(strings.Join(cells, "  "), " "))
	}
	return out
}

func cell(v string, w int, right bool) string {
	if right {
		return padL(v, w)
	}
	return padR(v, w)
}

func (m *dashModel) laneLines() []string {
	lanes := append([]laneStatus(nil), m.d.ZenLanes...)
	if len(lanes) == 0 {
		return nil
	}
	exits := map[string]poolExit{}
	if m.d.Pool != nil {
		for _, x := range m.d.Pool.Exits {
			exits[x.Proxy] = x
		}
	}
	sort.SliceStable(lanes, func(i, j int) bool {
		if lanes[i].Requests != lanes[j].Requests {
			return lanes[i].Requests > lanes[j].Requests
		}
		return lanes[i].ID < lanes[j].ID
	})

	cols := []colSpec{{"lane", false}, {"exit", false}, {"cc", false}, {"ttfb", true},
		{"req", true}, {"429", true}, {"avg turn", true}, {"tokens", true}}
	anyCC := false
	for _, l := range lanes {
		if l.Country != "" {
			anyCC = true
		}
	}
	var rows [][]string
	var tokSum int64
	for i, l := range lanes {
		if i == 8 {
			break
		}
		lane := l.ID
		if lipgloss.Width(lane) > 18 {
			lane = ansi.Truncate(lane, 17, "…")
		}
		exit, cc, ttfb := "direct", "", "·"
		if e, ok := exits[l.Proxy]; ok {
			exit = e.Proxy
			if lipgloss.Width(exit) > 24 {
				exit = ansi.Truncate(exit, 23, "…")
			}
			cc = e.Country
			ttfb = fmtMs(e.Latency)
		}
		r429 := "·"
		if l.RateLimit > 0 {
			r429 = cYellow.Render(strconv.Itoa(l.RateLimit))
		}
		rows = append(rows, []string{
			lane, exit, cc, ttfb, fmtInt(l.Requests), r429,
			fmtMs(l.AvgMs), fmtTok(l.Prompt + l.Completion),
		})
		tokSum += int64(l.Prompt + l.Completion)
	}
	if !anyCC {
		c := append([]colSpec{}, cols[:2]...)
		c = append(c, cols[3:]...)
		cols = c
		for i := range rows {
			rows[i] = append(rows[i][:2], rows[i][3:]...)
		}
	}
	out := drawTable(cols, rows)
	if len(lanes) > 8 {
		out = append(out, cDim.Render(fmt.Sprintf("%d more lanes · %s tok across %d lanes",
			len(lanes)-8, fmtTok(int(tokSum)), len(lanes))))
	}
	out = append(out, "")
	return out
}
