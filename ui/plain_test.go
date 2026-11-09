package ui

import (
	"bytes"
	"testing"

	"github.com/jobs-build/assimilate/spec"
)

func runPlain(names []string, evs []spec.Event) string {
	ch := make(chan spec.Event, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	var buf bytes.Buffer
	RunPlain(&buf, names, ch)
	return buf.String()
}

func TestRunPlain(t *testing.T) {
	names := []string{"api", "worker"}
	tests := []struct {
		name   string
		events []spec.Event
		want   string
	}{
		{
			name: "log lines prefixed",
			events: []spec.Event{
				{Build: 0, Kind: spec.KindLog, Line: "go build ./..."},
				{Build: 1, Kind: spec.KindLog, Line: "npm ci"},
			},
			want: "[api] go build ./...\n[worker] npm ci\n",
		},
		{
			name: "state with and without info",
			events: []spec.Event{
				{Build: 0, Kind: spec.KindState, State: spec.StatePushing},
				{Build: 0, Kind: spec.KindState, State: spec.StateBuilding},
				{Build: 0, Kind: spec.KindState, State: spec.StateFailed, Info: "exit status 1"},
			},
			want: "[api] ▸ pushing\n[api] ▸ building\n[api] ▸ failed (exit status 1)\n",
		},
		{
			name: "info deduped per build",
			events: []spec.Event{
				{Build: 0, Kind: spec.KindInfo, Info: "push 10%"},
				{Build: 0, Kind: spec.KindInfo, Info: "push 10%"}, // suppressed
				{Build: 0, Kind: spec.KindInfo, Info: "push 90%"},
				{Build: 0, Kind: spec.KindInfo, Info: "push 90%"}, // suppressed
				{Build: 1, Kind: spec.KindInfo, Info: "push 90%"}, // other build: prints
				{Build: 0, Kind: spec.KindInfo, Info: "push 10%"}, // changed again: prints
			},
			want: "[api] push 10%\n[api] push 90%\n[worker] push 90%\n[api] push 10%\n",
		},
		{
			name: "empty info suppressed",
			events: []spec.Event{
				{Build: 0, Kind: spec.KindInfo, Info: ""},
			},
			want: "",
		},
		{
			name: "global events print bare",
			events: []spec.Event{
				{Build: -1, Kind: spec.KindLog, Line: "3 builds queued"},
				{Build: -1, Kind: spec.KindInfo, Info: "sources pushed"},
				{Build: -1, Kind: spec.KindInfo, Info: "sources pushed"}, // deduped too
			},
			want: "3 builds queued\nsources pushed\n",
		},
		{
			// The TUI sanitizes log lines; RunPlain must not — non-TTY
			// consumers get the raw bytes.
			name: "log lines stay raw",
			events: []spec.Event{
				{Build: 0, Kind: spec.KindLog, Line: "\x1b[31m10%\r50%\tdone\x07"},
			},
			want: "[api] \x1b[31m10%\r50%\tdone\x07\n",
		},
		{
			name: "out-of-range build index degrades",
			events: []spec.Event{
				{Build: 7, Kind: spec.KindLog, Line: "orphan"},
			},
			want: "[#7] orphan\n",
		},
		{
			name: "interleaved arrival order preserved",
			events: []spec.Event{
				{Build: 0, Kind: spec.KindState, State: spec.StatePushing},
				{Build: 1, Kind: spec.KindState, State: spec.StatePushing},
				{Build: 0, Kind: spec.KindLog, Line: "step 1"},
				{Build: 1, Kind: spec.KindLog, Line: "step A"},
				{Build: 0, Kind: spec.KindState, State: spec.StateDone},
				{Build: 1, Kind: spec.KindState, State: spec.StateDone},
			},
			want: "[api] ▸ pushing\n[worker] ▸ pushing\n[api] step 1\n[worker] step A\n[api] ▸ done\n[worker] ▸ done\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runPlain(names, tt.events); got != tt.want {
				t.Errorf("output mismatch\ngot:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

// RunPlain returns exactly when the channel closes, even mid-stream.
func TestRunPlainReturnsOnClose(t *testing.T) {
	ch := make(chan spec.Event)
	close(ch)
	var buf bytes.Buffer
	RunPlain(&buf, nil, ch) // must not block
	if buf.Len() != 0 {
		t.Errorf("unexpected output: %q", buf.String())
	}
}
