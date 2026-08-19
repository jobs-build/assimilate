package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"

	"github.com/jobs-build/assimilate/internal/spec"
)

// testSink records State, Snapshot and Log calls; safe for concurrent
// followers. Logs are recorded in the classic prefixed form ("kind:key8 │
// line" for node-tagged lines) so assertions read like terminal output.
type testSink struct {
	mu     sync.Mutex
	states []string
	logs   []string
	snaps  []api.Snapshot
}

func (s *testSink) State(phase, counts string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, phase+"|"+counts)
}

func (s *testSink) Snapshot(snap api.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps = append(s.snaps, snap)
}

func (s *testSink) Log(node, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if node != "" {
		line = shortNode(node) + " │ " + line
	}
	s.logs = append(s.logs, line)
}

func (s *testSink) snapCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.snaps)
}

func (s *testSink) logText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.logs, "\n")
}

func (s *testSink) stateList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.states...)
}

// waitFor polls cond until it holds or the budget runs out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// shortGrace shrinks the drain grace for the test's lifetime so stop paths
// run in milliseconds.
func shortGrace(t *testing.T) {
	t.Helper()
	old := followStopGrace
	followStopGrace = 5 * time.Millisecond
	t.Cleanup(func() { followStopGrace = old })
}

// nodeName mints a parseable node name (clientcli's test helper): the byte
// pattern keeps the key canonical for wire.ParseNodeName.
func nodeName(kind string, i int) string {
	b := byte(i/8<<4 | i%8)
	return kind + "_" + strings.Repeat(fmt.Sprintf("%02x", b), 32)
}

// hex8 is the shortNode key prefix for nodeName(_, i).
func hex8(i int) string {
	return strings.Repeat(fmt.Sprintf("%02x", byte(i/8<<4|i%8)), 4)
}

// fakeOpener backs the follower set in tests: one buffered chunk feed per
// opened node; next() blocks on the feed until the follower ctx dies. Nodes
// registered via failOpen error out of openLogs instead of opening.
type fakeOpener struct {
	mu       sync.Mutex
	views    map[string]api.LogView
	feeds    map[string]chan wire.LogChunk
	ctxs     map[string]context.Context
	openErrs map[string]error
	tries    map[string]bool
	opens    int
}

func newFakeOpener() *fakeOpener {
	return &fakeOpener{
		views:    map[string]api.LogView{},
		feeds:    map[string]chan wire.LogChunk{},
		ctxs:     map[string]context.Context{},
		openErrs: map[string]error{},
		tries:    map[string]bool{},
	}
}

func (f *fakeOpener) openLogs(ctx context.Context, node string, follow bool) (api.LogView, func() (wire.LogChunk, error), func(), error) {
	if !follow {
		return api.LogView{}, nil, nil, errors.New("follower must follow")
	}
	f.mu.Lock()
	f.tries[node] = true
	if err := f.openErrs[node]; err != nil {
		f.mu.Unlock()
		return api.LogView{}, nil, nil, err
	}
	ch := make(chan wire.LogChunk, 64)
	f.feeds[node] = ch
	f.ctxs[node] = ctx
	f.opens++
	view := f.views[node]
	f.mu.Unlock()
	next := func() (wire.LogChunk, error) {
		select {
		case c := <-ch:
			return c, nil
		case <-ctx.Done():
			return wire.LogChunk{}, ctx.Err()
		}
	}
	return view, next, func() {}, nil
}

func (f *fakeOpener) feed(t *testing.T, node string, chunk wire.LogChunk) {
	t.Helper()
	f.mu.Lock()
	ch := f.feeds[node]
	f.mu.Unlock()
	if ch == nil {
		t.Fatalf("no follower opened for %s", node)
	}
	ch <- chunk
}

func (f *fakeOpener) opened(node string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.feeds[node] != nil
}

// failOpen makes every openLogs for node fail with err.
func (f *fakeOpener) failOpen(node string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openErrs[node] = err
}

// tried reports an openLogs attempt for node, successful or not.
func (f *fakeOpener) tried(node string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tries[node]
}

func (f *fakeOpener) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

func (f *fakeOpener) followerDead(node string) bool {
	f.mu.Lock()
	ctx := f.ctxs[node]
	f.mu.Unlock()
	return ctx != nil && ctx.Err() != nil
}

// fakeFetcher backs the failure-recap path.
type fakeFetcher struct {
	mu      sync.Mutex
	views   map[string]api.LogView
	errs    map[string]error
	fetched []string
}

