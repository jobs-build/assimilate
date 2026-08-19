package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/jobs-build/jobs-iroh/api"
	jitui "github.com/jobs-build/jobs-iroh/tui"
	"github.com/jobs-build/jobs-iroh/wire"

	"github.com/jobs-build/assimilate/internal/spec"
)

const (
	maxLogLines  = 5000 // per-build ring capacity; oldest lines drop
	nodeLogLines = 2000 // per-node ring capacity (one ring per output node)
	minLeftW     = 20
	maxLeftW     = 48
	// rowOverhead is the non-name part of a left-pane image row:
	// cursor(1) + expander(1) + space + elapsed(4) + space + icon(1) + pad.
	rowOverhead = 9
)

type (
	eventMsg  struct{ ev spec.Event }
	closedMsg struct{}  // event stream closed: quit is now allowed
	cancelReq struct{}  // cancel request from a signal (filter-injected)
	tickMsg   time.Time // 1s cadence while any build runs (elapsed redraw)
)

var (
	styleDim   = lipgloss.NewStyle().Faint(true)
	styleGreen = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleRed   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleSel   = lipgloss.NewStyle().Bold(true)
)

// buildRow is one image build's UI state: the top-level row plus its
// expandable jobs-iroh build-graph subtree (fed by KindSnapshot events) and
// the per-node output rings (fed by node-tagged KindLog events).
type buildRow struct {
	name  string
	state spec.BuildState
	info  string    // transient note (push progress, error summary)
	log   *ring     // combined build output (node lines carry a node prefix)
	start time.Time // set when the build leaves StatePending
	end   time.Time // set on the terminal transition

	// Build-graph subtree (second level: "image and then build").
	snap     *api.Snapshot     // latest coalesced watch snapshot
	snapAt   time.Time         // arrival clock, for elapsed extrapolation
	graph    *jitui.BuildGraph // folded snap; nil until a graph-bearing snap
	labels   map[string]string // log-node name → display label (pane titles)
	nodeLogs map[string]*ring  // node name → its own output (lazy)
	expanded bool              // image row expanded into its subtree
	exp      map[string]bool   // per-path expansion override within it
}

// nodeRing returns (creating lazily) the ring of one node's output.
func (r *buildRow) nodeRing(node string) *ring {
	if r.nodeLogs == nil {
		r.nodeLogs = map[string]*ring{}
	}
	rg := r.nodeLogs[node]
	if rg == nil {
		rg = newRing(nodeLogLines)
		r.nodeLogs[node] = rg
	}
	return rg
}

// visRow is one selectable line of the two-level tree: an image row
// (tree == nil) or one row of that image's build-graph subtree.
type visRow struct {
	img  int
	tree *jitui.TreeRow // nil = the image row itself
}

// model is the TUI. Quit is gated on closedMsg — the stream must drain
// before the program ends — with one escape hatch: a second quit request
// while already cancelling sets forceQuit and quits immediately, so a
// stream that never closes cannot wedge the terminal.
type model struct {
	rows     []buildRow
	visible  []visRow // flattened two-level tree; selected indexes it
	selected int

	vp     viewport.Model
	follow bool // pinned to the log tail until the user scrolls up
	spin   spinner.Model

	width, height int
	baseLeftW     int // clamp(longest name+rowOverhead, minLeftW, maxLeftW)

	cancel     context.CancelFunc
	cancelling bool // cancel fired; footer shows cancelling…
	forceQuit  bool // second quit request while cancelling: abandon the drain
	closed     bool
	ticking    bool   // 1s ticker armed
	spinning   bool   // spinner tick armed
	logDirty   bool   // selected ring changed; rebuild coalesced to the next tick
	refreshes  int    // test seam: counts real O(ring) viewport rebuilds
	globalNote string // latest global (Build == -1) line/info, footer right
	clock      func() time.Time
}

func newModel(names []string, cancel context.CancelFunc, clock func() time.Time) model {
	rows := make([]buildRow, len(names))
	longest := 0
	for i, n := range names {
		rows[i] = buildRow{name: n, state: spec.StatePending, log: newRing(maxLogLines)}
		if w := lipgloss.Width(n); w > longest {
			longest = w
		}
	}
	m := model{
		rows:      rows,
		vp:        viewport.New(0, 0),
		follow:    true,
		spin:      spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		baseLeftW: clampInt(longest+rowOverhead, minLeftW, maxLeftW),
		cancel:    cancel,
		clock:     clock,
	}
	m.rebuildVisible()
	return m
}

