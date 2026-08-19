package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jobs-build/assimilate/internal/jobs"
	"github.com/jobs-build/assimilate/internal/spec"
)

// guard bounds waits that only a broken orchestrator would hit; tests never
// sleep on the happy path.
const guard = 5 * time.Second

type followFn func(ctx context.Context, sink jobs.Sink) (spec.BuildState, error)

// fake is a scripted Backend. jobs.Source is opaque (zero values carry no
// identity), so sources are tracked by the dir argument: PushSource pairs
// with the most recent Ingest — valid because the orchestrator ingests and
// pushes strictly one group at a time.
type fake struct {
	mu         sync.Mutex
	ingests    []string // dirs, call order
	pushes     []string // dirs (paired to last ingest), call order
	submits    []string // spec display names, call order
	cancels    []jobs.Handle
	cancelErrs []error // ctx.Err() as observed by Cancel
	cancelDls  []bool  // ctx had a deadline, as observed by Cancel
	lastIngest string

	ingestErr map[string]error                           // by dir
	pushErr   map[string]error                           // by dir
	pushHook  map[string]func(ctx context.Context) error // by dir; replaces default progress
	submitErr map[string]error                           // by display name
	follow    map[string]followFn                        // by display name; nil = default done
	cancelErr error                                      // returned by every Cancel
	onCancel  func(h jobs.Handle)
}

func kFor(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

func (f *fake) Ingest(ctx context.Context, dir string) (jobs.Source, error) {
	f.mu.Lock()
	f.ingests = append(f.ingests, dir)
	f.lastIngest = dir
	f.mu.Unlock()
	if err := f.ingestErr[dir]; err != nil {
		return jobs.Source{}, err
	}
	return jobs.Source{}, ctx.Err()
}

func (f *fake) PushSource(ctx context.Context, src *jobs.Source, prog jobs.ProgressFunc) error {
	f.mu.Lock()
	dir := f.lastIngest
	f.pushes = append(f.pushes, dir)
	f.mu.Unlock()
	if hook := f.pushHook[dir]; hook != nil {
		return hook(ctx)
	}
	if err := f.pushErr[dir]; err != nil {
		return err
	}
	prog(1, 2)
	prog(2, 2)
	return nil
}

func (f *fake) Submit(ctx context.Context, src jobs.Source, s spec.BuildSpec) (jobs.Handle, error) {
	name := s.DisplayName()
	f.mu.Lock()
	f.submits = append(f.submits, name)
	f.mu.Unlock()
	if err := f.submitErr[name]; err != nil {
		return jobs.Handle{}, err
	}
	return jobs.Handle{RequestID: "req-" + name, K: kFor(name)}, nil
}

func (f *fake) Follow(ctx context.Context, h jobs.Handle, sink jobs.Sink) (spec.BuildState, error) {
	name := strings.TrimPrefix(h.RequestID, "req-")
	if fn := f.follow[name]; fn != nil {
		return fn(ctx, sink)
	}
	sink.State("building", "1/1 running")
	sink.Log("", "hello")
	sink.State("done", "1/1 built") // terminal phase — must NOT surface as KindInfo
	return spec.StateDone, nil
}

func (f *fake) Cancel(ctx context.Context, h jobs.Handle) error {
	_, hasDeadline := ctx.Deadline()
	f.mu.Lock()
	f.cancels = append(f.cancels, h)
	f.cancelErrs = append(f.cancelErrs, ctx.Err())
	f.cancelDls = append(f.cancelDls, hasDeadline)
	f.mu.Unlock()
	if f.onCancel != nil {
		f.onCancel(h)
	}
	return f.cancelErr
}

// runCollect runs Run with a concurrently drained unbuffered channel (sends
// must never require caller-side buffering) and returns everything sent.
func runCollect(t *testing.T, ctx context.Context, root, registry string, specs []spec.BuildSpec, b Backend) ([]Result, []spec.Event, error) {
	t.Helper()
	events := make(chan spec.Event)
	var evs []spec.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range events {
			evs = append(evs, e)
		}
	}()
	results, err := Run(ctx, root, registry, specs, b, events)
	close(events) // the test owns the channel; Run must never close it
	<-done
	return results, evs, err
}

func perBuild(evs []spec.Event, build int) []spec.Event {
	var out []spec.Event
	for _, e := range evs {
		if e.Build == build {
			out = append(out, e)
		}
	}
	return out
}

func staticFollow(st spec.BuildState, err error) followFn {
	return func(context.Context, jobs.Sink) (spec.BuildState, error) { return st, err }
}

