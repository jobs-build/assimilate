// Package builds orchestrates all of an environment's builds: source dirs
// are ingested and pushed once each (in appearance order), every build of a
// pushed source starts immediately and runs concurrently, and everything is
// reported as spec.Events for the UI.
package builds

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jobs-build/jobs-iroh/api"

	"github.com/jobs-build/assimilate/internal/jobs"
	"github.com/jobs-build/assimilate/internal/spec"
)

// Backend abstracts the jobs client; *jobs.Client implements it, tests fake
// it (jobs.Source zero values are fine for fakes — the orchestrator treats
// sources as opaque).
type Backend interface {
	Ingest(ctx context.Context, dir string) (jobs.Source, error)
	PushSource(ctx context.Context, src *jobs.Source, prog jobs.ProgressFunc) error
	Submit(ctx context.Context, src jobs.Source, s spec.BuildSpec) (jobs.Handle, error)
	Follow(ctx context.Context, h jobs.Handle, sink jobs.Sink) (spec.BuildState, error)
	Cancel(ctx context.Context, h jobs.Handle) error
}

// Result is one build's outcome.
type Result struct {
	Spec     spec.BuildSpec
	K        string // 64-hex build key; set once submitted
	ImageRef string // registry reference; set once submitted
	State    spec.BuildState
	Err      string // short failure/cancellation detail
}

// cancelGrace bounds each best-effort Cancel sent after ctx is cancelled.
const cancelGrace = 5 * time.Second

// drainGrace bounds how long Follows may drain after ctx is cancelled:
// cancelGrace for the best-effort Cancels plus headroom for the terminal
// snapshots to arrive. Var so tests can shrink it (like jobs'
// followStopGrace).
var drainGrace = cancelGrace + 5*time.Second

// maxInFlight bounds concurrent submit+Follow pairs: 60 watch streams + the
// 24 log follows budgeted in internal/jobs (maxClientFollows) + transient
// one-shot calls (submit, cancel, log fetch) stays under the ~100 live
// streams one QUIC connection supports.
const maxInFlight = 60

// inFlightSlots is maxInFlight behind a test seam.
var inFlightSlots = maxInFlight