func (f *fakeFetcher) fetchLogs(_ context.Context, node string) (api.LogView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, node)
	if err := f.errs[node]; err != nil {
		return api.LogView{}, err
	}
	return f.views[node], nil
}

func (f *fakeFetcher) fetchedNodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fetched...)
}

func runningSnap(nodes ...string) api.Snapshot {
	var snap api.Snapshot
	for _, n := range nodes {
		snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: n, Phase: wire.PhaseRunning})
	}
	return snap
}

// TestLinePrinterAssembly: chunk-boundary line splits, CRLF trim, gap marker,
// and the trailing-partial flush.
func TestLinePrinterAssembly(t *testing.T) {
	sink := &testSink{}
	p := &linePrinter{sink: sink, node: "n"}

	p.printView(api.LogView{Head: []byte("head partial"), GapSize: 42, Tail: []byte("tail\r\n")})
	p.write([]byte("hel"))
	p.write([]byte("lo\nwor"))
	p.write([]byte("ld\ntrailing"))
	p.flush()

	want := "n │ head partial\n" + // flushed ahead of the gap marker
		"n │ ... [42 bytes omitted] ...\n" +
		"n │ tail\n" + // \r trimmed
		"n │ hello\n" +
		"n │ world\n" +
		"n │ trailing"
	if got := sink.logText(); got != want {
		t.Fatalf("printer output:\n%q\nwant:\n%q", got, want)
	}
}

// TestFollowSetFollowAndStop: a running node gets a follower whose lines
// arrive prefixed; leaving the active set grace-stops the follower and close
// flushes its trailing partial line; retries get a marker; stale-gen chunks
// are dropped.
func TestFollowSetFollowAndStop(t *testing.T) {
	shortGrace(t)
	nodeA := nodeName("buildrun", 1)
	prefix := "buildrun:" + hex8(1) + " │ "
	sink := &testSink{}
	fake := newFakeOpener()
	fs := newFollowSet(context.Background(), fake, nil, sink)
	defer fs.close()

	fs.sync(runningSnap(nodeA))
	waitFor(t, "follower open", func() bool { return fake.opened(nodeA) })

	fake.feed(t, nodeA, wire.LogChunk{Gen: 1, Stream: "stdout", Seq: 1, Data: []byte("hello\nwor")})
	fake.feed(t, nodeA, wire.LogChunk{Gen: 1, Stream: "stdout", Seq: 2, Data: []byte("ld\n")})
	waitFor(t, "lines delivered", func() bool {
		return strings.Contains(sink.logText(), prefix+"hello") &&
			strings.Contains(sink.logText(), prefix+"world")
	})

	// A stale chunk (older gen) is dropped; a newer gen marks the retry.
	fake.feed(t, nodeA, wire.LogChunk{Gen: 0, Stream: "stdout", Seq: 9, Data: []byte("stale\n")})
	fake.feed(t, nodeA, wire.LogChunk{Gen: 2, Stream: "stdout", Seq: 1, Data: []byte("again\ntrailing")})
	waitFor(t, "retry lines", func() bool {
		return strings.Contains(sink.logText(), prefix+"(retried — attempt gen 2)") &&
			strings.Contains(sink.logText(), prefix+"again")
	})
	if strings.Contains(sink.logText(), "stale") {
		t.Fatalf("stale-gen chunk must be dropped:\n%s", sink.logText())
	}

	// Node leaves the active set → follower dies after the grace; close
	// flushes the unterminated tail.
	fs.sync(api.Snapshot{Nodes: []api.NodeSnap{{Node: nodeA, Phase: wire.PhaseDone}}})
	waitFor(t, "follower stop", func() bool { return fake.followerDead(nodeA) })
	fs.close()
	if !strings.Contains(sink.logText(), prefix+"trailing") {
		t.Fatalf("trailing partial line not flushed:\n%s", sink.logText())
	}
	if !fs.streamedNodes()[nodeA] {
		t.Fatal("streamedNodes must remember the followed node")
	}
}