func TestRunHappyPath(t *testing.T) {
	specs := []spec.BuildSpec{
		{Name: "a", Path: "svc/a", Platform: "linux/amd64"},
		{Name: "a-arm", Path: "svc/a", Platform: "linux/arm64"},
		{Name: "b", Path: "/", Platform: "linux/amd64"},
	}
	f := &fake{}
	results, evs, err := runCollect(t, context.Background(), "/proj", "reg:5000", specs, f)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// One Ingest+Push per unique path, in appearance order.
	wantDirs := []string{"/proj/svc/a", "/proj"}
	if !reflect.DeepEqual(f.ingests, wantDirs) {
		t.Errorf("ingests = %v, want %v", f.ingests, wantDirs)
	}
	if !reflect.DeepEqual(f.pushes, wantDirs) {
		t.Errorf("pushes = %v, want %v", f.pushes, wantDirs)
	}

	// Results in spec order, all done, K/ImageRef set at submit.
	if len(results) != len(specs) {
		t.Fatalf("got %d results, want %d", len(results), len(specs))
	}
	for i, res := range results {
		if res.Spec.Name != specs[i].Name {
			t.Errorf("results[%d].Spec.Name = %q, want %q", i, res.Spec.Name, specs[i].Name)
		}
		if res.State != spec.StateDone {
			t.Errorf("results[%d].State = %q, want done", i, res.State)
		}
		if res.Err != "" {
			t.Errorf("results[%d].Err = %q, want empty", i, res.Err)
		}
		k := kFor(specs[i].DisplayName())
		if res.K != k {
			t.Errorf("results[%d].K = %q, want %q", i, res.K, k)
		}
		if want := spec.ImageRef("reg:5000", k); res.ImageRef != want {
			t.Errorf("results[%d].ImageRef = %q, want %q", i, res.ImageRef, want)
		}
	}

	// Exact per-build event sequence: pushing → infos → building → follow
	// output → done. The fake's terminal sink.State must not appear.
	for i := range specs {
		want := []spec.Event{
			{Build: i, Kind: spec.KindState, State: spec.StatePushing},
			{Build: i, Kind: spec.KindInfo, Info: "ingesting"},
			{Build: i, Kind: spec.KindInfo, Info: "push 1/2 objects"},
			{Build: i, Kind: spec.KindInfo, Info: "push 2/2 objects"},
			{Build: i, Kind: spec.KindState, State: spec.StateBuilding},
			{Build: i, Kind: spec.KindInfo, Info: "1/1 running"},
			{Build: i, Kind: spec.KindLog, Line: "hello"},
			{Build: i, Kind: spec.KindState, State: spec.StateDone},
		}
		if got := perBuild(evs, i); !reflect.DeepEqual(got, want) {
			t.Errorf("build %d events:\n got %+v\nwant %+v", i, got, want)
		}
	}
	if len(f.cancels) != 0 {
		t.Errorf("unexpected cancels: %v", f.cancels)
	}
}

