package ui

import (
	"fmt"
	"slices"
	"testing"
)

func TestRing(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		push  int // pushes "l0".."l<n-1>"
		want  []string
	}{
		{"empty", 3, 0, []string{}},
		{"under capacity", 3, 2, []string{"l0", "l1"}},
		{"exactly full", 3, 3, []string{"l0", "l1", "l2"}},
		{"drops oldest", 3, 5, []string{"l2", "l3", "l4"}},
		{"wraps repeatedly", 2, 7, []string{"l5", "l6"}},
		{"capacity one", 1, 4, []string{"l3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRing(tt.limit)
			for i := 0; i < tt.push; i++ {
				r.push(fmt.Sprintf("l%d", i))
			}
			if got := r.lines(); !slices.Equal(got, tt.want) {
				t.Errorf("lines() = %v, want %v", got, tt.want)
			}
			if r.count() != len(tt.want) {
				t.Errorf("count() = %d, want %d", r.count(), len(tt.want))
			}
		})
	}
}

// The model's per-build buffer trims at maxLogLines.
func TestModelRingTrim(t *testing.T) {
	m := newTestModel(t, []string{"api"}, nil, nil)
	m = drive(m, sizeMsg(80, 24))
	for i := 0; i < maxLogLines+7; i++ {
		m = drive(m, logEv(0, fmt.Sprintf("line %d", i)))
	}
	got := m.rows[0].log.lines()
	if len(got) != maxLogLines {
		t.Fatalf("ring holds %d lines, want %d", len(got), maxLogLines)
	}
	if got[0] != "line 7" {
		t.Errorf("oldest line = %q, want %q", got[0], "line 7")
	}
	if last := got[len(got)-1]; last != fmt.Sprintf("line %d", maxLogLines+6) {
		t.Errorf("newest line = %q, want %q", last, fmt.Sprintf("line %d", maxLogLines+6))
	}
}
