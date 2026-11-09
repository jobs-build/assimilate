package ui

// ring is a fixed-capacity line buffer: a push past capacity drops the
// oldest line.
type ring struct {
	buf   []string
	head  int // index of the oldest line once full
	limit int
}

func newRing(limit int) *ring {
	return &ring{limit: limit}
}

func (r *ring) push(s string) {
	if len(r.buf) < r.limit {
		r.buf = append(r.buf, s)
		return
	}
	r.buf[r.head] = s
	r.head = (r.head + 1) % r.limit
}

// lines returns the buffered lines oldest-first as a fresh slice (callers
// truncate in place).
func (r *ring) lines() []string {
	out := make([]string, 0, len(r.buf))
	out = append(out, r.buf[r.head:]...)
	out = append(out, r.buf[:r.head]...)
	return out
}

func (r *ring) count() int { return len(r.buf) }
