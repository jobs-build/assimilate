// Package ui renders build progress: a bubbletea TUI on a PTY, plain
// prefixed lines otherwise. Both consume the same spec.Event stream and
// return when the stream closes.
package ui

import (
	"context"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jobs-build/assimilate/internal/spec"
)

// RunTUI runs the full-screen TUI until events closes (all builds settled)
// or the user quits. Layout: left, the build list in appearance order with
// live state and elapsed time, ↑/↓ selection; right, the selected build's
// log tail (per-build ring buffer, PgUp/PgDn scroll). 'q'/ctrl-c invoke
// cancel (the orchestrator then cancels builds and the event stream drains
// and closes — the TUI never exits before the stream ends, so terminal
// state is consistent); a second 'q'/ctrl-c while cancelling force-quits
// without waiting for the drain. A footer shows done/total and key help.
func RunTUI(ctx context.Context, cancel context.CancelFunc, names []string, events <-chan spec.Event) error {
	m := newModel(names, cancel, time.Now)
	// The program's lifetime is bound to the event stream, not to ctx:
	// cancel cancels ctx itself, so a ctx-killed program would vanish
	// before the stream drains (and before "cancelling…" ever renders).
	// WithoutCancel keeps ctx values without inheriting its cancellation.
	p := tea.NewProgram(m,
		tea.WithContext(context.WithoutCancel(ctx)),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithFilter(quitFilter),
	)

	// Pump: every event becomes a msg; the channel close becomes the quit
	// sentinel. Send on a finished program is a no-op, so the pump keeps
	// draining even if the program dies early — the orchestrator can never
	// block on a full events channel.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			p.Send(eventMsg{ev})
		}
		p.Send(closedMsg{})
	}()

	mdl, err := p.Run()
	cancel() // no-op after a normal close; unwinds the orchestrator if the UI died early
	if mm, ok := mdl.(model); ok && mm.forceQuit {
		// Force quit: the user gave up on the drain because the stream never
		// closed. cancel() above bounds the orchestrator's drain, so events
		// should close shortly — wait briefly, then return anyway instead of
		// blocking forever on a wedged producer (the pump keeps draining in
		// the background; Send on a finished program is a no-op).
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		return err
	}
	<-done // stream fully drained — the correctness contract
	return err
}

// quitFilter keeps signals inside the drain contract: SIGINT/SIGTERM become
// the same cancel request the q key issues (a repeat force-quits, mirroring
// the key path), and QuitMsg passes only once the stream has closed — or
// when the user forced a second quit past a stream that never closes.
func quitFilter(mdl tea.Model, msg tea.Msg) tea.Msg {
	switch msg.(type) {
	case tea.InterruptMsg:
		return cancelReq{}
	case tea.QuitMsg:
		if mm, ok := mdl.(model); ok && !mm.closed && !mm.forceQuit {
			return cancelReq{}
		}
	}
	return msg
}

// RunPlain consumes events without a TTY: state transitions and log lines
// as "[name] …" prefixed lines on w, in arrival order. Transient KindInfo
// notes print only when their text changes for that build; global events
// (Build == -1) print bare.
func RunPlain(w io.Writer, names []string, events <-chan spec.Event) {
	lastInfo := map[int]string{}
	prefix := func(ev spec.Event, line string) string {
		if ev.Build < 0 {
			return line
		}
		if ev.Build < len(names) {
			return "[" + names[ev.Build] + "] " + line
		}
		return fmt.Sprintf("[#%d] %s", ev.Build, line)
	}
	for ev := range events {
		switch ev.Kind {
		case spec.KindLog:
			line := ev.Line
			if ev.Node != "" {
				// Node-tagged lines arrive raw; re-add follow.go's classic
				// "kind:key8 │ " prefix so interleaved nodes stay readable.
				line = shortNodeName(ev.Node) + " │ " + line
			}
			fmt.Fprintln(w, prefix(ev, line))
		case spec.KindState:
			line := "▸ " + string(ev.State)
			if ev.Info != "" {
				line += " (" + ev.Info + ")"
			}
			fmt.Fprintln(w, prefix(ev, line))
		case spec.KindInfo:
			if lastInfo[ev.Build] == ev.Info {
				continue
			}
			lastInfo[ev.Build] = ev.Info
			fmt.Fprintln(w, prefix(ev, ev.Info))
		}
	}
}