// rebuildVisible reflattens the two-level tree: every image row, plus — for
// expanded images with a folded graph — that image's build-graph rows.
// Selection is preserved by identity (image, path) and clamped.
func (m *model) rebuildVisible() {
	var curImg, curPath = -1, ""
	if m.selected < len(m.visible) {
		v := m.visible[m.selected]
		curImg = v.img
		if v.tree != nil {
			curPath = v.tree.Path
		}
	}
	m.visible = m.visible[:0]
	sel := 0
	for i := range m.rows {
		if curImg == i && curPath == "" {
			sel = len(m.visible)
		}
		m.visible = append(m.visible, visRow{img: i})
		r := &m.rows[i]
		if !r.expanded || r.graph == nil {
			continue
		}
		for _, tr := range jitui.FlattenTree(r.graph, r.exp) {
			if curImg == i && curPath == tr.Path {
				sel = len(m.visible)
			}
			m.visible = append(m.visible, visRow{img: i, tree: &tr})
		}
	}
	m.selected = clampInt(sel, 0, max(0, len(m.visible)-1))
}

// selectedRing is the ring the right pane shows: the image's combined log,
// or the selected graph row's own node output ("" ring shows empty).
func (m *model) selectedRing() *ring {
	if m.selected >= len(m.visible) {
		return nil
	}
	v := m.visible[m.selected]
	r := &m.rows[v.img]
	if v.tree == nil {
		return r.log
	}
	if row := m.graphRow(v); row != nil && row.LogNode != "" {
		return r.nodeRing(row.LogNode)
	}
	return nil
}

// graphRow resolves a visRow's folded build-graph row (nil for image rows).
func (m *model) graphRow(v visRow) *jitui.BuildRow {
	if v.tree == nil || m.rows[v.img].graph == nil {
		return nil
	}
	return m.rows[v.img].graph.Rows[v.tree.Node]
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) { return m.update(msg) }

// update is Update with a concrete receiver type, for tests.
func (m model) update(msg tea.Msg) (model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.KeyMsg:
		return m.updateKey(msg)

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		return m, cmd

	case tickMsg:
		m.flushLog()
		if m.anyActive() {
			return m, tickCmd()
		}
		m.ticking = false
		return m, nil

	case spinner.TickMsg:
		m.flushLog()
		if !m.anyBuilding() {
			m.spinning = false
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case eventMsg:
		return m.apply(msg.ev)

	case cancelReq:
		return m.quitRequest()

	case closedMsg:
		m.closed = true
		m.flushLog()
		return m, tea.Quit
	}
	return m, nil
}

func (m model) updateKey(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(+1)
	case "right", "l":
		m.setExpanded(true)
	case "left", "h":
		m.setExpanded(false)
	case "enter", " ", "space":
		m.toggleExpanded()
	case "q", "ctrl+c":
		return m.quitRequest()
	case "pgup", "pgdown":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		return m, cmd
	}
	return m, nil
}

// expandable reports whether the selected row can fold: an image row with a
// folded graph, or a graph row with children.
func (m *model) expandable() bool {
	if m.selected >= len(m.visible) {
		return false
	}
	v := m.visible[m.selected]
	if v.tree == nil {
		return m.rows[v.img].graph != nil
	}
	return v.tree.HasKids
}

// setExpanded records the selected row's expansion and reflattens.
func (m *model) setExpanded(want bool) {
	if !m.expandable() {
		return
	}
	v := m.visible[m.selected]
	r := &m.rows[v.img]
	if v.tree == nil {
		if r.expanded == want {
			return
		}
		r.expanded = want
	} else {
		if r.exp == nil {
			r.exp = map[string]bool{}
		}
		r.exp[v.tree.Path] = want
	}
	m.rebuildVisible()
	m.follow = true
	m.layout() // expansion changes leftWidth; resize the viewport with it
}

func (m *model) toggleExpanded() {
	if !m.expandable() {
		return
	}
	v := m.visible[m.selected]
	if v.tree == nil {
		m.setExpanded(!m.rows[v.img].expanded)
	} else {
		m.setExpanded(!v.tree.Expanded)
	}
}