// TestFollowSetPerBuildCap: a fan-out beyond maxFollowNodes follows exactly
// the cap, notes the overflow once, and a freed slot admits a waiting node.
func TestFollowSetPerBuildCap(t *testing.T) {
	shortGrace(t)
	var nodes []string
	for i := 0; i < maxFollowNodes+3; i++ {
		nodes = append(nodes, nodeName("import", i))
	}
	sink := &testSink{}
	fake := newFakeOpener()
	fs := newFollowSet(context.Background(), fake, nil, sink)
	defer fs.close()

	fs.sync(runningSnap(nodes...))
	waitFor(t, "cap followers", func() bool { return fake.openCount() == maxFollowNodes })
	fs.sync(runningSnap(nodes...)) // unchanged snapshot must not re-open
	if n := fake.openCount(); n != maxFollowNodes {
		t.Fatalf("open count = %d, want %d", n, maxFollowNodes)
	}
	if got := strings.Count(sink.logText(), "follow cap reached"); got != 1 {
		t.Fatalf("cap note delivered %d times:\n%s", got, sink.logText())
	}

	// First node finishes → its slot admits one waiting node.
	snap := runningSnap(nodes[1:]...)
	snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: nodes[0], Phase: wire.PhaseDone})
	fs.sync(snap)
	waitFor(t, "freed slot refilled", func() bool { return fake.openCount() == maxFollowNodes+1 })
	waitFor(t, "finished follower stopped", func() bool { return fake.followerDead(nodes[0]) })
}

// TestFollowSetRunningPriority: queued nodes are followed (a fast job may
// never be seen running — snapshots are coalesced), but when slots are
// scarce the running nodes claim them first regardless of snapshot order.
func TestFollowSetRunningPriority(t *testing.T) {
	shortGrace(t)
	fake := newFakeOpener()
	fs := newFollowSet(context.Background(), fake, nil, &testSink{})
	defer fs.close()

	queued := nodeName("import", 1)
	fs.sync(api.Snapshot{Nodes: []api.NodeSnap{{Node: queued, Phase: wire.PhaseQueued}}})
	waitFor(t, "queued node followed", func() bool { return fake.opened(queued) })

	// Fill the remaining slots: running nodes listed AFTER queued ones still
	// win the free slots.
	var snap api.Snapshot
	snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: queued, Phase: wire.PhaseQueued})
	for i := 0; i < maxFollowNodes; i++ {
		snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: nodeName("pin", 10+i), Phase: wire.PhaseQueued})
	}
	var running []string
	for i := 0; i < maxFollowNodes; i++ {
		running = append(running, nodeName("buildrun", 20+i))
		snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: running[i], Phase: wire.PhaseRunning})
	}
	fs.sync(snap)
	waitFor(t, "cap reached", func() bool { return fake.openCount() == maxFollowNodes })
	// The kept queued follower plus running nodes hold every slot; the
	// running nodes that fit are all attached, the later queued ones are not.
	for _, n := range running[:maxFollowNodes-1] {
		if !fake.opened(n) {
			t.Fatalf("running node %s must out-rank queued nodes for a slot", n)
		}
	}
	if fake.opened(nodeName("pin", 10)) {
		t.Fatal("queued node must not claim a slot ahead of running ones")
	}
}

// TestFollowSetGlobalBudget: the client-wide semaphore bounds live follow
// streams ACROSS follower sets; a slot freed by one build's finished
// follower becomes available to another build.
func TestFollowSetGlobalBudget(t *testing.T) {
	shortGrace(t)
	if maxFollowNodes != 4 || maxClientFollows != 24 {
		t.Fatalf("caps = %d per build / %d client-wide, want 4 / 24", maxFollowNodes, maxClientFollows)
	}
	sem := make(chan struct{}, 2) // small budget to exercise the mechanism
	fakeA, fakeB := newFakeOpener(), newFakeOpener()
	sinkB := &testSink{}
	fsA := newFollowSet(context.Background(), fakeA, sem, &testSink{})
	fsB := newFollowSet(context.Background(), fakeB, sem, sinkB)
	defer fsA.close()
	defer fsB.close()

	a1, a2 := nodeName("buildrun", 1), nodeName("buildrun", 2)
	fsA.sync(runningSnap(a1, a2))
	waitFor(t, "first build holds the budget", func() bool { return fakeA.openCount() == 2 })

	// The second build finds no budget: no follower, one cap note.
	b1 := nodeName("buildrun", 3)
	fsB.sync(runningSnap(b1))
	if fakeB.openCount() != 0 {
		t.Fatalf("second build opened %d followers with an exhausted budget", fakeB.openCount())
	}
	if !strings.Contains(sinkB.logText(), "follow cap reached") {
		t.Fatalf("missing cap note:\n%s", sinkB.logText())
	}

	// One of the first build's nodes finishes → its stream's slot frees
	// (post-grace) and the second build's next sync claims it.
	snap := runningSnap(a2)
	snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: a1, Phase: wire.PhaseDone})
	fsA.sync(snap)
	waitFor(t, "freed budget claimed across builds", func() bool {
		fsB.sync(runningSnap(b1))
		return fakeB.opened(b1)
	})
}