func TestTerminalMapping(t *testing.T) {
	transport := errors.New("stream torn down")
	cases := []struct {
		name      string
		follow    followFn
		submitErr error
		wantState spec.BuildState
		wantErr   string // substring of Result.Err; "" = must be empty
		wantRun   string // exact Run error text; "" = nil
		wantK     bool
	}{
		{name: "done", wantState: spec.StateDone, wantK: true},
		{name: "failed", follow: staticFollow(spec.StateFailed, nil),
			wantState: spec.StateFailed, wantErr: "build failed", wantRun: "1 of 1 builds failed", wantK: true},
		{name: "cancelled", follow: staticFollow(spec.StateCancelled, nil),
			wantState: spec.StateCancelled, wantErr: "cancelled", wantRun: "1 of 1 builds cancelled", wantK: true},
		{name: "transport-error", follow: staticFollow("", transport),
			wantState: spec.StateFailed, wantErr: "stream torn down", wantRun: "1 of 1 builds failed", wantK: true},
		{name: "non-terminal", follow: staticFollow(spec.StateBuilding, nil),
			wantState: spec.StateFailed, wantErr: "non-terminal", wantRun: "1 of 1 builds failed", wantK: true},
		{name: "submit-error", submitErr: errors.New("no quota"),
			wantState: spec.StateFailed, wantErr: "no quota", wantRun: "1 of 1 builds failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{}
			if tc.follow != nil {
				f.follow = map[string]followFn{"x": tc.follow}
			}
			if tc.submitErr != nil {
				f.submitErr = map[string]error{"x": tc.submitErr}
			}
			sp := []spec.BuildSpec{{Name: "x", Path: "svc/x", Platform: "linux/amd64"}}
			results, evs, err := runCollect(t, context.Background(), "/r", "reg", sp, f)

			if tc.wantRun == "" {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			} else if err == nil || err.Error() != tc.wantRun {
				t.Fatalf("Run error = %v, want %q", err, tc.wantRun)
			}
			res := results[0]
			if res.State != tc.wantState {
				t.Errorf("State = %q, want %q", res.State, tc.wantState)
			}
			if tc.wantErr == "" {
				if res.Err != "" {
					t.Errorf("Err = %q, want empty", res.Err)
				}
			} else if !strings.Contains(res.Err, tc.wantErr) {
				t.Errorf("Err = %q, want containing %q", res.Err, tc.wantErr)
			}
			if tc.wantK {
				if res.K != kFor("x") || res.ImageRef != spec.ImageRef("reg", kFor("x")) {
					t.Errorf("K/ImageRef = %q/%q, want set", res.K, res.ImageRef)
				}
			} else if res.K != "" || res.ImageRef != "" {
				t.Errorf("K/ImageRef = %q/%q, want unset", res.K, res.ImageRef)
			}

			got := perBuild(evs, 0)
			last := got[len(got)-1]
			wantInfo := ""
			if tc.wantState == spec.StateFailed {
				wantInfo = res.Err
			}
			want := spec.Event{Build: 0, Kind: spec.KindState, State: tc.wantState, Info: wantInfo}
			if last != want {
				t.Errorf("terminal event = %+v, want %+v", last, want)
			}
		})
	}
}

// TestPipelining proves the next group's push proceeds while an earlier
// group's build is still following: a's Follow returns done only once b's
// push has started (a broken, non-pipelined orchestrator fails after guard).
func TestPipelining(t *testing.T) {
	push2Started := make(chan struct{})
	dir2 := "/r/svc/b"
	f := &fake{
		pushHook: map[string]func(ctx context.Context) error{
			dir2: func(context.Context) error { close(push2Started); return nil },
		},
		follow: map[string]followFn{
			"a": func(context.Context, jobs.Sink) (spec.BuildState, error) {
				select {
				case <-push2Started:
					return spec.StateDone, nil
				case <-time.After(guard):
					return spec.StateFailed, errors.New("group 2 push never started while group 1 followed")
				}
			},
		},
	}
	specs := []spec.BuildSpec{
		{Name: "a", Path: "svc/a", Platform: "linux/amd64"},
		{Name: "b", Path: "svc/b", Platform: "linux/amd64"},
	}
	results, _, err := runCollect(t, context.Background(), "/r", "reg", specs, f)
	if err != nil {
		t.Fatalf("Run: %v (%+v)", err, results)
	}
	// Pushes stay sequential in group order even though follows overlap.
	if want := []string{"/r/svc/a", dir2}; !reflect.DeepEqual(f.pushes, want) {
		t.Errorf("pushes = %v, want %v", f.pushes, want)
	}
}

// TestConcurrentFollows forces the follows of one group to overlap: each
// blocks until both have been entered (serialized follows fail after guard).
func TestConcurrentFollows(t *testing.T) {
	var mu sync.Mutex
	entered := 0
	both := make(chan struct{})
	rendezvous := func(context.Context, jobs.Sink) (spec.BuildState, error) {
		mu.Lock()
		entered++
		if entered == 2 {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
			return spec.StateDone, nil
		case <-time.After(guard):
			return spec.StateFailed, errors.New("follows did not overlap")
		}
	}
	f := &fake{follow: map[string]followFn{"a": rendezvous, "b": rendezvous}}
	specs := []spec.BuildSpec{
		{Name: "a", Path: "svc/p", Platform: "linux/amd64"},
		{Name: "b", Path: "svc/p", Platform: "linux/arm64"},
	}
	results, _, err := runCollect(t, context.Background(), "/r", "reg", specs, f)
	if err != nil {
		t.Fatalf("Run: %v (%+v)", err, results)
	}
}