// quitRequest handles q/ctrl-c/SIGINT: the first request fires cancel
// exactly once and waits for the closed sentinel so the stream drains;
// a second request while already cancelling force-quits past a stream
// that never closes.
func (m model) quitRequest() (model, tea.Cmd) {
	if m.cancelling {
		m.forceQuit = true
		return m, tea.Quit
	}
	m.requestCancel()
	return m, nil
}

// requestCancel fires cancel exactly once.
func (m *model) requestCancel() {
	if m.cancelling {
		return
	}
	m.cancelling = true
	if m.cancel != nil {
		m.cancel()
	}
}

// moveSelection selects a neighbouring visible row and re-pins its log tail.
func (m *model) moveSelection(delta int) {
	if len(m.visible) == 0 {
		return
	}
	sel := clampInt(m.selected+delta, 0, len(m.visible)-1)
	if sel == m.selected {
		return
	}
	m.selected = sel
	m.follow = true
	m.refreshLog()
}

// apply folds one stream event into the model and arms the ticker/spinner
// when a state transition first needs them.
func (m model) apply(ev spec.Event) (model, tea.Cmd) {
	if ev.Build < 0 || ev.Build >= len(m.rows) {
		switch ev.Kind {
		case spec.KindLog:
			m.globalNote = sanitizeLine(ev.Line)
		case spec.KindInfo, spec.KindState:
			if ev.Info != "" {
				m.globalNote = ev.Info
			}
		}
		return m, nil
	}
	row := &m.rows[ev.Build]
	var cmds []tea.Cmd
	switch ev.Kind {
	case spec.KindState:
		prev := row.state
		row.state = ev.State
		switch {
		case ev.Info != "":
			row.info = ev.Info
		case ev.State.Terminal() && ev.State != spec.StateFailed:
			// Drop a stale transient note ("push 3/7") from a settled row;
			// a failed row keeps its detail even when the event has none.
			row.info = ""
		}
		if prev == spec.StatePending && ev.State != spec.StatePending {
			row.start = m.clock()
		}
		if ev.State.Terminal() {
			row.end = m.clock()
			if row.start.IsZero() { // cancelled before it ever started
				row.start = row.end
			}
			m.flushLog() // the last tick may never come; show the final tail now
		}
		if ev.State == spec.StateFailed && row.graph != nil && !row.expanded {
			// A failed image unfolds so the failing node is one ↓ away.
			row.expanded = true
			m.rebuildVisible()
			m.layout()
		}
		if !m.ticking && m.anyActive() {
			m.ticking = true
			cmds = append(cmds, tickCmd())
		}
		if !m.spinning && m.anyBuilding() {
			m.spinning = true
			cmds = append(cmds, m.spin.Tick)
		}
	case spec.KindSnapshot:
		if ev.Snap == nil {
			return m, nil
		}
		row.snap, row.snapAt = ev.Snap, m.clock()
		// An old server sends no graph edges: the image row simply never
		// becomes expandable (SnapshotHasGraph keeps the last usable fold).
		if jitui.SnapshotHasGraph(*ev.Snap) {
			row.graph = jitui.FoldSnapshot(*ev.Snap)
			row.labels = graphLabels(row.graph)
		}
		if row.expanded {
			m.rebuildVisible()
			// The selected node's log target can change with the fold
			// (queued → running switches LogNode) — re-aim the viewport.
			if m.selected < len(m.visible) && m.visible[m.selected].img == ev.Build && m.visible[m.selected].tree != nil {
				m.refreshLog()
			}
		}
	case spec.KindLog:
		line := sanitizeLine(ev.Line)
		combined := line
		if ev.Node != "" {
			row.nodeRing(ev.Node).push(line)
			combined = shortNodeName(ev.Node) + " │ " + line
		}
		row.log.push(combined)
		if m.eventTouchesSelection(ev) {
			// Coalesce: mark dirty and let the pending tick rebuild, so a
			// verbose build costs O(ring) per tick, not per line.
			m.logDirty = true
			if !m.ticking && !m.spinning {
				m.refreshLog() // no tick is coming; rebuild now so the tail follows
			}
		}
	case spec.KindInfo:
		row.info = ev.Info
	}
	return m, tea.Batch(cmds...)
}

