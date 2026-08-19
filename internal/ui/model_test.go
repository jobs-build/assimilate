package ui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"

	"github.com/jobs-build/assimilate/internal/spec"
)

// fakeClock is a manually advanced time source.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock               { return &fakeClock{t: time.Unix(1_000_000, 0)} }

// newTestModel builds a model with a counting cancel func and a fake clock
// (both optional).
func newTestModel(t *testing.T, names []string, cancelled *int, clk *fakeClock) model {
	t.Helper()
	cancel := func() {
		if cancelled != nil {
			*cancelled++
		}
	}
	now := time.Now
	if clk != nil {
		now = clk.now
	}
	return newModel(names, cancel, now)
}

func drive(m model, msgs ...tea.Msg) model {
	for _, msg := range msgs {
		m, _ = m.update(msg)
	}
	return m
}

func sizeMsg(w, h int) tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} }

func key(s string) tea.Msg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func stateEv(build int, st spec.BuildState) tea.Msg {
	return eventMsg{spec.Event{Build: build, Kind: spec.KindState, State: st}}
}

func logEv(build int, line string) tea.Msg {
	return eventMsg{spec.Event{Build: build, Kind: spec.KindLog, Line: line}}
}

func infoEv(build int, info string) tea.Msg {
	return eventMsg{spec.Event{Build: build, Kind: spec.KindInfo, Info: info}}
}

func TestSelectionMovement(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		want int
	}{
		{"initial", nil, 0},
		{"down", []string{"down"}, 1},
		{"j alias", []string{"j"}, 1},
		{"down twice", []string{"down", "down"}, 2},
		{"clamped at bottom", []string{"down", "down", "down", "down"}, 2},
		{"up from top clamps", []string{"up"}, 0},
		{"k alias", []string{"down", "down", "k"}, 1},
		{"round trip", []string{"j", "j", "k", "k"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel(t, []string{"a", "b", "c"}, nil, nil)
			m = drive(m, sizeMsg(80, 24))
			for _, k := range tt.keys {
				m = drive(m, key(k))
			}
			if m.selected != tt.want {
				t.Errorf("selected = %d, want %d", m.selected, tt.want)
			}
		})
	}
}

// Selecting another build swaps the viewport to its ring and re-follows.
func TestSelectionSwapsLog(t *testing.T) {
	m := newTestModel(t, []string{"a", "b"}, nil, nil)
	m = drive(m, sizeMsg(80, 24), logEv(0, "from a"), logEv(1, "from b"))
	if v := m.View(); !strings.Contains(v, "from a") || strings.Contains(v, "from b") {
		t.Fatalf("expected build a's log only, got:\n%s", v)
	}
	m = drive(m, key("down"))
	if v := m.View(); !strings.Contains(v, "from b") || strings.Contains(v, "from a") {
		t.Fatalf("expected build b's log only, got:\n%s", v)
	}
	if !m.follow {
		t.Error("selection change must re-enable follow")
	}
}

func TestFollowTail(t *testing.T) {
	m := newTestModel(t, []string{"a"}, nil, nil)
	m = drive(m, sizeMsg(80, 10)) // vp.Height = 8
	for i := 0; i < 30; i++ {
		m = drive(m, logEv(0, "line "+itoa(i)))
	}
	if !m.follow || !m.vp.AtBottom() {
		t.Fatal("expected follow at bottom after log burst")
	}
	if v := m.View(); !strings.Contains(v, "line "+itoa(29)) {
		t.Fatalf("tail line not visible:\n%s", v)
	}

	// Scrolling up pauses follow; new lines must not move the view.
	m = drive(m, key("pgup"))
	if m.follow {
		t.Fatal("pgup must pause follow")
	}
	off := m.vp.YOffset
	m = drive(m, logEv(0, "line 30"), logEv(0, "line 31"))
	if m.vp.YOffset != off {
		t.Errorf("scrolled-up viewport moved: YOffset %d → %d", off, m.vp.YOffset)
	}
	if m.follow {
		t.Error("new lines while scrolled up must not resume follow")
	}

	// Paging back to the bottom resumes following.
	for i := 0; i < 10 && !m.vp.AtBottom(); i++ {
		m = drive(m, key("pgdown"))
	}
	if !m.follow {
		t.Error("returning to the bottom must resume follow")
	}
	m = drive(m, logEv(0, "line 32"))
	if !m.vp.AtBottom() {
		t.Error("follow must keep the viewport pinned to the tail")
	}
}

