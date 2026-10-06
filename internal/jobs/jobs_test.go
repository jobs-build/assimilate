package jobs

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/builddef"
	"github.com/jobs-build/jobs-iroh/wire"

	"github.com/jobs-build/assimilate/internal/spec"
)

func TestDefaultDataDir(t *testing.T) {
	cases := []struct {
		name               string
		dataDir, xdg, home string
		want               string
	}{
		{name: "explicit", dataDir: "/data/assim", xdg: "/xdg", home: "/home/u", want: "/data/assim"},
		{name: "xdg", xdg: "/xdg", home: "/home/u", want: filepath.Join("/xdg", "assimilate")},
		{name: "home", home: "/home/u", want: filepath.Join("/home/u", ".local", "share", "assimilate")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ASSIMILATE_DATA_DIR", tc.dataDir)
			t.Setenv("XDG_DATA_HOME", tc.xdg)
			t.Setenv("HOME", tc.home)
			if got := DefaultDataDir(); got != tc.want {
				t.Fatalf("DefaultDataDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestLocalIngestDefinitionKey drives the offline half against a real store
// and a fixture source tree: K is 64-hex, stable across repeated ingests and
// calls, distinct per args, and moves when a source file changes.
func TestLocalIngestDefinitionKey(t *testing.T) {
	ctx := context.Background()
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	srcDir := t.TempDir()
	writeFile(t, srcDir, "BUILD.jobs", "# recipe\n")
	writeFile(t, srcDir, "svc/main.go", "package main\n")

	src, err := l.Ingest(ctx, srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if !hex64.MatchString(src.String()) {
		t.Fatalf("source key %q is not 64-hex", src.String())
	}

	s := spec.BuildSpec{Path: "/", Platform: "linux/amd64", Args: map[string]string{"variant": "slim"}}
	k1, err := l.DefinitionKey(src, s)
	if err != nil {
		t.Fatal(err)
	}
	if !hex64.MatchString(k1) {
		t.Fatalf("K %q is not 64-hex", k1)
	}

	// Stable: same call, and a fresh ingest of unchanged sources.
	if k2, err := l.DefinitionKey(src, s); err != nil || k2 != k1 {
		t.Fatalf("repeat DefinitionKey = %q, %v; want %q", k2, err, k1)
	}
	src2, err := l.Ingest(ctx, srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if src2.String() != src.String() {
		t.Fatalf("re-ingest key %s, want %s", src2, src)
	}
	if k2, err := l.DefinitionKey(src2, s); err != nil || k2 != k1 {
		t.Fatalf("re-ingest DefinitionKey = %q, %v; want %q", k2, err, k1)
	}

	// Distinct per definition knobs.
	for name, other := range map[string]spec.BuildSpec{
		"args":       {Path: "/", Platform: "linux/amd64", Args: map[string]string{"variant": "full"}},
		"platform":   {Path: "/", Platform: "linux/arm64", Args: map[string]string{"variant": "slim"}},
		"build-file": {Path: "/", Platform: "linux/amd64", Args: map[string]string{"variant": "slim"}, BuildFile: "BUILD.prod"},
	} {
		if k, err := l.DefinitionKey(src, other); err != nil || k == k1 {
			t.Fatalf("%s variant: K = %q, %v; must differ from %q", name, k, err, k1)
		}
	}

	// A source change moves the tree key and therefore K.
	writeFile(t, srcDir, "svc/main.go", "package main // changed\n")
	src3, err := l.Ingest(ctx, srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if src3.String() == src.String() {
		t.Fatal("changed source must change the tree key")
	}
	k3, err := l.DefinitionKey(src3, s)
	if err != nil {
		t.Fatal(err)
	}
	if k3 == k1 {
		t.Fatal("changed source must change K")
	}

	// The zero Source is for fakes only — the real path must refuse it.
	if _, err := l.DefinitionKey(Source{}, s); err == nil {
		t.Fatal("DefinitionKey of a zero Source must fail")
	}
}

// TestOpenConflict: the store flock is single-process; a second Open of the
// same data dir fails fast with the descriptive error.
func TestOpenConflict(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "another assimilate is using") {
		t.Fatalf("second Open error = %v, want 'another assimilate is using …'", err)
	}
}

func TestScratchRef(t *testing.T) {
	re := regexp.MustCompile(`^client-push/[0-9a-f]{16}$`)
	a, err := scratchRef()
	if err != nil {
		t.Fatal(err)
	}
	b, err := scratchRef()
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchString(a) || !re.MatchString(b) {
		t.Fatalf("scratch refs %q, %q do not match %v", a, b, re)
	}
	if a == b {
		t.Fatalf("scratch refs must be fresh per push, got %q twice", a)
	}
}

func TestCountsSummary(t *testing.T) {
	cases := []struct {
		counts wire.Counts
		want   string
	}{
		{wire.Counts{Total: 7, Done: 3, Running: 1}, "3/7 built · 1 running"},
		{wire.Counts{Total: 7, Done: 7}, "7/7 built"},
		{wire.Counts{Total: 5, Done: 2, Running: 2, Failed: 1}, "2/5 built · 2 running · 1 failed"},
		{wire.Counts{Total: 4, Done: 1, Failed: 3}, "1/4 built · 3 failed"},
		{wire.Counts{}, "0/0 built"},
	}
	for _, tc := range cases {
		if got := countsSummary(tc.counts); got != tc.want {
			t.Errorf("countsSummary(%+v) = %q, want %q", tc.counts, got, tc.want)
		}
	}
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// project lays out a small monorepo and returns its root.
func project(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "services/a/BUILD.jobs", "# recipe\n")
	writeFile(t, root, "services/a/main.go", "package main\n")
	writeFile(t, root, "lib/x.go", "package lib\n")
	writeFile(t, root, "other/y.txt", "y\n")
	return root
}

// keyOf computes K of s over root with a fresh store, as one assimilate run
// would.
func keyOf(t *testing.T, root string, s spec.BuildSpec) string {
	t.Helper()
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	src, err := l.SourceFor(context.Background(), root, s)
	if err != nil {
		t.Fatal(err)
	}
	k, err := l.DefinitionKey(src, s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSourceForWithoutSourcesIsTheSubtree(t *testing.T) {
	ctx := context.Background()
	root := project(t)
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	s := spec.BuildSpec{Path: "/services/a", Platform: "linux/amd64"}
	src, err := l.SourceFor(ctx, root, s)
	if err != nil {
		t.Fatal(err)
	}
	subtree, err := l.Ingest(ctx, filepath.Join(root, "services", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if src.String() != subtree.String() {
		t.Fatalf("source %s, want the subtree ingest %s", src, subtree)
	}

	// The definition is what it always was: the subtree as the build root,
	// no Dir, no Ctx.
	in, err := builddef.TreeInput(src.key)
	if err != nil {
		t.Fatal(err)
	}
	params, err := canonicalParams(nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := builddef.Definition{Source: in, Platform: "linux/amd64", Params: params}.Key()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := l.DefinitionKey(src, s); err != nil || got != want.String() {
		t.Fatalf("K = %s, %v; want %s", got, err, want)
	}
	if len(l.roots) != 0 {
		t.Error("a spec without sources ingested the project root")
	}
}

func TestSourceForWidensTheDefinition(t *testing.T) {
	ctx := context.Background()
	root := project(t)
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	s := spec.BuildSpec{Path: "/services/a", Sources: []string{"/lib"}, Platform: "linux/amd64", BuildFile: "BUILD.prod"}
	src, err := l.SourceFor(ctx, root, s)
	if err != nil {
		t.Fatal(err)
	}
	// The context holds exactly the covered paths, in the project layout.
	top, err := l.store.Ls(ctx, src.key, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range top {
		names = append(names, e.Name)
	}
	if want := []string{"lib", "services"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("context root holds %v, want %v", names, want)
	}

	in, err := builddef.TreeInput(src.key)
	if err != nil {
		t.Fatal(err)
	}
	params, err := canonicalParams(nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := builddef.Definition{
		Source: in, Dir: "services/a", Platform: "linux/amd64", Params: params,
		BuildFile: "BUILD.prod", Ctx: builddef.CtxWidened,
	}.Key()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := l.DefinitionKey(src, s); err != nil || got != want.String() {
		t.Fatalf("K = %s, %v; want %s", got, err, want)
	}
}

func TestSourceForKey(t *testing.T) {
	root := project(t)
	s := spec.BuildSpec{Path: "/services/a", Sources: []string{"/lib"}, Platform: "linux/amd64"}
	base := keyOf(t, root, s)

	if again := keyOf(t, root, s); again != base {
		t.Fatalf("K is not stable: %s then %s", base, again)
	}

	// Outside the covered paths: no effect.
	writeFile(t, root, "other/y.txt", "changed\n")
	writeFile(t, root, "new/z.txt", "z\n")
	if got := keyOf(t, root, s); got != base {
		t.Error("a change outside the covered paths moved K")
	}

	// Only timestamps inside them: no effect.
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, p := range []string{"lib/x.go", "lib", "services/a/main.go", "services/a", "services"} {
		if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(p)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if got := keyOf(t, root, s); got != base {
		t.Error("changed mtimes inside the covered paths moved K")
	}

	// Content inside them: K moves.
	writeFile(t, root, "lib/x.go", "package lib // changed\n")
	afterLib := keyOf(t, root, s)
	if afterLib == base {
		t.Error("a change in a source did not move K")
	}
	writeFile(t, root, "services/a/main.go", "package main // changed\n")
	if got := keyOf(t, root, s); got == afterLib {
		t.Error("a change in the build path did not move K")
	}
}

func TestSourceForErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("a source that does not exist", func(t *testing.T) {
		root := project(t)
		l, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		s := spec.BuildSpec{Name: "api", Path: "/services/a", Sources: []string{"/nope"}, Platform: "linux/amd64"}
		_, err = l.SourceFor(ctx, root, s)
		if err == nil || !strings.Contains(err.Error(), "/nope is not in the project tree") || !strings.Contains(err.Error(), "api") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "amberignore") {
			t.Errorf("a path missing on disk blamed .amberignore: %v", err)
		}
	})

	t.Run("a source an .amberignore excludes", func(t *testing.T) {
		root := project(t)
		writeFile(t, root, ".amberignore", "/lib\n")
		l, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		s := spec.BuildSpec{Path: "/services/a", Sources: []string{"/lib"}, Platform: "linux/amd64"}
		_, err = l.SourceFor(ctx, root, s)
		if err == nil || !strings.Contains(err.Error(), "/lib is not in the project tree") || !strings.Contains(err.Error(), ".amberignore") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a build path that does not exist", func(t *testing.T) {
		root := project(t)
		l, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		s := spec.BuildSpec{Path: "/services/missing", Sources: []string{"/lib"}, Platform: "linux/amd64"}
		if _, err = l.SourceFor(ctx, root, s); err == nil || !strings.Contains(err.Error(), "/services/missing is not in the project tree") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSourceForIngestsTheRootOnce(t *testing.T) {
	ctx := context.Background()
	root := project(t)
	writeFile(t, root, "services/b/BUILD.jobs", "# recipe\n")
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	a, err := l.SourceFor(ctx, root, spec.BuildSpec{Path: "/services/a", Sources: []string{"/lib"}, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	// A file that appears after the first ingest is not seen by the second
	// spec: one run, one view of the project.
	writeFile(t, root, "lib/late.go", "package lib\n")
	b, err := l.SourceFor(ctx, root, spec.BuildSpec{Path: "/services/b", Sources: []string{"/lib"}, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.roots) != 1 {
		t.Fatalf("%d root ingests recorded, want 1", len(l.roots))
	}
	entries, err := l.store.Ls(ctx, b.key, "lib")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "x.go" {
		t.Errorf("the second spec saw a second ingest: lib holds %v", entries)
	}
	if a.String() == b.String() {
		t.Error("two build paths share a context")
	}
}