// eventTouchesSelection reports whether a log event landed in the ring the
// right pane currently shows.
func (m *model) eventTouchesSelection(ev spec.Event) bool {
	if m.selected >= len(m.visible) {
		return false
	}
	v := m.visible[m.selected]
	if v.img != ev.Build {
		return false
	}
	if v.tree == nil {
		return true // combined ring: every line of this build lands there
	}
	gr := m.graphRow(v)
	return gr != nil && gr.LogNode != "" && gr.LogNode == ev.Node
}

// graphLabels indexes a fold's display labels by log node, for pane titles
// and node-line prefixes.
func graphLabels(g *jitui.BuildGraph) map[string]string {
	ls := map[string]string{}
	for _, r := range g.Rows {
		if r.LogNode == "" {
			continue
		}
		l := r.Label
		if l == "" {
			l = shortNodeName(r.Node)
		}
		ls[r.LogNode] = l
	}
	return ls
}

// shortNodeName renders a node name as kind:key8 (follow.go's prefix form).
func shortNodeName(name string) string {
	kind, k, err := wire.ParseNodeName(name)
	if err != nil {
		return name
	}
	return kind + ":" + k.String()[:8]
}

// anyActive: a build is running (left pending, not settled) — the elapsed
// column is live and the 1s ticker must run.
func (m model) anyActive() bool {
	for _, r := range m.rows {
		if r.state != spec.StatePending && !r.state.Terminal() {
			return true
		}
	}
	return false
}

