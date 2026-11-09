package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// refPresent checks ref existence via ListRefs — GetKey/GetRef would fire
// the GC access observer and reset the ref's clock, defeating expiry
// assertions (the jobs-iroh test-trap documented in its CLAUDE.md).
func refPresent(t *testing.T, l *Local, name string) bool {
	t.Helper()
	refs, err := l.store.ListRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Name == name {
			return true
		}
	}
	return false
}

// Auto-GC: the first MaybeGC seeds the tracker and writes the stamp; a
// fresh stamp suppresses sweeping; a backdated stamp lets the next MaybeGC
// expire a cold ref.
func TestMaybeGC(t *testing.T) {
	t.Setenv("JOBS_GC_RETENTION", "300ms")
	ctx := context.Background()
	dataDir := t.TempDir()

	l, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.gc == nil {
		t.Fatal("gc sweeper not constructed")
	}

	k, err := l.store.IngestFile(ctx, []byte("assimilate gc fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.store.PutRef(ctx, "build-output:cold", k); err != nil {
		t.Fatal(err)
	}

	l.MaybeGC(ctx) // seeds the tracker, writes the stamp
	stamp := filepath.Join(dataDir, "gc.stamp")
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("stamp not written: %v", err)
	}

	// Fresh stamp: MaybeGC must not sweep, even past retention.
	time.Sleep(400 * time.Millisecond)
	l.MaybeGC(ctx)
	if !refPresent(t, l, "build-output:cold") {
		t.Fatal("swept despite fresh stamp")
	}

	// Backdated stamp: the next MaybeGC sweeps and expires the cold ref.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	l.MaybeGC(ctx)
	if refPresent(t, l, "build-output:cold") {
		t.Fatal("cold ref survived a due sweep")
	}
}

func TestMaybeGCDisabledByEnv(t *testing.T) {
	t.Setenv("JOBS_GC_RETENTION", "0")
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.gc != nil {
		t.Fatal("gc constructed despite retention 0")
	}
	l.MaybeGC(context.Background()) // must be a no-op, not a panic
}