// snapSeq feeds canned steps to followLoop: each step may first wait for a
// condition (follower attach, chunk delivery), then returns its snapshot.
type snapStep struct {
	wait func() bool
	snap api.Snapshot
	err  error
}

func snapSeq(t *testing.T, steps []snapStep) func() (api.Snapshot, error) {
	i := 0
	return func() (api.Snapshot, error) {
		if i >= len(steps) {
			t.Error("watch read past the terminal snapshot")
			return api.Snapshot{}, errors.New("exhausted")
		}
		s := steps[i]
		i++
		if s.wait != nil {
			waitFor(t, "snapshot precondition", s.wait)
		}
		return s.snap, s.err
	}
}

// TestFollowLoopDone: raw phases and counts summaries flow into the sink per
// snapshot, followed output arrives, and the done terminal maps to StateDone
// with a nil error.
func TestFollowLoopDone(t *testing.T) {
	shortGrace(t)
	nodeA := nodeName("buildrun", 1)
	prefix := "buildrun:" + hex8(1) + " │ "
	sink := &testSink{}
	fake := newFakeOpener()

	running := runningSnap(nodeA)
	running.Phase = "running"
	running.Counts = wire.Counts{Total: 3, Done: 1, Running: 1}
	terminal := api.Snapshot{
		Phase:    "done",
		Counts:   wire.Counts{Total: 3, Done: 3},
		Nodes:    []api.NodeSnap{{Node: nodeA, Phase: wire.PhaseDone}},
		Terminal: true,
	}
	next := snapSeq(t, []snapStep{
		{snap: running},
		{wait: func() bool {
			if !fake.opened(nodeA) {
				return false
			}
			fake.feed(t, nodeA, wire.LogChunk{Gen: 1, Data: []byte("hello\n")})
			return strings.Contains(sink.logText(), prefix+"hello")
		}, snap: terminal},
	})

	state, err := followLoop(context.Background(), next, fake, &fakeFetcher{}, nil, sink)
	if err != nil || state != spec.StateDone {
		t.Fatalf("followLoop = %q, %v; want %q, nil", state, err, spec.StateDone)
	}
	wantStates := []string{"running|1/3 built · 1 running", "done|3/3 built"}
	if got := sink.stateList(); len(got) != 2 || got[0] != wantStates[0] || got[1] != wantStates[1] {
		t.Fatalf("states = %q, want %q", got, wantStates)
	}
	// Every snapshot also arrives raw (the TUI folds it into the graph
	// subtree), alongside — not instead of — the counts summary.
	if sink.snapCount() != 2 {
		t.Fatalf("snapshots delivered = %d, want 2", sink.snapCount())
	}
}

