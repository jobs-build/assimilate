package jobs

// The watch/log-follow half of Follow: a snapshot-driven follower set over
// the api logs stream, adapted from jobs-iroh/clientcli/logtracker.go with
// two differences — output goes into a Sink instead of a terminal view, and
// slots are bounded twice: per build AND client-wide across concurrent
// Follows, because every follow is one live QUIC stream on the shared admin
// connection (~100 allowed; N watches + follows + transient calls must fit).

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"

	"github.com/jobs-build/assimilate/internal/spec"
)

// maxFollowNodes caps one build's concurrent log-follow streams; a wide
// fan-out follows at most this many active nodes (running first) and notes
// the rest once.
const maxFollowNodes = 4

// maxClientFollows caps live follow streams across ALL of a Client's
// concurrent Follows.
const maxClientFollows = 24

// followStopGrace delays a finished node's follower teardown: the last
// output chunks are fanned out server-side before the result folds into the
// snapshot, so at "done" they can still be in flight on the QUIC stream —
// cutting the read immediately would lose them.
var followStopGrace = 500 * time.Millisecond

// followLoop drives one watch to its terminal snapshot: per snapshot the raw
// phase + counts summary into the sink (phase mapping is the orchestrator's
// job) and a follower-set sync; on a failed terminal the stored output of
// failing nodes not already streamed. next/lo/lf are the injectable seams
// the tests fake; sem is the client-wide follow budget (nil = unbounded).
func followLoop(ctx context.Context, next func() (api.Snapshot, error), lo logOpener, lf logFetcher, sem chan struct{}, sink Sink) (spec.BuildState, error) {
	fs := newFollowSet(ctx, lo, sem, sink)
	defer fs.close()
	for {
		snap, err := next()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("watch stream: %w", err)
		}
		sink.State(snap.Phase, countsSummary(snap.Counts))
		sink.Snapshot(snap)
		fs.sync(snap)
		if !snap.Terminal {
			continue
		}
		fs.close() // drain trailing output before the verdict
		switch snap.Phase {
		case wire.PhaseDone:
			return spec.StateDone, nil
		case wire.PhaseCancelled:
			return spec.StateCancelled, nil
		case wire.PhaseFailed:
			emitFailureLogs(ctx, lf, snap, sink, fs.streamedNodes())
			return spec.StateFailed, nil
		default:
			return "", fmt.Errorf("watch: unknown terminal phase %q", snap.Phase)
		}
	}
}

// countsSummary renders wire.Counts as the short human summary Sink.State
// carries ("3/7 built · 1 running").
func countsSummary(c wire.Counts) string {
	parts := []string{fmt.Sprintf("%d/%d built", c.Done, c.Total)}
	if c.Running > 0 {
		parts = append(parts, fmt.Sprintf("%d running", c.Running))
	}
	if c.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", c.Failed))
	}
	return strings.Join(parts, " · ")
}

// emitFailureLogs delivers the failure story into the sink: per hard-failed
// node (not the derived failed-upstream ones) a build-level header with the
// ErrSummary, then the stored head/gap/tail view (node-tagged) unless the
// node's output was already streamed live. Best-effort: fetch problems are
// noted and swallowed — the build verdict is already decided.
func emitFailureLogs(ctx context.Context, lf logFetcher, snap api.Snapshot, sink Sink, streamed map[string]bool) {
	for _, n := range snap.Nodes {
		if n.Phase != wire.PhaseFailed {
			continue
		}
		sink.Log("", fmt.Sprintf("--- %s failed (gen %d): %s", shortNode(n.Node), n.Gen, n.ErrSummary))
		if streamed[n.Node] {
			sink.Log("", "(output streamed above)")
			continue
		}
		view, err := lf.fetchLogs(ctx, n.Node)
		if err != nil {
			sink.Log(n.Node, fmt.Sprintf("(logs unavailable: %v)", err))
			continue
		}
		if len(view.Head) == 0 && len(view.Tail) == 0 {
			sink.Log(n.Node, "(no captured output)")
			continue
		}
		p := &linePrinter{sink: sink, node: n.Node}
		p.printView(view)
		p.flush()
	}
}

// shortNode renders a node name as kind:keyprefix for log-line prefixes.
func shortNode(name string) string {
	kind, k, err := wire.ParseNodeName(name)
	if err != nil {
		return name
	}
	return kind + ":" + k.String()[:8]
}

// isActivePhase reports a node whose output is (or is about to be) produced.
func isActivePhase(phase string) bool {
	switch phase {
	case wire.PhaseQueued, wire.PhaseRunning, wire.PhasePublishing:
		return true
	}
	return false
}

// linePrinter assembles one node's output bytes into node-tagged sink lines
// (no prefix — consumers add one where views mix nodes). Chunks cut lines
// anywhere, so a partial line waits in pending until its newline arrives or
// flush is called.
type linePrinter struct {
	sink    Sink
	node    string
	pending []byte
}

func (p *linePrinter) write(data []byte) {
	p.pending = append(p.pending, data...)
	for {
		i := bytes.IndexByte(p.pending, '\n')
		if i < 0 {
			return
		}
		p.println(p.pending[:i])
		p.pending = p.pending[i+1:]
	}
}

// flush emits any trailing unterminated line.
func (p *linePrinter) flush() {
	if len(p.pending) > 0 {
		p.println(p.pending)
		p.pending = nil
	}
}

