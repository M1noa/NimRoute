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
)

// styles mirror nimstatus.py's palette.
var (
	cDim     = lipgloss.NewStyle().Faint(true)
	cBold    = lipgloss.NewStyle().Bold(true)
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
		st:   usageTotalsFor(filepath.Join(e.dir, "nim-usage.jsonl")),
	}
	m.w, m.h = termSize()
	// render() budgets bodyHeight() = h-3 for the blank and key-hint lines the
	// tea view adds; a one-shot prints neither, so hand those rows back.
	m.h = m.h - reserve + 3
	for _, ln := range m.render() {
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

	sum := fmt.Sprintf("opencode %d active · nvidia %d active · nim lanes %d/%d",
		d.ConcurrentZen, d.ConcurrentNV, len(d.ZenLanes), d.LaneCap)
	return []string{head, cDim.Render(sum), ""}
}

func (m *dashModel) tailLines() []string {
	d, st := m.d, m.st
	var tail []string
	if len(m.modelGroups()) > 0 {
		tail = append(tail, cDim.Render("* tok/s uses complete request time, not decode speed · · = no sample"))
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
	if len(tail) > 0 {
		tail = append([]string{""}, tail...)
	}
	return tail
}

// render fits both tables to the terminal: head and foot stay put and each
// table gets a share of the rows left over, the busiest four first.
func (m *dashModel) render() []string {
	mrows := m.modelRows()
	head, tail := m.headLines(), m.tailLines()
	// two table headers, two trailing blanks, one caption each
	spare := m.bodyHeight() - len(head) - len(tail) - 4
	laneN, modelN := splitRows(spare, len(m.d.ZenLanes), len(mrows), 4)
	if laneN == 0 && len(m.d.ZenLanes) > 0 {
		laneN = 1
	}
	if modelN == 0 && len(mrows) > 0 {
		modelN = 1
	}
	out := append([]string{}, head...)
	out = append(out, m.laneLines(laneN)...)
	out = append(out, modelLines(mrows, modelN)...)
	return append(out, tail...)
}

// splitRows divides the spare table rows between two lists: each starts at the
// busiest floor rows, then grows alternately while height remains.
func splitRows(spare, wantA, wantB, floor int) (int, int) {
	if spare < 0 {
		spare = 0
	}
	if wantA > 0 && wantB > 0 {
		if floor > spare/2 {
			floor = spare / 2
		}
	} else {
		floor = 0
	}
	a, b := wantA, wantB
	if a > floor {
		a = floor
	}
	if b > floor {
		b = floor
	}
	for spare-a-b > 0 && (a < wantA || b < wantB) {
		if a < wantA {
			a++
		} else if b < wantB {
			b++
		}
		if spare-a-b > 0 && b < wantB {
			b++
		}
	}
	return a, b
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
	if d.Route != nil && d.Route.Enabled {
		g := modelGroup{prov: "route"}
		for _, r := range d.Route.Routes {
			g.ids = append(g.ids, r.ID)
			g.tails = append(g.tails, strings.Join(r.Via, " · "))
		}
		if len(g.ids) > 0 {
			gs = append(gs, g)
		}
	}
	return gs
}

// bareID is python's full.split("/", 1)[-1].
func bareID(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// modelRows builds the full model table, round-robin across backends: a row
// from every family lands before any one spends the whole cap, so the late
// catalogs (openrouter, route) are never starved by opencode's list.
func (m *dashModel) modelRows() [][]string {
	d := m.d
	byModel := nil2(d)
	bench := benchLookup(d.Benchmarks)

	gs := m.modelGroups()
	worst := 0
	for _, g := range gs {
		if len(g.ids) > worst {
			worst = len(g.ids)
		}
	}
	var rows [][]string
	for i := 0; i < worst; i++ {
		for _, g := range gs {
			if i >= len(g.ids) {
				continue
			}
			val := g.ids[i]
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
			}
			tail := ""
			if i < len(g.tails) && g.tails[i] != "" {
				tail = cDim.Render(g.tails[i])
			}
			rows = append(rows, []string{
				healthCell(d.Health, full),
				provStyle(g.prov).Render(full),
				scoreCell(bench(full)),
				req, tok, tps, tail,
			})
		}
	}
	return rows
}

// modelLines draws the model table, capped at limit rows.
func modelLines(rows [][]string, limit int) []string {
	if len(rows) == 0 {
		return []string{cDim.Render("no models (pool still starting)"), ""}
	}
	want := len(rows)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	cols := []colSpec{{"", false}, {"model", false}, {"score", true}, {"req", true},
		{"tokens", true}, {"tok/s*", true}, {"", false}}
	out := drawTable(cols, rows)
	if len(rows) < want {
		out = append(out, cDim.Render(fmt.Sprintf("⋮ %d more models · taller terminal shows more", want-len(rows))))
	}
	out = append(out, "")
	return out
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

// laneLines draws the lane table, capped at limit rows.
func (m *dashModel) laneLines(limit int) []string {
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

	want := len(lanes)
	if limit > 0 && want > limit {
		lanes = lanes[:limit]
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
	out := drawTable(cols, rows)
	if len(lanes) < want {
		out = append(out, cDim.Render(fmt.Sprintf("⋮ %d more lanes · taller terminal shows more", want-len(lanes))))
	}
	out = append(out, "")
	return out
}