func (m model) anyBuilding() bool {
	for _, r := range m.rows {
		if r.state == spec.StateBuilding {
			return true
		}
	}
	return false
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// --- geometry ---

// leftWidth caps the fixed left pane to half of a narrow terminal. An
// expanded build-graph subtree needs room for its indented rows, so any
// open fold widens the pane toward 2/5 of the screen.
func (m model) leftWidth() int {
	w := m.baseLeftW
	if m.anyExpanded() {
		if t := clampInt(m.width*2/5, minLeftW, maxLeftW); t > w {
			w = t
		}
	}
	if m.width > 0 && w > m.width/2 {
		w = max(1, m.width/2)
	}
	return w
}

func (m model) anyExpanded() bool {
	for i := range m.rows {
		if m.rows[i].expanded && m.rows[i].graph != nil {
			return true
		}
	}
	return false
}

// layout recomputes the panes after a resize.
func (m *model) layout() {
	m.vp.Width = max(1, m.width-m.leftWidth()-1)
	m.vp.Height = max(1, m.height-2) // right-pane title + footer
	m.refreshLog()
}

// refreshLog reloads the viewport with the selected row's ring buffer (the
// image's combined log, or one node's own output), hard-truncating lines to
// the pane width and re-pinning the tail when following. O(ring) — appended
// lines only mark logDirty and rely on flushLog at tick cadence.
func (m *model) refreshLog() {
	m.logDirty = false
	if len(m.visible) == 0 || m.vp.Width <= 0 {
		return
	}
	m.refreshes++
	var lines []string
	if rg := m.selectedRing(); rg != nil {
		lines = rg.lines()
	}
	for i, l := range lines {
		lines[i] = truncLine(l, m.vp.Width)
	}
	m.vp.SetContent(strings.Join(lines, "\n"))
	if m.follow {
		m.vp.GotoBottom()
	}
}

// flushLog performs the rebuild a coalesced log line deferred.
func (m *model) flushLog() {
	if m.logDirty {
		m.refreshLog()
	}
}

// sanitizeLine makes one raw build-log line inert for the viewport: the
// terminal must render it as text, never execute it. Carriage returns keep
// progress-bar semantics (the text after the last interior \r wins), every
// ANSI escape sequence (CSI/OSC/DCS/SGR/…) is stripped, tabs expand to
// 8-column stops, and remaining C0/DEL control bytes drop. RunPlain
// intentionally bypasses this: non-TTY consumers get the raw bytes.
func sanitizeLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	s = ansi.Strip(s) // drops escape sequences but keeps C0 controls
	if !strings.ContainsFunc(s, isCtrl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	col := 0
	for _, r := range s {
		switch {
		case r == '\t':
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case isCtrl(r):
			// dropped (\a, \b, …)
		default:
			b.WriteRune(r)
			col += ansi.StringWidth(string(r))
		}
	}
	return b.String()
}

func isCtrl(r rune) bool { return r < 0x20 || r == 0x7f }

func truncLine(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// --- view ---

// View renders left build list │ right log pane, footer last. Must not
// crash before the first WindowSizeMsg.
func (m model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "starting…"
	}
	leftW := m.leftWidth()
	contentH := max(1, m.height-1)
	left := m.leftLines(leftW, contentH)
	right := m.rightLines(max(1, m.width-leftW-1), contentH)
	var b strings.Builder
	for i := 0; i < contentH; i++ {
		b.WriteString(left[i])
		b.WriteString(styleDim.Render("│"))
		b.WriteString(right[i])
		b.WriteByte('\n')
	}
	b.WriteString(m.footer())
	return b.String()
}

// leftLines renders the two-level tree, windowed around the selection when
// it outgrows the pane.
func (m model) leftLines(w, h int) []string {
	top := 0
	if len(m.visible) > h {
		top = clampInt(m.selected-h/2, 0, len(m.visible)-h)
	}
	lines := make([]string, h)
	for i := range lines {
		if j := top + i; j < len(m.visible) {
			lines[i] = m.rowLine(j, w)
		} else {
			lines[i] = strings.Repeat(" ", w)
		}
	}
	return lines
}

// rowLine is one left-pane line: an image row
// ('<cursor><expander> <name> <elapsed> <icon>', the Info note dim in the
// spare name space) or one of its build-graph rows, indented; always
// exactly w cells wide.
func (m model) rowLine(i, w int) string {
	v := m.visible[i]
	if v.tree != nil {
		return m.nodeLine(i, v, w)
	}
	r := m.rows[v.img]
	cursor := " "
	if i == m.selected {
		cursor = ">"
	}
	expander := " "
	if r.graph != nil {
		if r.expanded {
			expander = "▾"
		} else {
			expander = "▸"
		}
	}
	nameW := max(1, w-rowOverhead)
	field := truncLine(r.name, nameW)
	if i == m.selected {
		field = styleSel.Render(field)
	}
	if spare := nameW - lipgloss.Width(field); r.info != "" && spare >= 5 {
		field += " " + styleDim.Render(truncLine(r.info, spare-1))
	}
	fill := max(0, nameW-lipgloss.Width(field))
	line := cursor + expander + field + strings.Repeat(" ", fill) +
		fmt.Sprintf(" %4s ", fmtElapsed(m.elapsed(r))) + m.icon(r.state)
	return truncLine(line, w)
}

// nodeLine renders one build-graph row: indent under its image, expander,
// phase glyph, label, then the stage/elapsed/error detail dim.
func (m model) nodeLine(i int, v visRow, w int) string {
	tr := v.tree
	row := m.graphRow(v)
	if row == nil {
		return strings.Repeat(" ", w)
	}
	cursor := " "
	if i == m.selected {
		cursor = ">"
	}
	expander := " "
	if tr.HasKids {
		if tr.Expanded {
			expander = "▾"
		} else {
			expander = "▸"
		}
	}
	label := row.Label
	if label == "" {
		label = shortNodeName(row.Node)
	}
	if i == m.selected {
		label = styleSel.Render(label)
	}
	line := cursor + strings.Repeat("  ", tr.Depth+1) + expander + " " +
		m.nodeIcon(row) + " " + label
	if detail := m.nodeDetail(&m.rows[v.img], row); detail != "" {
		line += " " + styleDim.Render(detail)
	}
	if pad := w - lipgloss.Width(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return truncLine(line, w)
}

// nodeDetail is a graph row's compact status suffix. Running elapsed is the
// server-computed ElapsedMs extrapolated by the client-clock delta since
// the snapshot arrived (skew-safe, keeps ticking between pushes).
func (m model) nodeDetail(img *buildRow, row *jitui.BuildRow) string {
	elapsed := time.Duration(row.ElapsedMs) * time.Millisecond
	switch row.Phase {
	case wire.PhaseRunning, wire.PhasePublishing:
		if !img.snapAt.IsZero() {
			elapsed += m.clock().Sub(img.snapAt)
		}
		return row.Stage + " " + fmtElapsed(elapsed)
	case wire.PhaseQueued:
		return row.Stage + " queued"
	case wire.PhaseDone:
		if row.Cached {
			return "(cached)"
		}
		if row.ElapsedMs > 0 {
			return fmtElapsed(elapsed)
		}
		return ""
	case wire.PhaseFailed:
		d := row.Stage
		if row.Err != "" {
			d += ": " + row.Err
		}
		return d
	case wire.PhaseUpstream:
		return "upstream failed"
	case wire.PhaseCancelled:
		return "cancelled"
	}
	return ""
}

// nodeIcon is a graph row's one-cell state marker (image rows use icon).
func (m model) nodeIcon(row *jitui.BuildRow) string {
	switch row.Phase {
	case wire.PhaseRunning, wire.PhasePublishing:
		return m.spin.View()
	case wire.PhaseDone:
		if row.Cached {
			return styleDim.Render("✓")
		}
		return styleGreen.Render("✓")
	case wire.PhaseFailed, wire.PhaseUpstream:
		return styleRed.Render("✗")
	case wire.PhaseQueued:
		return "◦"
	case wire.PhaseCancelled:
		return styleDim.Render("∅")
	}
	return styleDim.Render("·")
}

// rightLines is the selected row's title over its log viewport: the image's
// name+state for image rows, the node's label+phase (and "no output" when
// the node has none) for graph rows.
func (m model) rightLines(w, h int) []string {
	lines := make([]string, h)
	if m.selected < len(m.visible) {
		v := m.visible[m.selected]
		r := m.rows[v.img]
		var title string
		if v.tree == nil {
			title = " " + r.name + " — " + string(r.state)
			if r.info != "" {
				title += " · " + r.info
			}
		} else if row := m.graphRow(v); row != nil {
			label := row.Label
			if label == "" {
				label = shortNodeName(row.Node)
			}
			title = " " + r.name + " › " + label + " — " + row.Phase
			if d := m.nodeDetail(&m.rows[v.img], row); d != "" {
				title += " · " + d
			}
			if row.LogNode == "" {
				title += " · (no output for this row)"
			}
		}
		lines[0] = truncLine(title, w)
	}
	vpLines := strings.Split(m.vp.View(), "\n")
	for i := 1; i < h; i++ {
		if i-1 < len(vpLines) {
			lines[i] = vpLines[i-1]
		}
	}
	return lines
}

func (m model) footer() string {
	done := 0
	for _, r := range m.rows {
		if r.state.Terminal() {
			done++
		}
	}
	base := fmt.Sprintf(" %d/%d done", done, len(m.rows))
	var line string
	if m.cancelling {
		line = styleDim.Render(base+" · ") + styleRed.Render("cancelling…") +
			styleDim.Render(" (q again to force quit)")
	} else {
		line = styleDim.Render(base + " · ↑/↓ select · ←/→ fold · PgUp/PgDn scroll · q cancel")
	}
	if m.globalNote != "" {
		note := styleDim.Render(truncLine(m.globalNote, m.width/2))
		if gap := m.width - lipgloss.Width(line) - lipgloss.Width(note); gap >= 1 {
			line += strings.Repeat(" ", gap) + note
		}
	}
	return truncLine(line, m.width)
}

// elapsed: negative = not started (pending), frozen at end once terminal.
func (m model) elapsed(r buildRow) time.Duration {
	if r.start.IsZero() {
		return -1
	}
	if r.state.Terminal() {
		return r.end.Sub(r.start)
	}
	return m.clock().Sub(r.start)
}

// fmtElapsed renders a duration in at most 4 cells ("" for not started).
func fmtElapsed(d time.Duration) string {
	if d < 0 {
		return ""
	}
	s := int(d / time.Second)
	switch {
	case s < 100:
		return strconv.Itoa(s) + "s"
	case s < 100*60:
		return strconv.Itoa(s/60) + "m"
	default:
		return strconv.Itoa(min(s/3600, 99)) + "h"
	}
}

func (m model) icon(st spec.BuildState) string {
	switch st {
	case spec.StatePushing:
		return "⇡"
	case spec.StateBuilding:
		return m.spin.View()
	case spec.StateDone:
		return styleGreen.Render("✓")
	case spec.StateFailed:
		return styleRed.Render("✗")
	case spec.StateCancelled:
		return styleDim.Render("∅")
	default: // pending
		return styleDim.Render("·")
	}
}

func clampInt(v, lo, hi int) int {
	return min(max(v, lo), hi)
}