// Run executes every spec (already unique, in appearance order), emitting
// events along the way. root is the project root that spec paths are
// relative to; registry prefixes image refs.
//
// Behavior: specs are grouped by source path preserving appearance order;
// each group's source is ingested and pushed (its builds show StatePushing),
// then each build submits and follows concurrently while later groups still
// push. A failing build does not stop the others — the user sees every
// failure. Context cancellation sends best-effort Cancels for in-flight
// builds. Run returns one Result per spec (same order) and a non-nil error
// when any build did not end StateDone.
func Run(ctx context.Context, root, registry string, specs []spec.BuildSpec, b Backend, events chan<- spec.Event) ([]Result, error) {
	// Follows outlive ctx: after cancellation the server-side Cancel drives
	// them to a terminal snapshot, so they run detached — but time-bounded:
	// if a Cancel frame is lost or the server never terminates the request,
	// stopFollows trips every armed stream deadline (internal/jobs/conn.go)
	// so no Follow can block Run forever.
	detached := context.WithoutCancel(ctx)
	followCtx, stopFollows := context.WithCancel(detached)
	defer stopFollows()
	r := &runner{
		ctx:       ctx,
		followCtx: followCtx,
		registry:  registry,
		specs:     specs,
		b:         b,
		events:    events,
		results:   make([]Result, len(specs)),
		slots:     make(chan struct{}, inFlightSlots),
		flight:    &flight{b: b, base: detached, handles: map[int]jobs.Handle{}},
	}
	for i, s := range specs {
		r.results[i] = Result{Spec: s, State: spec.StatePending}
	}

	// One group per unique source path, first-appearance order; a group's
	// builds all submit as soon as its push lands.
	type group struct {
		path string
		idxs []int
	}
	var groups []*group
	byPath := map[string]*group{}
	for i, s := range specs {
		g := byPath[s.Path]
		if g == nil {
			g = &group{path: s.Path}
			byPath[s.Path] = g
			groups = append(groups, g)
		}
		g.idxs = append(g.idxs, i)
	}

	// On ctx death: best-effort Cancels first, then the drain bound. dg is
	// read here, not in the callback — the callback goroutine can outlive
	// Run, and a later test's seam write must not race the read. Letting
	// the timer fire after a normal return is harmless: stopFollows is
	// idempotent and deferred anyway.
	dg := drainGrace
	stop := context.AfterFunc(ctx, func() {
		r.flight.cancelAll()
		time.AfterFunc(dg, stopFollows)
	})
	defer stop()

	// Ingest+push one group at a time while earlier groups' builds run.
	for _, g := range groups {
		if ctx.Err() != nil {
			r.cancelGroup(g.idxs)
			continue
		}
		for _, i := range g.idxs {
			r.send(spec.Event{Build: i, Kind: spec.KindState, State: spec.StatePushing})
			r.send(spec.Event{Build: i, Kind: spec.KindInfo, Info: "ingesting"})
		}
		src, err := b.Ingest(ctx, spec.SourceDir(root, g.path))
		if err != nil {
			r.groupError(g.idxs, fmt.Errorf("ingest %s: %w", g.path, err))
			continue
		}
		prog := func(done, total int) {
			for _, i := range g.idxs {
				r.send(spec.Event{Build: i, Kind: spec.KindInfo, Info: fmt.Sprintf("push %d/%d objects", done, total)})
			}
		}
		if err := b.PushSource(ctx, &src, prog); err != nil {
			r.groupError(g.idxs, fmt.Errorf("push %s: %w", g.path, err))
			continue
		}
		for _, i := range g.idxs {
			r.wg.Add(1)
			go r.build(i, src)
		}
	}
	r.wg.Wait()

	bad, cancelled := 0, 0
	for i := range r.results {
		if r.results[i].State == spec.StateDone {
			continue
		}
		bad++
		if r.results[i].State == spec.StateCancelled {
			cancelled++
		}
	}
	if bad == 0 {
		return r.results, nil
	}
	word := "failed"
	if cancelled == bad {
		word = "cancelled"
	}
	return r.results, fmt.Errorf("%d of %d builds %s", bad, len(specs), word)
}

// runner is one Run invocation. results is index-owned: every element is
// written by exactly one goroutine (the main loop for groups that never
// spawn, that build's goroutine otherwise), so no lock is needed — wg.Wait
// orders all writes before the return.
type runner struct {
	ctx       context.Context
	followCtx context.Context // survives ctx cancellation, drainGrace-bounded
	registry  string
	specs     []spec.BuildSpec
	b         Backend
	events    chan<- spec.Event
	results   []Result
	slots     chan struct{} // in-flight submit+Follow semaphore
	flight    *flight
	wg        sync.WaitGroup
}

// send delivers one event; the caller owns the channel and may apply
// backpressure — blocking here is fine.
func (r *runner) send(e spec.Event) { r.events <- e }

// finish records i's terminal state and emits the terminal KindState event;
// Info carries the failure summary only for StateFailed.
func (r *runner) finish(i int, st spec.BuildState, detail string) {
	r.results[i].State = st
	if st != spec.StateDone {
		r.results[i].Err = detail
	}
	info := ""
	if st == spec.StateFailed {
		info = detail
	}
	r.send(spec.Event{Build: i, Kind: spec.KindState, State: st, Info: info})
}

// groupError settles a whole group whose ingest or push failed: cancelled if
// ctx died, failed otherwise. Other groups continue.
func (r *runner) groupError(idxs []int, err error) {
	if r.ctx.Err() != nil {
		r.cancelGroup(idxs)
		return
	}
	msg := err.Error()
	for _, i := range idxs {
		r.finish(i, spec.StateFailed, msg)
	}
}

func (r *runner) cancelGroup(idxs []int) {
	for _, i := range idxs {
		r.finish(i, spec.StateCancelled, "cancelled")
	}
}