// TestFollowLoopFailedRecap: the failed terminal returns StateFailed with a
// NIL error; hard-failed nodes get an ErrSummary header, streamed nodes a
// pointer at the scroll, un-streamed ones their stored view; derived
// failed-upstream nodes stay silent.
func TestFollowLoopFailedRecap(t *testing.T) {
	shortGrace(t)
	streamed := nodeName("buildrun", 1)
	stored := nodeName("buildfrom", 2)
	upstream := nodeName("buildvalue", 3)
	sink := &testSink{}
	fake := newFakeOpener()
	fetch := &fakeFetcher{views: map[string]api.LogView{
		stored: {Node: stored, Gen: 2, Head: []byte("compile error: boom\n")},
	}}

	running := runningSnap(streamed)
	running.Phase = "running"
	running.Counts = wire.Counts{Total: 3, Running: 1}
	terminal := api.Snapshot{
		Phase:  "failed",
		Counts: wire.Counts{Total: 3, Done: 1, Failed: 2},
		Nodes: []api.NodeSnap{
			{Node: streamed, Phase: wire.PhaseFailed, Gen: 1, ErrSummary: "exit 1: boom"},
			{Node: stored, Phase: wire.PhaseFailed, Gen: 2, ErrSummary: "exit 2: compile error"},
			{Node: upstream, Phase: wire.PhaseUpstream, ErrSummary: "upstream failed"},
		},
		Terminal: true,
	}
	next := snapSeq(t, []snapStep{
		{snap: running},
		{wait: func() bool { return fake.opened(streamed) }, snap: terminal},
	})

	state, err := followLoop(context.Background(), next, fake, fetch, nil, sink)
	if err != nil {
		t.Fatalf("failed terminal must not be an error, got %v", err)
	}
	if state != spec.StateFailed {
		t.Fatalf("state = %q, want %q", state, spec.StateFailed)
	}
	text := sink.logText()
	for _, want := range []string{
		"--- buildrun:" + hex8(1) + " failed (gen 1): exit 1: boom",
		"(output streamed above)",
		"--- buildfrom:" + hex8(2) + " failed (gen 2): exit 2: compile error",
		"buildfrom:" + hex8(2) + " │ compile error: boom",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("failure recap missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "buildvalue:") {
		t.Fatalf("failed-upstream node must not be recapped:\n%s", text)
	}
	if got := fetch.fetchedNodes(); len(got) != 1 || got[0] != stored {
		t.Fatalf("fetched %q, want only %q", got, stored)
	}
}

// TestFollowLoopRecapAfterOpenFailure: a node counts as streamed only once
// its follow stream actually opened — when openLogs fails, nothing was
// streamed, so the failure recap must fetch and print that node's stored
// view instead of pointing at the scroll.
func TestFollowLoopRecapAfterOpenFailure(t *testing.T) {
	shortGrace(t)
	broken := nodeName("buildrun", 1) // follow attempted, openLogs fails
	streamed := nodeName("buildfrom", 2)
	sink := &testSink{}
	fake := newFakeOpener()
	fake.failOpen(broken, errors.New("stream refused"))
	fetch := &fakeFetcher{views: map[string]api.LogView{
		broken: {Node: broken, Gen: 1, Head: []byte("panic: kaboom\n")},
	}}

	running := runningSnap(broken, streamed)
	running.Phase = "running"
	running.Counts = wire.Counts{Total: 2, Running: 2}
	terminal := api.Snapshot{
		Phase:  "failed",
		Counts: wire.Counts{Total: 2, Failed: 2},
		Nodes: []api.NodeSnap{
			{Node: broken, Phase: wire.PhaseFailed, Gen: 1, ErrSummary: "exit 1: kaboom"},
			{Node: streamed, Phase: wire.PhaseFailed, Gen: 1, ErrSummary: "exit 2: boom"},
		},
		Terminal: true,
	}
	next := snapSeq(t, []snapStep{
		{snap: running},
		{wait: func() bool { return fake.opened(streamed) && fake.tried(broken) }, snap: terminal},
	})

	state, err := followLoop(context.Background(), next, fake, fetch, nil, sink)
	if err != nil || state != spec.StateFailed {
		t.Fatalf("followLoop = %q, %v; want %q, nil", state, err, spec.StateFailed)
	}
	text := sink.logText()
	for _, want := range []string{
		"--- buildrun:" + hex8(1) + " failed (gen 1): exit 1: kaboom",
		"buildrun:" + hex8(1) + " │ panic: kaboom", // stored view, fetched
		"--- buildfrom:" + hex8(2) + " failed (gen 1): exit 2: boom",
		"(output streamed above)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("failure recap missing %q:\n%s", want, text)
		}
	}
	// Exactly one streamed-above pointer: the never-opened node must not
	// claim one, and it alone gets the stored-log fetch.
	if got := strings.Count(text, "(output streamed above)"); got != 1 {
		t.Fatalf("streamed-above printed %d times, want 1:\n%s", got, text)
	}
	if got := fetch.fetchedNodes(); len(got) != 1 || got[0] != broken {
		t.Fatalf("fetched %q, want only %q", got, broken)
	}
}

// TestFollowLoopTransportError: a broken watch stream is an unknown outcome —
// non-nil error, no state.
func TestFollowLoopTransportError(t *testing.T) {
	shortGrace(t)
	next := snapSeq(t, []snapStep{{err: errors.New("stream reset")}})
	state, err := followLoop(context.Background(), next, newFakeOpener(), &fakeFetcher{}, nil, &testSink{})
	if err == nil || state != "" {
		t.Fatalf("followLoop = %q, %v; want \"\", transport error", state, err)
	}
	if !strings.Contains(err.Error(), "stream reset") {
		t.Fatalf("error must carry the cause, got %v", err)
	}
}

// TestFollowLoopContextCancelled: cancellation surfaces as ctx.Err(), not as
// a wrapped transport error.
func TestFollowLoopContextCancelled(t *testing.T) {
	shortGrace(t)
	ctx, cancel := context.WithCancel(context.Background())
	next := func() (api.Snapshot, error) {
		cancel()
		return api.Snapshot{}, errors.New("read deadline")
	}
	state, err := followLoop(ctx, next, newFakeOpener(), &fakeFetcher{}, nil, &testSink{})
	if !errors.Is(err, context.Canceled) || state != "" {
		t.Fatalf("followLoop = %q, %v; want \"\", context.Canceled", state, err)
	}
}