func TestFailureIsolation(t *testing.T) {
	f := &fake{follow: map[string]followFn{"bad": staticFollow(spec.StateFailed, nil)}}
	specs := []spec.BuildSpec{
		{Name: "a", Path: "svc/p", Platform: "linux/amd64"},
		{Name: "bad", Path: "svc/p", Platform: "linux/arm64"},
		{Name: "c", Path: "svc/q", Platform: "linux/amd64"},
	}
	results, _, err := runCollect(t, context.Background(), "/r", "reg", specs, f)
	if err == nil || err.Error() != "1 of 3 builds failed" {
		t.Fatalf("Run error = %v, want %q", err, "1 of 3 builds failed")
	}
	wantStates := []spec.BuildState{spec.StateDone, spec.StateFailed, spec.StateDone}
	for i, want := range wantStates {
		if results[i].State != want {
			t.Errorf("results[%d].State = %q, want %q", i, results[i].State, want)
		}
	}
}

// TestGroupSetupFailure: an ingest or push error fails only that group's
// builds; later groups build normally.
func TestGroupSetupFailure(t *testing.T) {
	boom := errors.New("disk on fire")
	dirA, dirC := "/r/svc/a", "/r/svc/c"
	cases := []struct {
		mode       string
		wantErr    string   // exact Result.Err of the failed builds
		wantPushes []string // pushes recorded (fake records before failing)
	}{
		{"ingest", "ingest svc/a: disk on fire", []string{dirC}},
		{"push", "push svc/a: disk on fire", []string{dirA, dirC}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			f := &fake{}
			if tc.mode == "ingest" {
				f.ingestErr = map[string]error{dirA: boom}
			} else {
				f.pushErr = map[string]error{dirA: boom}
			}
			specs := []spec.BuildSpec{
				{Name: "a", Path: "svc/a", Platform: "linux/amd64"},
				{Name: "b", Path: "svc/a", Platform: "linux/arm64"},
				{Name: "c", Path: "svc/c", Platform: "linux/amd64"},
			}
			results, evs, err := runCollect(t, context.Background(), "/r", "reg", specs, f)
			if err == nil || err.Error() != "2 of 3 builds failed" {
				t.Fatalf("Run error = %v, want %q", err, "2 of 3 builds failed")
			}
			for i := 0; i < 2; i++ {
				if results[i].State != spec.StateFailed {
					t.Errorf("results[%d].State = %q, want failed", i, results[i].State)
				}
				if results[i].Err != tc.wantErr {
					t.Errorf("results[%d].Err = %q, want %q", i, results[i].Err, tc.wantErr)
				}
				want := []spec.Event{
					{Build: i, Kind: spec.KindState, State: spec.StatePushing},
					{Build: i, Kind: spec.KindInfo, Info: "ingesting"},
					{Build: i, Kind: spec.KindState, State: spec.StateFailed, Info: tc.wantErr},
				}
				if got := perBuild(evs, i); !reflect.DeepEqual(got, want) {
					t.Errorf("build %d events:\n got %+v\nwant %+v", i, got, want)
				}
			}
			if results[2].State != spec.StateDone {
				t.Errorf("results[2].State = %q, want done", results[2].State)
			}
			if !reflect.DeepEqual(f.pushes, tc.wantPushes) {
				t.Errorf("pushes = %v, want %v", f.pushes, tc.wantPushes)
			}
			if want := []string{"c"}; !reflect.DeepEqual(f.submits, want) {
				t.Errorf("submits = %v, want %v", f.submits, want)
			}
		})
	}
}

