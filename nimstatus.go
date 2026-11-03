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
	"github.com/charmbracelet/x/term"
	"gopkg.in/yaml.v3"
)

// styles mirror nimstatus.py's palette.
var (
	cDim     = lipgloss.NewStyle().Faint(true)
	cWhite   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	cGreen   = lipgloss.NewStyle().Foreground(lipgloss.Color("108"))
	cSpring  = lipgloss.NewStyle().Foreground(lipgloss.Color("85"))
	cRed     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	cYellow  = lipgloss.NewStyle().Foreground(lipgloss.Color("221"))
	cMagenta = lipgloss.NewStyle().Foreground(lipgloss.Color("201"))
	cSlate   = lipgloss.NewStyle().Foreground(lipgloss.Color("68"))
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
	"wroute":     lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
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

// healthCell is the ✓/X/- marker beside a model id.
func healthCell(h map[string]modelHealth, model string) string {
	m, ok := h[model]
	if !ok {
		return cDim.Render("-")
	}
	switch m.State {
	case "working":
		return cGreen.Render("✓")
	case "rate_limited":
		return cYellow.Render("X")
	case "dead":
		return cRed.Render("X")
	}
	return cDim.Render("-")
}

// modelCore is python's _model_core: bare id, dots folded, free suffix trimmed.
func modelCore(mid string) string {
	if i := strings.LastIndex(mid, "/"); i >= 0 {
		mid = mid[i+1:]
	}
	core := strings.ToLower(strings.ReplaceAll(mid, ".", "-"))
	for _, suf := range []string{":free", "-free"} {
		if strings.HasSuffix(core, suf) && len(core) > len(suf) {
			core = core[:len(core)-len(suf)]
		}
	}
	return core
}

// benchLookup resolves a serving id to a bench row: exact key first, then the
// bare model core, marked alias so the score renders dim.
func benchLookup(b *benchSummary) func(string) *benchRow {
	if b == nil {
		return func(string) *benchRow { return nil }
	}
	byID := make(map[string]*benchRow, len(b.Models))
	for i := range b.Models {
		byID[b.Models[i].ID] = &b.Models[i]
	}
	return func(full string) *benchRow {
		if r, ok := byID[full]; ok {
			return r
		}
		core := modelCore(full)
		for _, r := range b.Models {
			if modelCore(r.ID) == core {
				cp := r
				cp.Match = "alias"
				return &cp
			}
		}
		return nil
	}
}

func scoreCell(r *benchRow) string {
	if r == nil || r.Intel == 0 {
		return cDim.Render("·")
	}
	if r.Match == "exact" {
		return cWhite.Render(fmt.Sprintf("%.1f", r.Intel))
	}
	return cDim.Render(fmt.Sprintf("%.1f", r.Intel))
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
	// todayByModel is today's per-model work, so the model table can show
	// real numbers right after a restart: the server's in-memory totals
	// start at zero, and a table of zeros under a five-digit "today"
	// footer reads like the dashboard is broken.
	todayByModel map[string]modelUse
}

type modelUse struct {
	req, tok, comp, dur int64
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
	Model      string `json:"model"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
	Total      int64  `json:"total_tokens"`
	DurationMs int64  `json:"duration_ms"`
}

func usageTotalsFor(path string) nimStats {
	c := &statsCache
	today := time.Now().Format("2006-01-02")
	reset := func() {
		c.path, c.off, c.day, c.st, c.valid = path, 0, today, nimStats{}, false
	}
	if !c.valid || c.path != path {
		reset()
		c.path, c.day, c.valid = path, today, true
	} else if c.day != today {
		// the day rolled over under a running dashboard: every row already
		// read is yesterday's, so keep the offset and the all-time totals and
		// only restart today's window. re-reading the whole log here cost
		// seconds (134MB, 485k lines) once a day, mid-frame.
		c.day, c.st.todayReq, c.st.todayTok, c.st.todayByModel = today, 0, 0, nil
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
		// ts is written in UTC: comparing its date prefix to the local
		// "today" put the day boundary in the wrong place (a 21:00 CDT row
		// stamped 02:00Z landed on tomorrow), so today's totals were a
		// rolling 24h window rather than today.
		day := ""
		if t, err := time.Parse(time.RFC3339Nano, u.TS); err == nil {
			day = t.Local().Format("2006-01-02")
		} else if len(u.TS) >= 10 {
			day = u.TS[:10]
		}
		c.st.allReq++
		c.st.allTok += n
		// the file holds every day: only today's rows are today's totals.
		// counting the whole file as today made both numbers read the same.
		if day == today {
			c.st.todayReq++
			c.st.todayTok += n
			if u.Model != "" {
				if c.st.todayByModel == nil {
					c.st.todayByModel = map[string]modelUse{}
				}
				mu := c.st.todayByModel[u.Model]
				mu.req++
				mu.tok += n
				mu.comp += u.Completion
				mu.dur += u.DurationMs
				c.st.todayByModel[u.Model] = mu
			}
		}
		if c.st.since == "" && day != "" {
			c.st.since = day
		}
	}
	return c.st
}

// usagePathFor resolves the usage log the server writes. the dashboards used
// to hardcode nim-usage.jsonl, so a config that moved usage.path showed zero
// totals on every refresh. unset keeps the default name.
func usagePathFor(dir string) string {
	name := "nim-usage.jsonl"
	cp := configPath()
	if _, err := os.Stat(cp); err != nil {
		cp = filepath.Join(dir, "config.yml")
	}
	if raw, err := os.ReadFile(cp); err == nil {
		var c struct {
			Usage struct {
				Path string `yaml:"path"`
			} `yaml:"usage"`
		}
		if yaml.Unmarshal(raw, &c) == nil && c.Usage.Path != "" {
			name = c.Usage.Path
		}
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(dir, name)
	}
	return name
}

// prefixed puts the backend on every id. a portal id names its own vendor
// ("poolside/laguna-s-2.1:free"), so a bare slash is no reason to skip it.
func prefixed(prefix string, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			out = append(out, id)
			continue
		}
		out = append(out, prefix+id)
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

// dashAnimMsg drives the sub-second repaints a lit model row needs; the 2s
// poll tick is far too slow to animate anything.
type dashAnimMsg struct{}

type dashMsg struct {
	d   *StatusResponse
	st  nimStats
	err error
}

// flashFor is how long a model row stays lit after it serves a request.
const flashFor = 500 * time.Millisecond

// flashFrames spin in the gutter around a model id that just served. each is
// one terminal cell, so the row never shifts while it animates.
var flashFrames = []string{"✦", "✧", "✺", "✳", "✩", "·"}

type dashModel struct {
	port, dir string
	w, h      int
	top       int
	d         *StatusResponse
	st        nimStats
	err       error
	at        time.Time
	// seen is the request count per model at the last frame; a model whose
	// count grew lands in flashes and lights up for flashFor.
	seen    map[string]int
	flashes map[string]time.Time
	// loading holds a fetch in flight: the tick is 2s and the client timeout
	// 3s, so a slow /status would otherwise stack polls (and concurrent
	// writes to statsCache) on every tick.
	loading bool
	// pins are the table headers and where they sit in the rendered list,
	// recorded by render() so the view can hold one at the top while scrolled.
	pins []dashPin
}

func nimStatusTail(e nimEnv) {
	m := &dashModel{port: e.port, dir: e.dir}
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		nimFatal("dashboard", err)
	}
}

// dashOnce prints the dashboard once for `./nim status`. reserve is the rows
// the caller already printed above it.
func dashOnce(e nimEnv, reserve int) error {
	d, err := nimFetchStatus(e.port)
	if err != nil {
		return err
	}
	m := &dashModel{
		port: e.port,
		dir:  e.dir,
		d:    d,
		st:   usageTotalsFor(usagePathFor(e.dir)),
	}
	m.w, m.h = termSize()
	// render() budgets bodyHeight() = h-3 for the blank and key-hint lines the
	// tea view adds; a one-shot prints neither, so hand those rows back.
	m.h = m.h - reserve + 3
	lines := m.render()
	// a one-shot has no viewport: keep it to a screenful and point at the
	// dashboard, which scrolls the whole catalog.
	if max := m.h; max > 0 && len(lines) > max {
		n := len(lines) - max + 1
		lines = append(lines[:max-1:max-1], cDim.Render(fmt.Sprintf("· %d more rows · ./nim status-tail scrolls them", n)))
	}
	for _, ln := range lines {
		fmt.Println(ansi.Truncate(ln, m.w, "…"))
	}
	return nil
}

// termSize reports the terminal size, falling back to rich's 80x25 when the
// output is not a tty.
func termSize() (int, int) {
	w, h, err := term.GetSize(os.Stdout.Fd())
	if err != nil || w <= 0 || h <= 0 {
		return 80, 25
	}
	return w, h
}

// modelsLimit is how many rows fit in a bare `./nim models`.
func modelsLimit() int {
	_, h := termSize()
	n := h - 6
	if n < 1 {
		n = 1
	}
	return n
}

func dashLoad(port, dir string) tea.Cmd {
	return func() tea.Msg {
		d, err := nimFetchStatus(port)
		if err != nil {
			return dashMsg{err: err}
		}
		return dashMsg{d: d, st: usageTotalsFor(usagePathFor(dir))}
	}
}

func dashTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return dashTickMsg{} })
}

func dashAnimTick() tea.Cmd {
	return tea.Tick(60*time.Millisecond, func(time.Time) tea.Msg { return dashAnimMsg{} })
}

func (m *dashModel) Init() tea.Cmd {
	m.loading = true
	return tea.Batch(dashLoad(m.port, m.dir), dashTick())
}

func (m *dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case dashTickMsg:
		if m.loading {
			return m, dashTick()
		}
		m.loading = true
		return m, tea.Batch(dashLoad(m.port, m.dir), dashTick())
	case dashAnimMsg:
		// repaint while anything is lit, then let the chain die so an idle
		// dashboard is back to one wake-up per poll.
		live := false
		for id, at := range m.flashes {
			if time.Since(at) >= flashFor {
				delete(m.flashes, id)
				continue
			}
			live = true
		}
		if live {
			return m, dashAnimTick()
		}
		return m, nil
	case dashMsg:
		m.loading = false
		// one slow /status must not wedge the view: keep the last good frame
		// and let the tick chain already in flight try again. scheduling a tick
		// here added a second chain per failed fetch, so an hour of intermittent
		// timeouts ended up polling and redrawing dozens of times a second.
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err, m.d, m.st, m.at = nil, msg.d, msg.st, time.Now()
		was := len(m.flashes) > 0
		m.trackUsage()
		if !was && len(m.flashes) > 0 {
			return m, dashAnimTick()
		}
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

// trackUsage lights a model row when its request count grew between
// refreshes: the server's own counters, or today's log rows when the server
// restarted under the dashboard and its totals went back to zero.
func (m *dashModel) trackUsage() {
	cur := make(map[string]int, len(m.seen))
	if m.d.Usage != nil {
		for id, t := range m.d.Usage.ByModel {
			cur[id] = t.Requests
		}
	}
	for id, u := range m.st.todayByModel {
		if n := int(u.req); n > cur[id] {
			cur[id] = n
		}
	}
	if m.seen != nil {
		if m.flashes == nil {
			m.flashes = map[string]time.Time{}
		}
		now := time.Now()
		for id, n := range cur {
			if n > m.seen[id] {
				m.flashes[id] = now
			}
		}
	}
	m.seen = cur
}

// flashAt reports how far through its flash a model is (1 = just lit, 0 =
// settled) and whether it is lit at all.
func (m *dashModel) flashAt(id string) (float64, bool) {
	at, ok := m.flashes[id]
	if !ok {
		return 0, false
	}
	el := time.Since(at)
	if el < 0 || el >= flashFor {
		return 0, false
	}
	return 1 - float64(el)/float64(flashFor), true
}

// flashStyle eases the highlight from bright white back to the provider's
// colour, so a used row settles instead of snapping.
func flashStyle(ease float64, prov string) lipgloss.Style {
	ease = ease * ease * (3 - 2*ease)
	switch {
	case ease > 0.8:
		return cWhite
	case ease > 0.55:
		return cSpring
	case ease > 0.3:
		return cGreen
	}
	return provStyle(prov)
}

// flashCell draws a model id in its reserved gutter: two blank cells either
// side normally, a pair of spinning sparks while the row is lit. the gutter
// is always there, so a flash never shifts the rest of the table.
func (m *dashModel) flashCell(full, prov string) string {
	ease, on := m.flashAt(full)
	if !on {
		return provStyle(prov).Render("  " + full + "  ")
	}
	f := flashFrames[int((1-ease)*float64(len(flashFrames)))%len(flashFrames)]
	out := flashStyle(ease, prov).Render(f + " " + full + " " + f)
	if w := lipgloss.Width(out); w < lipgloss.Width(full)+4 {
		out = padR(out, lipgloss.Width(full)+4)
	}
	return out
}

func (m *dashModel) bodyHeight() int {
	h := m.h - 3
	if h < 5 {
		h = 5
	}
	return h
}

func (m *dashModel) View() string {
	if m.err != nil && m.d == nil {
		return ansi.Truncate(cRed.Render("nimroute: "+m.err.Error()), m.w, "…") +
			"\n\n" + cDim.Render("[q] quit")
	}
	if m.d == nil {
		return cDim.Render("loading…")
	}
	lines := m.render()
	body := m.bodyHeight()
	// a pinned header spends rows the viewport would have shown, so the scroll
	// limit shrinks by exactly what it takes.
	pinned := m.pinnedHeader(m.top)
	room := body - len(pinned)
	if room < 1 {
		pinned, room = nil, body
	}
	max := len(lines) - room
	if max < 0 {
		max = 0
	}
	if m.top > max {
		m.top = max
		pinned = m.pinnedHeader(m.top)
		room = body - len(pinned)
		if room < 1 {
			pinned, room = nil, body
		}
	}
	end := m.top + room
	if end > len(lines) {
		end = len(lines)
	}
	shown := make([]string, 0, body)
	for _, ln := range pinned {
		shown = append(shown, ansi.Truncate(ln, m.w, "…"))
	}
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
	return strings.Join(shown, "\n") + "\n\n" + ansi.Truncate(foot, m.w, "…")
}

func (m *dashModel) headLines() []string {
	d := m.d
	head := cSlate.Render("NimRoute " + d.Version)
	head += cDim.Render("  up " + d.Uptime + "  ")
	head += fmt.Sprintf("load %d/%d", d.Concurrent, d.SemLimit)
	if d.Keys != nil {
		ks := fmt.Sprintf("  keys %d/%d", d.Available, d.Total)
		if d.Available > 0 {
			head += cGreen.Render(ks)
		} else {
			head += cRed.Render(ks)
		}
	}

	sum := fmt.Sprintf("opencode %d active · nvidia %d active · lanes %d/%d",
		d.ConcurrentZen, d.ConcurrentNV, len(d.ZenLanes), d.LaneCap)
	return []string{head, cDim.Render(sum)}
}

func (m *dashModel) tailLines() []string {
	d, st := m.d, m.st
	var tail []string
	if len(m.modelGroups()) > 0 {
		tail = append(tail, cDim.Render("* tok/s uses complete request time, not decode speed · · = no sample"))
	} else {
		tail = append(tail, cDim.Render("no models (pool still starting)"))
	}
	if bl := benchLine(d.Benchmarks); bl != "" {
		tail = append(tail, cDim.Render(bl))
	}
	if d.Freepi != nil {
		for _, a := range d.Freepi.Accounts {
			mark, style := "◇", cMagenta
			if a.Inflight {
				mark, style = "◆", cYellow
			}
			l := style.Render(mark + " freepi:" + a.Name + "  ")
			tier := a.Tier
			if tier == "" {
				tier = "?"
			}
			l += fmt.Sprintf("%s req · %s tok · %s · %g credits · ",
				fmtInt(a.Requests), fmtTok(int(a.Prompt+a.Completion)), tier, a.Credits)
			l += usageBar(a.SpentPct, 10)
			l += cDim.Render(fmt.Sprintf(" %s day · %d today", fmtPct(a.SpentPct), a.ReqToday))
			tail = append(tail, l)
		}
	}
	if d.Nous != nil && len(d.Nous.Keys) > 0 {
		live := 0
		var cooling []string
		for _, k := range d.Nous.Keys {
			if k.Available {
				live++
				continue
			}
			name, cd := k.Name, k.Cooldown
			if name == "" {
				name = "?"
			}
			if cd == "" {
				cd = "?"
			}
			cooling = append(cooling, name+" "+cd)
		}
		l := cDim.Render("nous  ")
		up := fmt.Sprintf("%d/%d keys up", live, len(d.Nous.Keys))
		if live > 0 {
			l += cSpring.Render(up)
		} else {
			l += cRed.Render(up)
		}
		if len(cooling) > 0 {
			l += cDim.Render("  ·  cooling " + strings.Join(cooling, ", "))
		}
		tail = append(tail, l)
	}
	if d.Pool != nil {
		p := cDim.Render("pool  ") + cGreen.Render(fmt.Sprintf("%d verified", d.Pool.Verified))
		p += cDim.Render(" · ")
		dropped := fmt.Sprintf("%d dropped", d.Pool.Dropped)
		if d.Pool.Dropped > 0 {
			p += cRed.Render(dropped)
		} else {
			p += cDim.Render(dropped)
		}
		ago := d.Pool.Refresh
		if ago == "" {
			ago = "?"
		}
		p += cDim.Render(" · refresh " + ago + " ago")
		tail = append(tail, p)
	}
	l := cDim.Render("today ") + cWhite.Render(fmtInt(int(st.todayReq))+" req")
	l += cSlate.Render(" · " + fmtTok(int(st.todayTok)) + " tok")
	l += cDim.Render("   │   all-time ") + cWhite.Render(fmtInt(int(st.allReq))+" req")
	l += cSlate.Render(" · " + fmtTok(int(st.allTok)) + " tok")
	if st.since != "" {
		l += cDim.Render(" since " + st.since)
	}
	tail = append(tail, l)
	return tail
}

// render fits both tables to the terminal: head and foot stay put and each
// table gets a share of the rows left over, the busiest four first.
func (m *dashModel) render() []string {
	mrows, hasHealth, hasTail := m.modelRows()
	head, tail := m.headLines(), m.tailLines()
	wantL := len(m.d.ZenLanes)
	// the lane table draws every seat zen.lanes buys, empty ones dim, so the
	// row count follows the configured capacity rather than the live table.
	seats := m.d.LaneCap
	if seats < wantL {
		seats = wantL
	}
	// everything is rendered and the viewport scrolls: capping rows to the
	// terminal height left the rest of the catalog unreachable, so a short
	// pane could never show the models below the fold.
	out := append([]string{}, head...)
	m.pins = m.pins[:0]
	if seats > 0 {
		ll := m.laneLines(seats)
		// laneLines leads with a blank row, so its header starts one line in.
		m.pins = append(m.pins, dashPin{start: len(out) + 1, end: len(out) + len(ll), lines: ll[1:3]})
		out = append(out, ll...)
	}
	if len(mrows) > 0 {
		lead := seats == 0
		ml := modelLines(mrows, len(mrows), hasHealth, hasTail, lead)
		at := len(out)
		if lead {
			at++
		}
		m.pins = append(m.pins, dashPin{start: at, end: len(out) + len(ml), lines: ml[at-len(out) : at-len(out)+2]})
		out = append(out, ml...)
	}
	return append(out, tail...)
}

// dashPin is one table's header (column names plus the rule under them) and
// where it sits in the rendered list, so the view can hold it at the top once
// scrolling has taken it away.
type dashPin struct {
	start, end int
	lines      []string
}

// pinnedHeader is the header to hold above the viewport for one scroll offset:
// the table the first visible row belongs to, once its own header has scrolled
// off. past the last table's rows nothing is pinned, so the footer lines never
// sit under a stale table head.
func (m *dashModel) pinnedHeader(top int) []string {
	for i := len(m.pins) - 1; i >= 0; i-- {
		p := m.pins[i]
		if top >= p.start+len(p.lines) && top < p.end {
			return p.lines
		}
	}
	return nil
}

type modelGroup struct {
	prov  string
	ids   []string
	tails []string // trailing column: freepi multiplier, route vias
}

// modelGroups lists each backend catalog in display order.
func (m *dashModel) modelGroups() []modelGroup {
	d := m.d
	var gs []modelGroup
	if d.Opencode != nil && len(d.Opencode.Models) > 0 {
		gs = append(gs, modelGroup{prov: "opencode", ids: prefixed("opencode/", d.Opencode.Models)})
	}
	if d.Freepi != nil {
		mults := map[string]string{}
		for _, a := range d.Freepi.Accounts {
			for k, v := range a.Mults {
				mults[k] = v
			}
		}
		seen := map[string]bool{}
		var ids, tails []string
		for _, a := range d.Freepi.Accounts {
			for _, id := range a.Models {
				if !strings.Contains(id, "/") {
					id = "freeepi/" + id
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				ids = append(ids, id)
				tails = append(tails, mults[bareID(id)])
			}
		}
		if len(ids) > 0 {
			gs = append(gs, modelGroup{prov: "freepi", ids: ids, tails: tails})
		}
	}
	if d.Antseed != nil {
		ids := d.Antseed.Models
		if len(ids) == 0 { // a placeholder row, still backend-prefixed
			if d.Antseed.StaleFor == "" {
				ids = []string{"discovering…"}
			} else {
				ids = []string{"no free models"}
			}
		}
		gs = append(gs, modelGroup{prov: "antseed", ids: prefixed("antseed/", ids)})
	}
	if len(d.Keys) > 0 {
		names := make([]string, 0, len(d.Keys))
		for _, k := range d.Keys {
			names = append(names, k.Name)
		}
		gs = append(gs, modelGroup{prov: "nvidia", ids: []string{"nvidia/keys: " + strings.Join(names, ", ")}})
	}
	if d.Nous != nil && len(d.Nous.Models) > 0 {
		// portal ids already hold a slash, so the generic prefixing below
		// would leave them bare and miss the usage.by_model key.
		gs = append(gs, modelGroup{prov: "nous", ids: prefixed("nous/", d.Nous.Models)})
	}
	if d.Uno != nil && len(d.Uno.Models) > 0 {
		gs = append(gs, modelGroup{prov: "uno", ids: d.Uno.Models})
	}
	if d.OpenRouter != nil && len(d.OpenRouter.Models) > 0 {
		gs = append(gs, modelGroup{prov: "openrouter", ids: prefixed("openrouter/", d.OpenRouter.Models)})
	}
	if d.Orcarouter != nil && len(d.Orcarouter.Models) > 0 {
		gs = append(gs, modelGroup{prov: "orcarouter", ids: prefixed("orcarouter/", d.Orcarouter.Models)})
	}
	if d.Route != nil && d.Route.Enabled {
		g := modelGroup{prov: "route"}
		for _, r := range d.Route.Routes {
			g.ids = append(g.ids, r.ID)
			// the exact models, not just the backends: an older server only
			// fills via.
			t := strings.Join(r.Via, " · ")
			if len(r.Models) > 0 {
				t = strings.Join(r.Models, " · ")
			}
			g.tails = append(g.tails, t)
		}
		if len(g.ids) > 0 {
			gs = append(gs, g)
		}
	}
	if d.Wroute != nil && d.Wroute.Enabled {
		g := modelGroup{prov: "wroute"}
		for _, r := range d.Wroute.Routes {
			g.ids = append(g.ids, r.ID)
			g.tails = append(g.tails, strings.Join(r.Models, " · "))
		}
		if len(g.ids) > 0 {
			gs = append(gs, g)
		}
	}
	// each backend keeps its block, sorted, so the catalog reads as one list
	// per provider instead of whatever order the status snapshot happened to
	// carry. tails ride along with their ids.
	for gi := range gs {
		g := &gs[gi]
		order := make([]int, len(g.ids))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			return strings.ToLower(g.ids[order[a]]) < strings.ToLower(g.ids[order[b]])
		})
		ids := make([]string, len(g.ids))
		var tails []string
		if len(g.tails) > 0 {
			tails = make([]string, len(g.ids))
		}
		for n, i := range order {
			ids[n] = g.ids[i]
			if tails != nil && i < len(g.tails) {
				tails[n] = g.tails[i]
			}
		}
		g.ids, g.tails = ids, tails
	}
	return gs
}

// groupID restores the provider prefix a group's bare ids drop.
func groupID(prov, id string) string {
	if strings.Contains(id, "/") {
		return id
	}
	return prov + "/" + id
}

// bareID is python's full.split("/", 1)[-1].
func bareID(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// modelRows builds the full model table, one provider block after another:
// the dashboard scrolls, so every backend keeps its ids together instead of
// interleaving them across the table.
func (m *dashModel) modelRows() (rows [][]string, hasHealth, hasTail bool) {
	d := m.d
	byModel := nil2(d)
	bench := benchLookup(d.Benchmarks)

	gs := m.modelGroups()
	// an empty column of dashes reads like a broken table: only draw the
	// health and via columns when the snapshot actually carries them.
	// one known state is not enough — the column is a wall of "-" unless a
	// model in the table has been tried, which is the common cold-start.
	for _, g := range gs {
		for _, id := range g.ids {
			if h, ok := d.Health[id]; ok && h.State != "" {
				hasHealth = true
			}
		}
	}
	for _, g := range gs {
		for _, t := range g.tails {
			if t != "" {
				hasTail = true
			}
		}
		// a quoted price rides that same trailing column, like freepi's
		// multiplier: one column for row facts, never a second one.
		for i, id := range g.ids {
			if i < len(g.tails) && g.tails[i] != "" {
				continue
			}
			if _, ok := d.Prices[groupID(g.prov, id)]; ok {
				hasTail = true
			}
			if freeBackend(g.prov) && !strings.HasPrefix(id, "nvidia/keys:") {
				hasTail = true
			}
		}
	}
	for _, g := range gs {
		for i, val := range g.ids {
			// every row is one full id: provider and model never split, so the
			// table matches what a client sends and what /status keys usage by.
			full := val
			if !strings.Contains(val, "/") {
				full = g.prov + "/" + val
			}
			req, tok, tps := "0", cDim.Render("·"), cDim.Render("·")
			if t, ok := modelTotals(byModel, full); ok {
				req = fmtInt(int(t.Requests))
				tok = cDim.Render(fmtTok(int(t.Prompt + t.Completion)))
				if t.DurationMs > 0 {
					tps = cDim.Render(fmt.Sprintf("%.1f", float64(t.Completion)/(float64(t.DurationMs)/1000)))
				}
			} else if u, ok := m.st.todayByModel[full]; ok {
				// the server's counters reset on restart; today's log rows are
				// the same work, so the table agrees with the footer totals.
				req = fmtInt(int(u.req))
				tok = cDim.Render(fmtTok(int(u.tok)))
				if u.dur > 0 {
					tps = cDim.Render(fmt.Sprintf("%.1f", float64(u.comp)/(float64(u.dur)/1000)))
				}
			}
			tail := ""
			if i < len(g.tails) && g.tails[i] != "" {
				tail = cDim.Render(g.tails[i])
			} else if p, ok := d.Prices[full]; ok {
				tail = priceCell(p)
			} else if freeBackend(g.prov) && !strings.HasPrefix(val, "nvidia/keys:") {
				// a free backend quotes $0 on every model it lists: nothing
				// upstream ever sends a price for these ids.
				tail = priceCell(pricePair{})
			}
			if ease, on := m.flashAt(full); on {
				// the count that grew is part of the “this just served” read.
				req = flashStyle(ease, g.prov).Render(req)
			}
			row := []string{
				m.flashCell(full, g.prov),
				scoreCell(bench(full)),
				req, tok, tps,
			}
			if hasHealth {
				row = append([]string{healthCell(d.Health, full)}, row...)
			}
			if hasTail {
				row = append(row, tail)
			}
			rows = append(rows, row)
		}
	}
	return rows, hasHealth, hasTail
}

// freeBackend is a backend whose whole catalog costs nothing, so every row it
// lists quotes a zero price instead of leaving the column blank. nvidia's key
// row is not a model and is excluded by the caller.
func freeBackend(prov string) bool {
	switch prov {
	case "opencode", "antseed", "freepi", "nvidia":
		return true
	}
	return false
}

// priceCell draws one provider-quoted price as usd per million input/output
// tokens: "0.15/0.6". a zero pair is the free tier, quoted as $0 like any
// other price rather than labelled.
func priceCell(p pricePair) string {
	if p.In == 0 && p.Out == 0 {
		return cSpring.Render("$0")
	}
	// four significant digits: the catalog carries float noise (0.059999)
	// and the column is a pointer, not an invoice.
	fmtP := func(v float64) string { return strconv.FormatFloat(v, 'g', 4, 64) }
	return cDim.Render(fmtP(p.In) + "/" + fmtP(p.Out))
}

// modelLines draws the model table, capped at limit rows.
func modelLines(rows [][]string, limit int, hasHealth, hasTail, lead bool) []string {
	if len(rows) == 0 {
		return nil
	}
	want := len(rows)
	if limit < 0 {
		limit = 0
	}
	if want > limit {
		rows = rows[:limit]
	}
	cols := []colSpec{}
	if hasHealth {
		cols = append(cols, colSpec{"ok", false})
	}
	cols = append(cols, colSpec{"model", false}, colSpec{"score", true}, colSpec{"req", true},
		colSpec{"tokens", true}, colSpec{"tok/s*", true})
	if hasTail {
		// route and wroute rows list the exact models behind them here,
		// freepi rows their speed multiple: one header for both, or the
		// column reads like a bug.
		cols = append(cols, colSpec{"models / ×", false})
	}
	out := drawTable(cols, rows)
	if lead {
		out = append([]string{""}, out...)
	}
	if len(rows) < want {
		out = append(out, cDim.Render(fmt.Sprintf("⋮ %d more models · taller terminal shows more", want-len(rows))))
	}
	return append(out, "")
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

// drawTable pads every column to its widest cell, trims trailing blanks and
// rules under the header like rich's box.simple.
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
	width := 2 * (len(cols) - 1)
	for i, c := range cols {
		head[i] = cell(c.h, w[i], c.right)
		width += w[i]
	}
	out := []string{
		cDim.Render(strings.TrimRight(strings.Join(head, "  "), " ")),
		cDim.Render(strings.Repeat("─", width)),
	}
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

// laneLines draws the lane table, capped at limit rows. seats the table has
// room for but no live lane in (the pool is smaller than zen.lanes, so
// trimLanesToPool dropped them) are drawn dim, so a thin pool reads as empty
// seats rather than as a router that only ever had two lanes.
func (m *dashModel) laneLines(limit int) []string {
	lanes := append([]laneStatus(nil), m.d.ZenLanes...)
	slots := m.d.LaneCap
	if slots < len(lanes) {
		slots = len(lanes)
	}
	if slots == 0 {
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

	if limit < 0 {
		limit = 0
	}
	if limit < len(lanes) {
		lanes = lanes[:limit]
	}
	spare := slots - len(lanes)
	if spare > 4 {
		// a short pool would otherwise fill the pane with placeholders and
		// push the whole model catalog off screen; the count line carries
		// the rest.
		spare = 4
	}
	if room := limit - len(lanes); spare > room {
		spare = room
	}
	var rows [][]string
	anyCC := false
	for _, l := range lanes {
		lane := l.ID
		if lane == "" {
			lane = "?"
		}
		if lipgloss.Width(lane) > 14 {
			lane = ansi.Truncate(lane, 14, "")
		}
		exit, ttfb := cDim.Render("—"), "·"
		if l.Proxy != "" {
			exit = l.Proxy
			if i := strings.Index(exit, "://"); i >= 0 {
				exit = exit[i+3:]
			}
			if lipgloss.Width(exit) > 24 {
				exit = ansi.Truncate(exit, 23, "…")
			}
			ttfb = fmtMs(exits[l.Proxy].AvgMs)
		}
		cc := strings.TrimSpace(exits[l.Proxy].Country)
		if cc == "" {
			cc = strings.TrimSpace(l.Country)
		}
		if cc == "" || cc == "??" {
			cc = "·"
		} else {
			anyCC = true
		}
		r429 := strconv.Itoa(l.RateLimit)
		if l.RateLimit > 0 {
			r429 = cYellow.Render(r429)
		}
		rows = append(rows, []string{
			cSlate.Render(lane), exit, cc, ttfb, fmtInt(l.Requests), r429,
			fmtMs(l.AvgMs), fmtTok(l.Tokens),
		})
	}
	for i := 0; i < spare; i++ {
		dash := cDim.Render("—")
		dot := cDim.Render("·")
		rows = append(rows, []string{dash, dash, dot, dot, dot, dot, dot, dot})
	}
	cols := []colSpec{{"lane", false}, {"exit", false}, {"cc", false}, {"ttfb", true},
		{"req", true}, {"429", true}, {"avg turn", true}, {"tokens", true}}
	if !anyCC {
		c := append([]colSpec{}, cols[:2]...)
		c = append(c, cols[3:]...)
		cols = c
		for i := range rows {
			rows[i] = append(rows[i][:2], rows[i][3:]...)
		}
	}
	out := append([]string{""}, drawTable(cols, rows)...)
	if len(rows) < slots {
		out = append(out, cDim.Render(fmt.Sprintf("⋮ %d more lanes · no exit to pin them to", slots-len(rows))))
	}
	return append(out, "")
}