// build submits and follows one build of an already-pushed source, holding
// one in-flight slot throughout so live watch streams stay under the QUIC
// connection's stream budget.
func (r *runner) build(i int, src jobs.Source) {
	defer r.wg.Done()
	select {
	case r.slots <- struct{}{}:
	case <-r.ctx.Done():
		r.finish(i, spec.StateCancelled, "cancelled")
		return
	}
	defer func() { <-r.slots }()
	if r.ctx.Err() != nil {
		r.finish(i, spec.StateCancelled, "cancelled")
		return
	}
	h, err := r.b.Submit(r.ctx, src, r.specs[i])
	if err != nil {
		if r.ctx.Err() != nil {
			r.finish(i, spec.StateCancelled, "cancelled")
		} else {
			r.finish(i, spec.StateFailed, "submit: "+err.Error())
		}
		return
	}
	r.results[i].K = h.K
	r.results[i].ImageRef = spec.ImageRef(r.registry, h.K)
	r.flight.add(i, h)
	r.send(spec.Event{Build: i, Kind: spec.KindState, State: spec.StateBuilding})
	st, err := r.b.Follow(r.followCtx, h, sink{build: i, events: r.events})
	r.flight.remove(i)
	switch {
	case err != nil && r.ctx.Err() != nil:
		// ctx died: whether the stream tore down or the drain bound
		// tripped, the outcome is the user's cancellation.
		r.finish(i, spec.StateCancelled, "cancelled")
	case err != nil:
		// Outcome unknown (transport trouble) — surfaced as a failure.
		r.finish(i, spec.StateFailed, err.Error())
	case st == spec.StateDone:
		r.finish(i, spec.StateDone, "")
	case st == spec.StateCancelled:
		r.finish(i, spec.StateCancelled, "cancelled")
	case st == spec.StateFailed:
		r.finish(i, spec.StateFailed, "build failed")
	default:
		r.finish(i, spec.StateFailed, fmt.Sprintf("follow ended in non-terminal state %q", st))
	}
}

// sink adapts one build's follow stream to spec.Events. Terminal phases are
// dropped — the terminal KindState comes from Follow's return value, and
// StateBuilding is emitted once at submit, not per snapshot.
type sink struct {
	build  int
	events chan<- spec.Event
}

func (s sink) State(phase, counts string) {
	if spec.BuildState(phase).Terminal() {
		return
	}
	s.events <- spec.Event{Build: s.build, Kind: spec.KindInfo, Info: counts}
}

func (s sink) Snapshot(snap api.Snapshot) {
	s.events <- spec.Event{Build: s.build, Kind: spec.KindSnapshot, Snap: &snap}
}

func (s sink) Log(node, line string) {
	s.events <- spec.Event{Build: s.build, Kind: spec.KindLog, Node: node, Line: line}
}

// flight tracks submitted builds whose Follow has not returned. cancelAll
// runs (once, from context.AfterFunc) when ctx dies; a handle registered
// after that races the sweep, so add cancels it itself — no build slips
// through submitted-but-uncancelled.
type flight struct {
	b    Backend
	base context.Context // detached from ctx: Cancels run after ctx is dead

	mu        sync.Mutex
	handles   map[int]jobs.Handle
	cancelled bool
}

func (f *flight) add(i int, h jobs.Handle) {
	f.mu.Lock()
	f.handles[i] = h
	cancelled := f.cancelled
	f.mu.Unlock()
	if cancelled {
		f.cancelOne(h)
	}
}

func (f *flight) remove(i int) {
	f.mu.Lock()
	delete(f.handles, i)
	f.mu.Unlock()
}

func (f *flight) cancelAll() {
	f.mu.Lock()
	f.cancelled = true
	hs := make([]jobs.Handle, 0, len(f.handles))
	for _, h := range f.handles {
		hs = append(hs, h)
	}
	f.mu.Unlock()
	for _, h := range hs {
		f.cancelOne(h)
	}
}

// cancelOne sends one best-effort Cancel on a fresh grace-bounded context.
func (f *flight) cancelOne(h jobs.Handle) {
	cctx, cancel := context.WithTimeout(f.base, cancelGrace)
	defer cancel()
	_ = f.b.Cancel(cctx, h)
}