func (p *linePrinter) println(line []byte) {
	p.sink.Log(p.node, string(bytes.TrimSuffix(line, []byte("\r"))))
}

// printView renders a stored head/gap/tail view through the printer.
func (p *linePrinter) printView(view api.LogView) {
	p.write(view.Head)
	if view.GapSize > 0 {
		p.flush()
		p.sink.Log(p.node, fmt.Sprintf("... [%d bytes omitted] ...", view.GapSize))
	}
	p.write(view.Tail)
}

// followSet streams the output of one watch's active nodes: sync diffs each
// snapshot's active set against the follower set, opening a follow stream
// per newly active node (per-build cap and client-wide budget permitting)
// and grace-stopping followers whose nodes left. Output lines are
// best-effort observability (the fan-out is lossy by contract) — they never
// decide the build verdict.
type followSet struct {
	ctx  context.Context
	open logOpener
	sem  chan struct{} // client-wide follow budget; nil = unbounded
	sink Sink

	mu       sync.Mutex
	active   map[string]context.CancelFunc
	followed map[string]bool // nodes whose follow stream opened, for the failure recap
	closed   bool
	capNoted bool
	wg       sync.WaitGroup
}

func newFollowSet(ctx context.Context, open logOpener, sem chan struct{}, sink Sink) *followSet {
	return &followSet{
		ctx: ctx, open: open, sem: sem, sink: sink,
		active:   map[string]context.CancelFunc{},
		followed: map[string]bool{},
	}
}

// sync folds one snapshot into the follower set. Surviving followers keep
// their slots before new nodes claim any, so the caps never churn an
// attached stream out.
//
// Every active-phase node is a candidate, not just running ones: snapshots
// are coalesced (≤4/s), so a fast job can pass through running entirely
// between two snapshots and only ever be seen queued — attaching at queued
// catches its output from the first chunk. Running nodes claim free slots
// first, so a wide fan-out's waiting jobs can't starve the streams that are
// actually producing output.
func (f *followSet) sync(snap api.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	active := map[string]bool{}
	for _, n := range snap.Nodes {
		if isActivePhase(n.Phase) {
			active[n.Node] = true
		}
	}
	slots := 0
	for node, cancel := range f.active {
		if active[node] {
			slots++
			continue
		}
		delete(f.active, node)
		time.AfterFunc(followStopGrace, cancel)
	}
	for _, runningPass := range []bool{true, false} {
		for _, n := range snap.Nodes {
			if !active[n.Node] || (n.Phase == wire.PhaseRunning) != runningPass ||
				f.active[n.Node] != nil {
				continue
			}
			if slots >= maxFollowNodes || !f.acquire() {
				f.noteCapLocked()
				continue
			}
			slots++
			f.followLocked(n.Node)
		}
	}
}

// acquire claims one client-wide follow slot without blocking: sync must
// never stall this build's watch loop on other builds' followers.
func (f *followSet) acquire() bool {
	if f.sem == nil {
		return true
	}
	select {
	case f.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns a follower's client-wide slot; called only after its
// stream is fully terminated (the slot tracks live streams, not nodes).
func (f *followSet) release() {
	if f.sem != nil {
		<-f.sem
	}
}

func (f *followSet) noteCapLocked() {
	if f.capNoted {
		return
	}
	f.capNoted = true
	f.sink.Log("", "[logs] follow cap reached — more steps are active, not all output is streamed")
}

func (f *followSet) followLocked(node string) {
	fctx, cancel := context.WithCancel(f.ctx)
	f.active[node] = cancel
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		defer f.release()
		defer cancel()
		f.runFollower(fctx, node)
	}()
}

// markStreamed records a node whose follow stream actually opened; only
// these skip the stored-log fetch in the failure recap — a node whose open
// failed streamed nothing, so the recap must still fetch its logs.
func (f *followSet) markStreamed(node string) {
	f.mu.Lock()
	f.followed[node] = true
	f.mu.Unlock()
}

// runFollower feeds one node's stored view then live chunks into the sink
// until the stream ends (grace stop, watch teardown, server close). A chunk
// for a newer gen means the attempt was retried: flush, mark, continue with
// the new attempt; older-gen chunks are stale and dropped.
func (f *followSet) runFollower(ctx context.Context, node string) {
	view, next, done, err := f.open.openLogs(ctx, node, true)
	if err != nil {
		if ctx.Err() == nil {
			f.sink.Log(node, fmt.Sprintf("(logs unavailable: %v)", err))
		}
		return
	}
	defer done()
	f.markStreamed(node)
	p := &linePrinter{sink: f.sink, node: node}
	defer p.flush()
	p.printView(view)
	gen := view.Gen
	for {
		chunk, err := next()
		if err != nil {
			return
		}
		if chunk.Gen < gen {
			continue // stale attempt
		}
		if chunk.Gen > gen {
			if gen > 0 {
				p.flush()
				f.sink.Log(node, fmt.Sprintf("(retried — attempt gen %d)", chunk.Gen))
			}
			gen = chunk.Gen
		}
		p.write(chunk.Data)
	}
}

// close stops every follower (each after the drain grace) and waits for
// them to finish. Idempotent.
func (f *followSet) close() {
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		for node, cancel := range f.active {
			delete(f.active, node)
			time.AfterFunc(followStopGrace, cancel)
		}
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// streamedNodes reports every node whose output was followed live, so the
// failure recap can point at the scroll instead of re-printing logs.
func (f *followSet) streamedNodes() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.followed)
}