// TestCancellation: on ctx cancellation the in-flight build gets a Cancel on
// a fresh deadline-bearing context and its Follow drains to cancelled; the
// group whose push aborts and the never-started group are marked cancelled
// without ever being submitted (or, for the last one, ingested).
func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	followedA := make(chan struct{})
	releaseA := make(chan struct{})
	push2Started := make(chan struct{})
	dir2 := "/r/svc/b"
	f := &fake{
		follow: map[string]followFn{
			"a": func(context.Context, jobs.Sink) (spec.BuildState, error) {
				close(followedA)
				select {
				case <-releaseA:
					return spec.StateCancelled, nil
				case <-time.After(guard):
					return spec.StateFailed, errors.New("cancel never arrived")
				}
			},
		},
		pushHook: map[string]func(ctx context.Context) error{
			dir2: func(ctx context.Context) error {
				close(push2Started)
				<-ctx.Done()
				return ctx.Err()
			},
		},
		onCancel: func(jobs.Handle) { close(releaseA) },
	}
	go func() {
		<-followedA
		<-push2Started
		cancel()
	}()
	specs := []spec.BuildSpec{
		{Name: "a", Path: "svc/a", Platform: "linux/amd64"},
		{Name: "b", Path: "svc/b", Platform: "linux/amd64"},
		{Name: "c", Path: "svc/c", Platform: "linux/amd64"},
	}
	results, evs, err := runCollect(t, ctx, "/r", "reg", specs, f)
	if err == nil || err.Error() != "3 of 3 builds cancelled" {
		t.Fatalf("Run error = %v, want %q", err, "3 of 3 builds cancelled")
	}
	for i := range results {
		if results[i].State != spec.StateCancelled {
			t.Errorf("results[%d].State = %q, want cancelled", i, results[i].State)
		}
		if results[i].Err != "cancelled" {
			t.Errorf("results[%d].Err = %q, want %q", i, results[i].Err, "cancelled")
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Only the in-flight build was cancelled, on a fresh (uncancelled)
	// context that carries the grace deadline.
	if len(f.cancels) != 1 || f.cancels[0].RequestID != "req-a" {
		t.Fatalf("cancels = %v, want exactly req-a", f.cancels)
	}
	if f.cancelErrs[0] != nil {
		t.Errorf("Cancel ctx already dead: %v", f.cancelErrs[0])
	}
	if !f.cancelDls[0] {
		t.Errorf("Cancel ctx has no deadline")
	}
	// c's group was never ingested; only a was submitted.
	if want := []string{"/r/svc/a", dir2}; !reflect.DeepEqual(f.ingests, want) {
		t.Errorf("ingests = %v, want %v", f.ingests, want)
	}
	if want := []string{"a"}; !reflect.DeepEqual(f.submits, want) {
		t.Errorf("submits = %v, want %v", f.submits, want)
	}

	wantA := []spec.Event{
		{Build: 0, Kind: spec.KindState, State: spec.StatePushing},
		{Build: 0, Kind: spec.KindInfo, Info: "ingesting"},
		{Build: 0, Kind: spec.KindInfo, Info: "push 1/2 objects"},
		{Build: 0, Kind: spec.KindInfo, Info: "push 2/2 objects"},
		{Build: 0, Kind: spec.KindState, State: spec.StateBuilding},
		{Build: 0, Kind: spec.KindState, State: spec.StateCancelled},
	}
	if got := perBuild(evs, 0); !reflect.DeepEqual(got, wantA) {
		t.Errorf("build 0 events:\n got %+v\nwant %+v", got, wantA)
	}
	wantB := []spec.Event{
		{Build: 1, Kind: spec.KindState, State: spec.StatePushing},
		{Build: 1, Kind: spec.KindInfo, Info: "ingesting"},
		{Build: 1, Kind: spec.KindState, State: spec.StateCancelled},
	}
	if got := perBuild(evs, 1); !reflect.DeepEqual(got, wantB) {
		t.Errorf("build 1 events:\n got %+v\nwant %+v", got, wantB)
	}
	wantC := []spec.Event{{Build: 2, Kind: spec.KindState, State: spec.StateCancelled}}
	if got := perBuild(evs, 2); !reflect.DeepEqual(got, wantC) {
		t.Errorf("build 2 events:\n got %+v\nwant %+v", got, wantC)
	}
}

// TestCancelBeforeSubmit: cancellation landing between a successful push and
// Submit marks the build cancelled without submitting or cancelling anything.
func TestCancelBeforeSubmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fake{}
	f.pushHook = map[string]func(ctx context.Context) error{
		"/r/svc/a": func(context.Context) error {
			cancel() // synchronous: ctx.Err() != nil once this returns
			return nil
		},
	}
	specs := []spec.BuildSpec{{Name: "a", Path: "svc/a", Platform: "linux/amd64"}}
	results, _, err := runCollect(t, ctx, "/r", "reg", specs, f)
	if err == nil || err.Error() != "1 of 1 builds cancelled" {
		t.Fatalf("Run error = %v, want %q", err, "1 of 1 builds cancelled")
	}
	if results[0].State != spec.StateCancelled {
		t.Errorf("State = %q, want cancelled", results[0].State)
	}
	if len(f.submits) != 0 {
		t.Errorf("submits = %v, want none", f.submits)
	}
	if len(f.cancels) != 0 {
		t.Errorf("cancels = %v, want none", f.cancels)
	}
}