func TestMouseWheelScroll(t *testing.T) {
	m := newTestModel(t, []string{"a"}, nil, nil)
	m = drive(m, sizeMsg(80, 10))
	for i := 0; i < 30; i++ {
		m = drive(m, logEv(0, "line "+itoa(i)))
	}
	m = drive(m, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.follow || m.vp.AtBottom() {
		t.Fatal("wheel-up must scroll and pause follow")
	}
	for i := 0; i < 10 && !m.vp.AtBottom(); i++ {
		m = drive(m, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	}
	if !m.follow {
		t.Error("wheeling back to the bottom must resume follow")
	}
}

func TestCancelOnceAndQuitOnlyAfterClose(t *testing.T) {
	for _, quitKey := range []string{"q", "ctrl+c"} {
		t.Run(quitKey, func(t *testing.T) {
			var cancelled int
			m := newTestModel(t, []string{"a", "b"}, &cancelled, nil)
			m = drive(m, sizeMsg(80, 24), stateEv(0, spec.StateBuilding))

			var cmd tea.Cmd
			m, cmd = m.update(key(quitKey))
			if cancelled != 1 {
				t.Fatalf("cancel called %d times, want 1", cancelled)
			}
			if cmd != nil {
				t.Fatal("quit key must not produce a command before the stream closes")
			}
			if !m.cancelling {
				t.Fatal("cancelling flag not set")
			}
			if !strings.Contains(m.View(), "cancelling…") {
				t.Error("footer must show cancelling…")
			}

			// The stream keeps flowing; still no quit.
			m, cmd = m.update(logEv(0, "draining"))
			if cmd != nil {
				t.Fatal("log events must not quit")
			}
			m, cmd = m.update(stateEv(0, spec.StateCancelled))
			if cmd != nil {
				t.Fatal("state events must not quit")
			}

			// Only the closed sentinel quits.
			_, cmd = m.update(closedMsg{})
			if cmd == nil {
				t.Fatal("closedMsg must quit")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatalf("closedMsg command = %T, want tea.QuitMsg", cmd())
			}
		})
	}
}

// Normal completion: the stream closes without any user input → quit.
func TestQuitOnNormalClose(t *testing.T) {
	var cancelled int
	m := newTestModel(t, []string{"a"}, &cancelled, nil)
	m = drive(m, sizeMsg(80, 24), stateEv(0, spec.StateBuilding), stateEv(0, spec.StateDone))
	m, cmd := m.update(closedMsg{})
	if cmd == nil {
		t.Fatal("closedMsg must quit")
	}
	if !m.closed {
		t.Error("closed flag not set")
	}
	if cancelled != 0 {
		t.Errorf("cancel called %d times on normal completion, want 0", cancelled)
	}
}

func TestElapsedAndIcons(t *testing.T) {
	clk := newFakeClock()
	m := newTestModel(t, []string{"api", "worker", "web", "old"}, nil, clk)
	m = drive(m, sizeMsg(80, 24))

	m = drive(m, stateEv(0, spec.StatePushing))
	clk.advance(5 * time.Second)
	m = drive(m, stateEv(0, spec.StateBuilding))
	clk.advance(7 * time.Second)
	m = drive(m, stateEv(0, spec.StateDone))
	m = drive(m, stateEv(1, spec.StatePushing))
	m = drive(m, eventMsg{spec.Event{Build: 2, Kind: spec.KindState, State: spec.StateFailed, Info: "exit 1"}})
	m = drive(m, stateEv(3, spec.StateCancelled))

	clk.advance(100 * time.Second) // terminal elapsed must stay frozen
	v := m.View()
	for _, want := range []string{"✓", "⇡", "✗", "∅", "12s", "exit 1"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	if e := m.elapsed(m.rows[0]); e != 12*time.Second {
		t.Errorf("done elapsed = %v, want 12s (frozen at terminal)", e)
	}
	if e := m.elapsed(m.rows[1]); e != 100*time.Second {
		t.Errorf("running elapsed = %v, want 100s (live)", e)
	}
}

func TestPendingHasNoElapsed(t *testing.T) {
	clk := newFakeClock()
	m := newTestModel(t, []string{"a"}, nil, clk)
	if e := m.elapsed(m.rows[0]); e >= 0 {
		t.Errorf("pending elapsed = %v, want negative (not started)", e)
	}
}

func TestFmtElapsed(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, ""},
		{0, "0s"},
		{42 * time.Second, "42s"},
		{99 * time.Second, "99s"},
		{100 * time.Second, "1m"},
		{99 * time.Minute, "99m"},
		{100 * time.Minute, "1h"},
		{200 * time.Hour, "99h"},
	}
	for _, tt := range tests {
		if got := fmtElapsed(tt.d); got != tt.want {
			t.Errorf("fmtElapsed(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestTickerLifecycle(t *testing.T) {
	m := newTestModel(t, []string{"a", "b"}, nil, nil)
	m = drive(m, sizeMsg(80, 24))
	if m.ticking {
		t.Fatal("ticker must not run while everything is pending")
	}

	var cmd tea.Cmd
	m, cmd = m.update(stateEv(0, spec.StatePushing))
	if !m.ticking || cmd == nil {
		t.Fatal("activating a build must arm the ticker")
	}
	// A second activation must not double-arm.
	m, cmd = m.update(stateEv(1, spec.StateBuilding))
	// (cmd may arm the spinner; the ticker flag stays set.)
	if !m.ticking {
		t.Fatal("ticker disarmed by second activation")
	}

	// Active builds re-arm the tick.
	m, cmd = m.update(tickMsg(time.Now()))
	if cmd == nil || !m.ticking {
		t.Fatal("tick must re-arm while builds are active")
	}

	// All settled: the tick dies out.
	m = drive(m, stateEv(0, spec.StateDone), stateEv(1, spec.StateFailed))
	m, cmd = m.update(tickMsg(time.Now()))
	if cmd != nil || m.ticking {
		t.Fatal("tick must stop once no build is active")
	}

	// A late activation re-arms it.
	m, cmd = m.update(stateEv(0, spec.StatePushing))
	if cmd == nil || !m.ticking {
		t.Fatal("re-activation must re-arm the ticker")
	}
}

func TestSpinnerLifecycle(t *testing.T) {
	m := newTestModel(t, []string{"a"}, nil, nil)
	m = drive(m, sizeMsg(80, 24))

	var cmd tea.Cmd
	m, cmd = m.update(stateEv(0, spec.StatePushing))
	if m.spinning {
		t.Fatal("pushing must not arm the spinner")
	}
	m, cmd = m.update(stateEv(0, spec.StateBuilding))
	if !m.spinning || cmd == nil {
		t.Fatal("building must arm the spinner")
	}
	m, cmd = m.update(spinner.TickMsg{Time: time.Now(), ID: m.spin.ID()})
	if cmd == nil {
		t.Fatal("spinner tick must re-arm while building")
	}
	m = drive(m, stateEv(0, spec.StateDone))
	m, cmd = m.update(spinner.TickMsg{Time: time.Now(), ID: m.spin.ID()})
	if cmd != nil || m.spinning {
		t.Fatal("spinner must stop once nothing is building")
	}
}

func TestViewBeforeFirstSize(t *testing.T) {
	m := newTestModel(t, []string{"a", "b"}, nil, nil)
	// Events and keys before the first WindowSizeMsg must not crash.
	m = drive(m, logEv(0, "early"), stateEv(0, spec.StateBuilding), key("down"), key("pgup"))
	if v := m.View(); v == "" {
		t.Fatal("pre-size view must render something")
	}
	// The early log line appears once the size arrives.
	m = drive(m, sizeMsg(80, 24), key("up"))
	if v := m.View(); !strings.Contains(v, "early") {
		t.Fatalf("early log line lost:\n%s", v)
	}
}

func TestEmptyBuildList(t *testing.T) {
	m := newTestModel(t, nil, nil, nil)
	m = drive(m, sizeMsg(80, 24), key("down"), key("up"), logEv(0, "x"))
	if v := m.View(); !strings.Contains(v, "0/0 done") {
		t.Fatalf("empty list view:\n%s", v)
	}
}

func TestLineWidthAndTruncation(t *testing.T) {
	const width = 44
	m := newTestModel(t, []string{"a-very-long-build-name-that-cannot-possibly-fit linux/amd64"}, nil, nil)
	m = drive(m, sizeMsg(width, 10),
		logEv(0, strings.Repeat("wide ", 60)),
		infoEv(0, strings.Repeat("progress ", 20)),
		eventMsg{spec.Event{Build: -1, Kind: spec.KindInfo, Info: strings.Repeat("note ", 30)}},
	)
	for i, line := range strings.Split(m.View(), "\n") {
		if w := lipgloss.Width(line); w > width {
			t.Errorf("line %d overflows: width %d > %d: %q", i, w, width, line)
		}
	}
}

func TestLeftPaneWidthClamp(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		termW int
		want  int
	}{
		{"short names clamp to min", []string{"ab"}, 120, minLeftW},
		{"fits", []string{strings.Repeat("n", 20)}, 120, 20 + rowOverhead},
		{"long names clamp to max", []string{strings.Repeat("n", 60)}, 120, maxLeftW},
		{"narrow terminal halves", []string{strings.Repeat("n", 60)}, 40, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel(t, tt.names, nil, nil)
			m = drive(m, sizeMsg(tt.termW, 24))
			if got := m.leftWidth(); got != tt.want {
				t.Errorf("leftWidth() = %d, want %d", got, tt.want)
			}
			if wantVP := tt.termW - tt.want - 1; m.vp.Width != wantVP {
				t.Errorf("vp.Width = %d, want %d", m.vp.Width, wantVP)
			}
		})
	}
}

func TestInfoShown(t *testing.T) {
	m := newTestModel(t, []string{"api"}, nil, nil)
	m = drive(m, sizeMsg(80, 24), stateEv(0, spec.StatePushing), infoEv(0, "push 45%"))
	if v := m.View(); !strings.Contains(v, "push 45%") {
		t.Fatalf("info note not shown:\n%s", v)
	}
}

func TestGlobalNoteInFooter(t *testing.T) {
	m := newTestModel(t, []string{"api"}, nil, nil)
	m = drive(m, sizeMsg(80, 24), eventMsg{spec.Event{Build: -1, Kind: spec.KindInfo, Info: "3 sources pushed"}})
	if v := m.View(); !strings.Contains(v, "3 sources pushed") {
		t.Fatalf("global note not shown:\n%s", v)
	}
}

func TestFooterCounts(t *testing.T) {
	m := newTestModel(t, []string{"a", "b", "c"}, nil, nil)
	m = drive(m, sizeMsg(80, 24))
	if v := m.View(); !strings.Contains(v, "0/3 done") {
		t.Fatalf("footer:\n%s", v)
	}
	m = drive(m, stateEv(0, spec.StateDone), stateEv(1, spec.StateFailed))
	if v := m.View(); !strings.Contains(v, "2/3 done") {
		t.Fatalf("footer after two terminals:\n%s", v)
	}
}

// A second quit request while cancelling must force-quit immediately —
// the escape hatch for a stream that never closes — with cancel still
// fired exactly once.
func TestForceQuitEscapeHatch(t *testing.T) {
	tests := []struct {
		name          string
		first, second tea.Msg
	}{
		{"q q", key("q"), key("q")},
		{"ctrl+c ctrl+c", key("ctrl+c"), key("ctrl+c")},
		{"sigint sigint", cancelReq{}, cancelReq{}},
		{"q then sigint", key("q"), cancelReq{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cancelled int
			m := newTestModel(t, []string{"a"}, &cancelled, nil)
			m = drive(m, sizeMsg(80, 24), stateEv(0, spec.StateBuilding))

			var cmd tea.Cmd
			m, cmd = m.update(tt.first)
			if cmd != nil {
				t.Fatal("first quit request must not quit (drain contract)")
			}
			if m.forceQuit {
				t.Fatal("first quit request must not force-quit")
			}
			if !strings.Contains(m.View(), "(q again to force quit)") {
				t.Error("footer must advertise the force-quit escape hatch")
			}

			m, cmd = m.update(tt.second)
			if !m.forceQuit {
				t.Fatal("second quit request must set forceQuit")
			}
			if cmd == nil {
				t.Fatal("second quit request must emit a command")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatalf("second quit request command = %T, want tea.QuitMsg", cmd())
			}
			if cancelled != 1 {
				t.Fatalf("cancel called %d times, want 1", cancelled)
			}
		})
	}
}

// quitFilter must suppress QuitMsg while the stream is open unless the
// user force-quit; InterruptMsg always becomes a cancel request.
func TestQuitFilter(t *testing.T) {
	m := newTestModel(t, []string{"a"}, nil, nil)
	if _, ok := quitFilter(m, tea.QuitMsg{}).(cancelReq); !ok {
		t.Error("QuitMsg while the stream is open must become cancelReq")
	}
	if _, ok := quitFilter(m, tea.InterruptMsg{}).(cancelReq); !ok {
		t.Error("InterruptMsg must become cancelReq")
	}
	forced := m
	forced.forceQuit = true
	if _, ok := quitFilter(forced, tea.QuitMsg{}).(tea.QuitMsg); !ok {
		t.Error("QuitMsg after forceQuit must pass through")
	}
	closed := m
	closed.closed = true
	if _, ok := quitFilter(closed, tea.QuitMsg{}).(tea.QuitMsg); !ok {
		t.Error("QuitMsg after close must pass through")
	}
	if _, ok := quitFilter(m, key("q")).(tea.KeyMsg); !ok {
		t.Error("other messages must pass through unchanged")
	}
}

func TestSanitizeLine(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "go build ./...", "go build ./..."},
		{"cr progress keeps last", "10%\r50%\r100% done", "100% done"},
		{"trailing cr trimmed", "pushing\r", "pushing"},
		{"trailing crlf trimmed", "pushing\r\n", "pushing"},
		{"csi erase stripped", "\x1b[2K\x1b[1Gstep 3", "step 3"},
		{"cursor movement stripped", "\x1b[3A\x1b[10Dcompiling", "compiling"},
		{"sgr stripped", "\x1b[1;31merror\x1b[0m: boom", "error: boom"},
		{"unterminated sgr stripped", "\x1b[31mred bleeds", "red bleeds"},
		{"osc bel stripped", "\x1b]0;title\x07after", "after"},
		{"osc st stripped", "\x1b]8;;https://x\x1b\\link", "link"},
		{"private mode toggle stripped", "\x1b[?25lhidden cursor", "hidden cursor"},
		{"tab expands to stop", "a\tb", "a       b"},
		{"tab at stop advances a full stop", "12345678\tb", "12345678        b"},
		{"c0 dropped", "bell\a and\bback", "bell andback"},
		{"del dropped", "a\x7fb", "ab"},
		{"cr then escapes", "old\r\x1b[2Knew", "new"},
		{"empty", "", ""},
		{"only cr", "\r", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeLine(tt.in); got != tt.want {
				t.Errorf("sanitizeLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Raw escape bytes must never survive into the ring or the global note.
func TestLogEscapesNeverReachRing(t *testing.T) {
	m := newTestModel(t, []string{"a"}, nil, nil)
	m = drive(m, sizeMsg(80, 10),
		logEv(0, "\x1b[2J\x1b[Hwiped"),
		logEv(0, "#6 [3/7] RUN make\r#6 [4/7] RUN test\x1b[0m"),
		eventMsg{spec.Event{Build: -1, Kind: spec.KindLog, Line: "\x1b]0;evil\x07global"}},
	)
	lines := m.rows[0].log.lines()
	want := []string{"wiped", "#6 [4/7] RUN test"}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("ring[%d] = %q, want %q", i, lines[i], w)
		}
	}
	if m.globalNote != "global" {
		t.Errorf("globalNote = %q, want %q", m.globalNote, "global")
	}
	if v := m.View(); !strings.Contains(v, "wiped") {
		t.Fatalf("sanitized line missing from view:\n%s", v)
	}
}

// A terminal transition without Info clears a stale transient note; a
// failed row keeps its detail (or its prior note when the event has none).
func TestTerminalClearsStaleTransientInfo(t *testing.T) {
	m := newTestModel(t, []string{"a", "b", "c", "d", "e"}, nil, nil)
	m = drive(m, sizeMsg(100, 24),
		infoEv(0, "push 3/7 objects"), stateEv(0, spec.StateDone),
		infoEv(1, "1 running"), stateEv(1, spec.StateCancelled),
		infoEv(2, "push 3/7 objects"),
		eventMsg{spec.Event{Build: 2, Kind: spec.KindState, State: spec.StateFailed, Info: "exit 1"}},
		infoEv(3, "push 3/7 objects"), stateEv(3, spec.StateFailed),
		infoEv(4, "push 5/7 objects"),
		eventMsg{spec.Event{Build: 4, Kind: spec.KindState, State: spec.StateDone, Info: "cached"}},
	)
	if got := m.rows[0].info; got != "" {
		t.Errorf("done row info = %q, want cleared", got)
	}
	if got := m.rows[1].info; got != "" {
		t.Errorf("cancelled row info = %q, want cleared", got)
	}
	if got := m.rows[2].info; got != "exit 1" {
		t.Errorf("failed row info = %q, want the failure detail kept", got)
	}
	if got := m.rows[3].info; got != "push 3/7 objects" {
		t.Errorf("failed row without detail info = %q, want prior note kept", got)
	}
	if got := m.rows[4].info; got != "cached" {
		t.Errorf("done row with info = %q, want %q", got, "cached")
	}
}

// While a tick is armed, appended log lines only mark the view dirty; the
// O(ring) rebuild happens once per tick, and the tail still follows.
func TestLogRebuildCoalescedToTick(t *testing.T) {
	m := newTestModel(t, []string{"a"}, nil, nil)
	m = drive(m, sizeMsg(80, 10), stateEv(0, spec.StateBuilding))
	base := m.refreshes
	for i := 0; i < 50; i++ {
		m = drive(m, logEv(0, "line "+itoa(i)))
	}
	if m.refreshes != base {
		t.Fatalf("refreshes = %d during log burst, want %d (coalesced)", m.refreshes, base)
	}
	if !m.logDirty {
		t.Fatal("log burst must mark the view dirty")
	}
	m = drive(m, tickMsg(time.Now()))
	if m.refreshes != base+1 {
		t.Fatalf("refreshes = %d after tick, want %d", m.refreshes, base+1)
	}
	if m.logDirty {
		t.Fatal("tick must clear the dirty flag")
	}
	if !m.vp.AtBottom() {
		t.Error("tail must follow after the flush")
	}
	if v := m.View(); !strings.Contains(v, "line 49") {
		t.Fatalf("tail line missing after tick:\n%s", v)
	}
	// A tick with nothing dirty must not rebuild.
	m = drive(m, tickMsg(time.Now()))
	if m.refreshes != base+1 {
		t.Fatalf("clean tick rebuilt: refreshes = %d, want %d", m.refreshes, base+1)
	}
	// The spinner tick flushes too.
	m = drive(m, logEv(0, "line 50"))
	m = drive(m, spinner.TickMsg{Time: time.Now(), ID: m.spin.ID()})
	if m.refreshes != base+2 || m.logDirty {
		t.Fatalf("spinner tick must flush: refreshes = %d, dirty = %v", m.refreshes, m.logDirty)
	}
}

// With no tick armed (nothing active) a log line rebuilds immediately so
// the tail never stalls.
func TestLogRebuildImmediateWhenIdle(t *testing.T) {
	m := newTestModel(t, []string{"a"}, nil, nil)
	m = drive(m, sizeMsg(80, 10))
	base := m.refreshes
	m = drive(m, logEv(0, "hello"))
	if m.refreshes != base+1 || m.logDirty {
		t.Fatalf("idle log line must rebuild now: refreshes = %d (base %d), dirty = %v",
			m.refreshes, base, m.logDirty)
	}
	if v := m.View(); !strings.Contains(v, "hello") {
		t.Fatalf("line missing from view:\n%s", v)
	}
}

// Terminal transitions and closedMsg flush a pending rebuild — the ticks
// that would have flushed it may never come.
func TestLogFlushOnTerminalAndClose(t *testing.T) {
	m := newTestModel(t, []string{"a"}, nil, nil)
	m = drive(m, sizeMsg(80, 10), stateEv(0, spec.StateBuilding), logEv(0, "final line"))
	if !m.logDirty {
		t.Fatal("log line while ticking must defer the rebuild")
	}
	m = drive(m, stateEv(0, spec.StateDone))
	if m.logDirty {
		t.Fatal("terminal transition must flush the pending rebuild")
	}
	if v := m.View(); !strings.Contains(v, "final line") {
		t.Fatalf("final line missing after terminal flush:\n%s", v)
	}

	m = drive(m, stateEv(0, spec.StateBuilding), logEv(0, "very last"))
	if !m.logDirty {
		t.Fatal("log line while ticking must defer the rebuild")
	}
	m = drive(m, closedMsg{})
	if m.logDirty {
		t.Fatal("closedMsg must flush the pending rebuild")
	}
	if v := m.View(); !strings.Contains(v, "very last") {
		t.Fatalf("last line missing after close flush:\n%s", v)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// --- two-level tree (image → build graph) ---

// nn mints a parseable node name: kind_<64-hex>.
func nn(kind string, i int) string { return kind + "_" + fmt.Sprintf("%064x", i) }

// graphSnap is a one-build closure: buildvalue "app" whose chain runs a
// buildrun, with a cached import child under the buildfrom.
func graphSnap() *api.Snapshot {
	bv, bf := nn("buildvalue", 1), nn("buildfrom", 1)
	br, imp := nn("buildrun", 2), nn("import", 3)
	return &api.Snapshot{
		Phase: "running",
		Nodes: []api.NodeSnap{
			{Node: bv, Label: "app", Phase: wire.PhaseWaiting, Deps: []string{bf, br}},
			{Node: bf, Phase: wire.PhaseDone, Deps: []string{imp}},
			{Node: br, Phase: wire.PhaseRunning, ElapsedMs: 41000, Runner: "r-a"},
			{Node: imp, Label: "fetch github", Phase: wire.PhaseDone, Cached: true},
		},
	}
}

func snapEv(build int, snap *api.Snapshot) tea.Msg {
	return eventMsg{spec.Event{Build: build, Kind: spec.KindSnapshot, Snap: snap}}
}

func nodeLogEv(build int, node, line string) tea.Msg {
	return eventMsg{spec.Event{Build: build, Kind: spec.KindLog, Node: node, Line: line}}
}

// TestTreeExpandCollapse: a snapshot makes the image expandable; expanding
// reveals the folded build-graph rows; collapsing hides them again.
func TestTreeExpandCollapse(t *testing.T) {
	m := newTestModel(t, []string{"img-a", "img-b"}, nil, nil)
	m = drive(m, sizeMsg(100, 24), stateEv(0, spec.StateBuilding), snapEv(0, graphSnap()))
	if len(m.visible) != 2 {
		t.Fatalf("visible before expand = %d rows, want the 2 image rows", len(m.visible))
	}
	if !m.expandable() {
		t.Fatal("image with a graph-bearing snapshot must be expandable")
	}

	m = drive(m, key("right"))
	// image-a + its root buildvalue + import child (cached-done default-
	// collapsed children are still VISIBLE as rows, just not expanded
	// further) + image-b.
	if len(m.visible) != 4 {
		t.Fatalf("visible after expand = %d rows (%+v), want 4", len(m.visible), m.visible)
	}
	if m.visible[1].tree == nil || m.visible[2].tree == nil {
		t.Fatalf("rows 1-2 must be graph rows: %+v", m.visible)
	}
	v := m.View()
	for _, want := range []string{"app", "build 41s", "fetch github", "(cached)"} {
		if !strings.Contains(v, want) {
			t.Errorf("expanded view missing %q:\n%s", want, v)
		}
	}

	m = drive(m, key("left"))
	if len(m.visible) != 2 {
		t.Fatalf("visible after collapse = %d rows, want 2", len(m.visible))
	}
}

// TestNodeLogRouting: node-tagged lines land in the node's own ring (raw)
// AND the image's combined ring (prefixed); selecting the node row shows
// only its output.
func TestNodeLogRouting(t *testing.T) {
	m := newTestModel(t, []string{"img"}, nil, nil)
	br := nn("buildrun", 2)
	m = drive(m, sizeMsg(100, 24), stateEv(0, spec.StateBuilding), snapEv(0, graphSnap()),
		nodeLogEv(0, br, "compiling world"),
		logEv(0, "build-level note"))

	// Image row selected: the combined ring shows both, node line prefixed.
	m.flushLog()
	v := m.View()
	if !strings.Contains(v, "buildrun:0000000000000002"[:16]+" │ compiling world") && !strings.Contains(v, " │ compiling world") {
		t.Fatalf("combined view missing prefixed node line:\n%s", v)
	}
	if !strings.Contains(v, "build-level note") {
		t.Fatalf("combined view missing the build-level line:\n%s", v)
	}

	// Select the root buildvalue row (its LogNode is the running buildrun).
	m = drive(m, key("right"), key("down"))
	if m.visible[m.selected].tree == nil {
		t.Fatalf("selection is not a graph row: %+v", m.visible[m.selected])
	}
	v = m.View()
	if !strings.Contains(v, "compiling world") {
		t.Fatalf("node pane missing its output:\n%s", v)
	}
	if strings.Contains(v, "build-level note") {
		t.Fatalf("node pane leaked the combined ring:\n%s", v)
	}
	if !strings.Contains(v, "img › app") {
		t.Fatalf("node pane title missing 'img › app':\n%s", v)
	}
}

// TestFailureAutoExpands: a terminal failure unfolds the image so the
// failing node is visible.
func TestFailureAutoExpands(t *testing.T) {
	m := newTestModel(t, []string{"img"}, nil, nil)
	snap := graphSnap()
	snap.Nodes[2].Phase, snap.Nodes[2].ErrSummary = wire.PhaseFailed, "boom"
	snap.Nodes[0].Phase = wire.PhaseUpstream
	m = drive(m, sizeMsg(100, 24), stateEv(0, spec.StateBuilding), snapEv(0, snap),
		eventMsg{spec.Event{Build: 0, Kind: spec.KindState, State: spec.StateFailed, Info: "build failed"}})
	if !m.rows[0].expanded {
		t.Fatal("failed image did not auto-expand")
	}
	if v := m.View(); !strings.Contains(v, "build: boom") {
		t.Fatalf("failed node detail missing:\n%s", v)
	}
}

// TestSelectionSurvivesSnapshotRefold: a fresh snapshot for an expanded
// image keeps the selection on the same path.
func TestSelectionSurvivesSnapshotRefold(t *testing.T) {
	m := newTestModel(t, []string{"img"}, nil, nil)
	m = drive(m, sizeMsg(100, 24), stateEv(0, spec.StateBuilding), snapEv(0, graphSnap()),
		key("right"), key("down"), key("down"))
	wantPath := m.visible[m.selected].tree.Path

	next := graphSnap()
	next.Nodes[2].ElapsedMs = 99000
	m = drive(m, snapEv(0, next))
	if got := m.visible[m.selected].tree; got == nil || got.Path != wantPath {
		t.Fatalf("selection moved after refold: %+v, want path %q", got, wantPath)
	}
}

// TestOldServerNotExpandable: a multi-node snapshot without Deps (old
// server) never makes the image expandable.
func TestOldServerNotExpandable(t *testing.T) {
	m := newTestModel(t, []string{"img"}, nil, nil)
	snap := &api.Snapshot{Phase: "running", Nodes: []api.NodeSnap{
		{Node: nn("buildvalue", 1), Phase: wire.PhaseWaiting},
		{Node: nn("buildrun", 2), Phase: wire.PhaseRunning},
	}}
	m = drive(m, sizeMsg(100, 24), stateEv(0, spec.StateBuilding), snapEv(0, snap))
	if m.expandable() {
		t.Fatal("old-server snapshot (no deps) must not be expandable")
	}
	m = drive(m, key("right"))
	if len(m.visible) != 1 {
		t.Fatalf("visible = %d rows, want just the image row", len(m.visible))
	}
}