// TestCancelDrainBound: with the best-effort Cancel lost (Cancel errors)
// and a Follow that only returns once its context dies, Run must still
// return within the drain bound after user cancellation — stopFollows trips
// the follow context — and the wedged build ends cancelled, not failed.
func TestCancelDrainBound(t *testing.T) {
	old := drainGrace
	drainGrace = 50 * time.Millisecond
	defer func() { drainGrace = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	followed := make(chan struct{})
	f := &fake{
		cancelErr: errors.New("cancel frame lost"),
		follow: map[string]followFn{
			"a": func(fctx context.Context, _ jobs.Sink) (spec.BuildState, error) {
				close(followed)
				// The real Follow blocks in a stream read whose armed
				// deadline (internal/jobs/conn.go) trips when fctx dies.
				<-fctx.Done()
				return "", fctx.Err()
			},
		},
	}
	go func() {
		<-followed
		cancel()
	}()
	specs := []spec.BuildSpec{{Name: "a", Path: "svc/a", Platform: "linux/amd64"}}

	type out struct {
		results []Result
		err     error
	}
	done := make(chan out, 1)
	go func() {
		results, _, err := runCollect(t, ctx, "/r", "reg", specs, f)
		done <- out{results, err}
	}()
	var got out
	select {
	case got = <-done:
	case <-time.After(guard):
		t.Fatal("Run did not return within the drain bound")
	}
	if got.err == nil || got.err.Error() != "1 of 1 builds cancelled" {
		t.Fatalf("Run error = %v, want %q", got.err, "1 of 1 builds cancelled")
	}
	if got.results[0].State != spec.StateCancelled || got.results[0].Err != "cancelled" {
		t.Errorf("result = %+v, want cancelled/cancelled", got.results[0])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cancels) != 1 || f.cancels[0].RequestID != "req-a" {
		t.Errorf("cancels = %v, want exactly req-a", f.cancels)
	}
}

// TestInFlightBound: concurrent submit+Follow pairs never exceed the
// in-flight cap yet do saturate it, and every queued build still completes.
func TestInFlightBound(t *testing.T) {
	const bound = 3
	old := inFlightSlots
	inFlightSlots = bound
	defer func() { inFlightSlots = old }()

	const n = 10
	var active, peak atomic.Int32
	release := make(chan struct{})
	gated := func(context.Context, jobs.Sink) (spec.BuildState, error) {
		c := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		select {
		case <-release:
			return spec.StateDone, nil
		case <-time.After(guard):
			return spec.StateFailed, errors.New("release never came")
		}
	}
	f := &fake{follow: map[string]followFn{}}
	specs := make([]spec.BuildSpec, n)
	for i := range specs {
		name := fmt.Sprintf("b%d", i)
		specs[i] = spec.BuildSpec{Name: name, Path: "svc/p", Platform: "linux/amd64"}
		f.follow[name] = gated
	}
	go func() {
		// Release everyone once the cap is saturated; on a broken (serial
		// or unbounded) orchestrator the follows fail the test via guard.
		deadline := time.Now().Add(guard)
		for peak.Load() < bound && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		close(release)
	}()
	results, _, err := runCollect(t, context.Background(), "/r", "reg", specs, f)
	if err != nil {
		t.Fatalf("Run: %v (%+v)", err, results)
	}
	if got := peak.Load(); got != bound {
		t.Errorf("peak concurrent follows = %d, want exactly %d", got, bound)
	}
	for i := range results {
		if results[i].State != spec.StateDone {
			t.Errorf("results[%d].State = %q, want done", i, results[i].State)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	failed := staticFollow(spec.StateFailed, nil)
	cancelled := staticFollow(spec.StateCancelled, nil)
	cases := []struct {
		name    string
		follows map[string]followFn
		want    string
	}{
		{"one-of-two-failed", map[string]followFn{"a": failed}, "1 of 2 builds failed"},
		{"all-cancelled", map[string]followFn{"a": cancelled, "b": cancelled}, "2 of 2 builds cancelled"},
		{"mixed-counts-as-failed", map[string]followFn{"a": failed, "b": cancelled}, "2 of 2 builds failed"},
	}
	specs := []spec.BuildSpec{
		{Name: "a", Path: "svc/p", Platform: "linux/amd64"},
		{Name: "b", Path: "svc/p", Platform: "linux/arm64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{follow: tc.follows}
			_, _, err := runCollect(t, context.Background(), "/r", "reg", specs, f)
			if err == nil || err.Error() != tc.want {
				t.Errorf("Run error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNoSpecs(t *testing.T) {
	f := &fake{}
	results, evs, err := runCollect(t, context.Background(), "/r", "reg", nil, f)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 0 || len(evs) != 0 {
		t.Errorf("results = %v, events = %v, want none", results, evs)
	}
	if len(f.ingests) != 0 {
		t.Errorf("ingests = %v, want none", f.ingests)
	}
}
