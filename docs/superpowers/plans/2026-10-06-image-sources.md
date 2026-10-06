# `sources` on jobs-build images — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A jobs-build image object can list other project paths (`sources`) its build may reference; assimilate then builds from the project root pruned to those paths, with the service as the build directory.

**Architecture:** The template scanner decodes and normalizes `sources` into `spec.BuildSpec`. `jobs.Local.SourceFor` resolves a spec's source tree: the subtree ingest of today, or — with sources — one memoized ingest of the project root pruned with jobs-iroh's `PruneTree`. `definition` sets `Dir` and `Ctx` for such specs. The build runner and `render` call `SourceFor` and group specs by source identity instead of by path.

**Tech Stack:** Go 1.26, `gopkg.in/yaml.v3` nodes (existing scanner), jobs-iroh v0.35.1 (`amber.Store.PruneTree`, `builddef.Definition.Dir/Ctx`, `builddef.CtxWidened`).

**Spec:** `docs/superpowers/specs/2026-10-06-image-sources-design.md`

## Global Constraints

- A spec without `sources` must produce the same definition bytes and the same K as before: no `Dir`, no `Ctx`, subtree ingest.
- `BuildSpec.Key()` of a spec without sources is unchanged.
- Error texts start with `jobs-build image object:` in the scanner, as the existing ones do, and carry file and line.
- No new command, flag or config key.
- `gofmt -l .` prints nothing; `go vet ./...` and `go test ./...` pass (the CI's three steps).
- Remove every binary you build.

## Review Focus

1. **An entry spelled differently** (`proto`, `/proto/`, `./proto`, `proto//x/..` is rejected for its `..`) must normalize to one path — Task 2, `TestSourcesDecoding`.
2. **Two image objects with the same path, one with sources and one without** are two builds with two tags — Task 1 (`TestKeyWithSources`), Task 4 (`TestRunGroupsBySource`).
3. **A source that exists on disk but is excluded by an `.amberignore`** must fail with a message that says so — Task 3, `TestSourceForErrors`.
4. **A change outside the covered paths, or only an mtime inside them,** must not move K — Task 3, `TestSourceForKey`.
5. **A project with several images that use sources** must walk the project root once — Task 3 (`TestSourceForIngestsTheRootOnce`).

## File Structure

```
internal/spec/spec.go      BuildSpec.Sources; Key; SourceKey; Keep; NormalizeSources
internal/tmpl/tmpl.go      decode `sources` in decodeBuild; decodeSources
internal/jobs/jobs.go      Local.SourceFor, ingestRoot, inTree; definition sets Dir/Ctx
internal/builds/builds.go  Backend.SourceFor replaces Ingest; groups by SourceKey
cmd/assimilate/main.go     render resolves sources through SourceFor
README.md, docs/design.md  the key, what a tag depends on, the root .amberignore
```

---

### Task 1: spec — sources in the build spec

**Files:** Modify `internal/spec/spec.go`; test `internal/spec/spec_test.go`.

**Interfaces — produces:**
- `BuildSpec.Sources []string` — normalized (see `NormalizeSources`); nil = none.
- `NormalizeSources(path string, sources []string) []string` — input entries are cleaned `/`-rooted paths; output sorted, de-duplicated, without entries equal to or inside `path` or inside another entry; nil when nothing remains.
- `(BuildSpec).SourceKey() string` — identity of the source tree (path + sources).
- `(BuildSpec).Keep() []string` — project-relative paths (no leading slash) of the pruned context: the sources, plus the path unless a source contains it; sorted.
- `(BuildSpec).Key()` — additionally covers sources.

- [ ] **Step 1: failing tests** — append to `internal/spec/spec_test.go`:

```go
func TestNormalizeSources(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		sources []string
		want    []string
	}{
		{"none", "/services/a", nil, nil},
		{"sorted", "/services/a", []string{"/proto", "/lib"}, []string{"/lib", "/proto"}},
		{"duplicates", "/services/a", []string{"/proto", "/proto"}, []string{"/proto"}},
		{"the path itself", "/services/a", []string{"/services/a", "/proto"}, []string{"/proto"}},
		{"inside the path", "/services/a", []string{"/services/a/gen"}, nil},
		{"inside another source", "/services/a", []string{"/lib/x", "/lib"}, []string{"/lib"}},
		{"a sibling with a common prefix stays", "/services/a", []string{"/services/a-b", "/lib", "/lib-2"}, []string{"/lib", "/lib-2", "/services/a-b"}},
		{"a source containing the path stays", "/services/a", []string{"/services"}, []string{"/services"}},
	}
	for _, tt := range tests {
		if got := NormalizeSources(tt.path, tt.sources); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: NormalizeSources(%q, %v) = %v, want %v", tt.name, tt.path, tt.sources, got, tt.want)
		}
	}
}

func TestKeyWithSources(t *testing.T) {
	plain := BuildSpec{Path: "/services/a", Platform: "linux/amd64", Args: map[string]string{"k": "v"}}
	// The key of a spec without sources is the encoding it always had.
	if got, want := plain.Key(), "11:/services/a0:11:linux/amd641:k1:v"; got != want {
		t.Errorf("Key without sources = %q, want %q", got, want)
	}
	withSources := plain
	withSources.Sources = []string{"/proto"}
	other := plain
	other.Sources = []string{"/lib"}
	if withSources.Key() == plain.Key() || withSources.Key() == other.Key() {
		t.Errorf("keys collide: %q %q %q", plain.Key(), withSources.Key(), other.Key())
	}
	// A sources list must not read as args.
	asArgs := BuildSpec{Path: "/services/a", Platform: "linux/amd64", Args: map[string]string{"k": "v", "/proto": "/lib"}}
	twoSources := plain
	twoSources.Sources = []string{"/lib", "/proto"}
	if asArgs.Key() == twoSources.Key() {
		t.Errorf("args and sources encode alike: %q", asArgs.Key())
	}
}

func TestSourceKeyAndKeep(t *testing.T) {
	a := BuildSpec{Path: "/services/a", Platform: "linux/amd64", Sources: []string{"/lib", "/proto"}}
	sameSource := BuildSpec{Path: "/services/a", Platform: "linux/arm64", BuildFile: "BUILD.prod", Sources: []string{"/lib", "/proto"}}
	if a.SourceKey() != sameSource.SourceKey() {
		t.Error("specs that differ only in platform and recipe must share a source")
	}
	for name, other := range map[string]BuildSpec{
		"no sources":    {Path: "/services/a"},
		"other sources": {Path: "/services/a", Sources: []string{"/lib"}},
		"other path":    {Path: "/services/b", Sources: []string{"/lib", "/proto"}},
	} {
		if other.SourceKey() == a.SourceKey() {
			t.Errorf("%s: SourceKey collides", name)
		}
	}
	if got, want := a.Keep(), []string{"lib", "proto", "services/a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Keep = %v, want %v", got, want)
	}
	// A source that contains the path covers it.
	wide := BuildSpec{Path: "/services/a", Sources: []string{"/proto", "/services"}}
	if got, want := wide.Keep(), []string{"proto", "services"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Keep = %v, want %v", got, want)
	}
}
```

Add `"reflect"` to the test file's imports if it is not there.

- [ ] **Step 2:** `go test ./internal/spec/` → FAIL (`undefined: NormalizeSources`, unknown field `Sources`).

- [ ] **Step 3: implement** in `internal/spec/spec.go`:

Field, after `Path`:

```go
	// Sources are the other project paths the build may reference ("/"-rooted,
	// as normalized by NormalizeSources; nil = none). With sources the build's
	// source is the project root pruned to Path and Sources, and Path becomes
	// the build directory inside it.
	Sources []string
```

In `Key`, before `return b.String()`:

```go
	// Sources follow a marker that no length-prefixed component can start
	// with, so a sources list never reads as args, and a spec without
	// sources keeps the key it always had.
	if len(s.Sources) > 0 {
		b.WriteString("|sources")
		for _, p := range s.Sources {
			comp(p)
		}
	}
```

New functions:

```go
// SourceKey identifies the source tree a spec builds from — its path and
// sources. Specs with equal SourceKeys share one ingest and one push.
func (s BuildSpec) SourceKey() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d:%s", len(s.Path), s.Path)
	for _, p := range s.Sources {
		fmt.Fprintf(&b, "%d:%s", len(p), p)
	}
	return b.String()
}

// Keep lists the project-relative paths (no leading slash, sorted) that make
// up the source tree of a spec with sources: the sources, plus the path
// unless a source contains it.
func (s BuildSpec) Keep() []string {
	keep := make([]string, 0, len(s.Sources)+1)
	covered := false
	for _, p := range s.Sources {
		if within(s.Path, p) {
			covered = true
		}
		keep = append(keep, strings.TrimPrefix(p, "/"))
	}
	if !covered {
		keep = append(keep, strings.TrimPrefix(s.Path, "/"))
	}
	sort.Strings(keep)
	return keep
}

// NormalizeSources returns the canonical sources of a spec at path. Entries
// are cleaned "/"-rooted paths; the result is sorted, without duplicates,
// without entries equal to or inside path (the build has those anyway) and
// without entries inside another entry. nil when nothing remains.
func NormalizeSources(path string, sources []string) []string {
	sorted := append([]string(nil), sources...)
	sort.Strings(sorted)
	var out []string
	for _, p := range sorted {
		if within(p, path) {
			continue
		}
		nested := false
		for _, kept := range out {
			if within(p, kept) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, p)
		}
	}
	return out
}

// within reports whether the "/"-rooted path p is dir or lies inside it.
func within(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}
```

- [ ] **Step 4:** `go test ./internal/spec/` → PASS.
- [ ] **Step 5:** commit `spec: sources in the build spec`.

---

### Task 2: tmpl — decode `sources`

**Files:** Modify `internal/tmpl/tmpl.go`; test `internal/tmpl/tmpl_test.go`.

**Interfaces — consumes:** `spec.NormalizeSources`, `BuildSpec.Sources`. **Produces:** `Scan` returns specs with `Sources` set.

- [ ] **Step 1: failing test** — append to `internal/tmpl/tmpl_test.go`:

```go
func TestSourcesDecoding(t *testing.T) {
	image := func(body string) string {
		return "spec:\n  containers:\n    - name: api\n      image:\n        type: jobs-build\n        platform: linux/amd64\n" + body
	}
	scanOne := func(t *testing.T, content string) (spec.BuildSpec, error) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		x, err := Scan(dir)
		if err != nil {
			return spec.BuildSpec{}, err
		}
		if len(x.Builds) != 1 {
			t.Fatalf("%d builds, want 1", len(x.Builds))
		}
		return x.Builds[0], nil
	}

	t.Run("normalized", func(t *testing.T) {
		got, err := scanOne(t, image("        path: services/api\n        sources:\n          - proto\n          - /lib/\n          - ./proto\n          - /lib/sub\n          - services/api/gen\n"))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"/lib", "/proto"}; !reflect.DeepEqual(got.Sources, want) {
			t.Errorf("Sources = %v, want %v", got.Sources, want)
		}
	})
	t.Run("absent and empty mean none", func(t *testing.T) {
		for _, body := range []string{"        path: /services/api\n", "        path: /services/api\n        sources: []\n"} {
			got, err := scanOne(t, image(body))
			if err != nil || got.Sources != nil {
				t.Errorf("Sources = %v, %v; want nil", got.Sources, err)
			}
		}
	})
	t.Run("flow style and key order", func(t *testing.T) {
		got, err := scanOne(t, image("        sources: [/proto]\n        path: /services/api\n"))
		if err != nil || !reflect.DeepEqual(got.Sources, []string{"/proto"}) {
			t.Errorf("Sources = %v, %v", got.Sources, err)
		}
	})

	for name, tt := range map[string]struct{ body, want string }{
		"not a list":       {"        path: /services/api\n        sources: /proto\n", "sources must be a list of paths"},
		"a mapping entry":  {"        path: /services/api\n        sources:\n          - a: b\n", "sources entries must be non-empty strings"},
		"an empty entry":   {"        path: /services/api\n        sources:\n          - \"\"\n", "sources entries must be non-empty strings"},
		"dot-dot":          {"        path: /services/api\n        sources:\n          - ../other\n", `sources must not contain ".."`},
		"the project root": {"        path: /services/api\n        sources:\n          - /\n", "sources must not name the project root"},
		"on a root build":  {"        sources:\n          - /proto\n", "sources is not allowed on a root build"},
		"root build path":  {"        path: /\n        sources: [proto]\n", "sources is not allowed on a root build"},
		"duplicate key":    {"        path: /services/api\n        sources: [/a]\n        sources: [/b]\n", `duplicate key "sources"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := scanOne(t, image(tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "a.yaml:") {
				t.Errorf("err = %v, want one naming the file and %q", err, tt.want)
			}
		})
	}
}
```

Use the test file's existing imports; add any of `os`, `path/filepath`, `reflect`, `strings`, `spec` that are missing. If the scanner's entry point or its result field is named differently from `Scan` / `Builds`, use the existing names (see the top of `tmpl.go`).

- [ ] **Step 2:** `go test ./internal/tmpl/ -run TestSourcesDecoding` → FAIL (`unknown key "sources"`).

- [ ] **Step 3: implement** in `decodeBuild`:

Declare beside `platformLine`:

```go
	var sources []string
	sourcesLine := 0
```

New case, before `default`:

```go
		case "sources":
			sources, err = decodeSources(file, v)
			sourcesLine = k.Line
```

Replace the tail `s.Path = normalizePath(s.Path); return s, nil` with:

```go
	s.Path = normalizePath(s.Path)
	if len(sources) > 0 && s.Path == "/" {
		return s, nodeErrf(file, sourcesLine, "jobs-build image object: sources is not allowed on a root build (path %q already covers the project)", "/")
	}
	s.Sources = spec.NormalizeSources(s.Path, sources)
	return s, nil
```

New function after `decodeBuild`:

```go
// decodeSources requires v to be a sequence of non-empty scalar paths without
// ".." that do not name the project root, and returns them cleaned and
// "/"-prefixed (not yet normalized against the build path).
func decodeSources(file string, v *yaml.Node) ([]string, error) {
	if v.Kind != yaml.SequenceNode {
		return nil, nodeErrf(file, v.Line, "jobs-build image object: sources must be a list of paths")
	}
	out := make([]string, 0, len(v.Content))
	for _, item := range v.Content {
		if item.Kind != yaml.ScalarNode || item.Value == "" {
			return nil, nodeErrf(file, item.Line, "jobs-build image object: sources entries must be non-empty strings")
		}
		if hasDotDot(item.Value) {
			return nil, nodeErrf(file, item.Line, "jobs-build image object: sources must not contain %q", "..")
		}
		p := normalizePath(item.Value)
		if p == "/" {
			return nil, nodeErrf(file, item.Line, "jobs-build image object: sources must not name the project root")
		}
		out = append(out, p)
	}
	return out, nil
}
```

Update `decodeBuild`'s doc comment and the package comment's field list: allowed keys are `{type, name, path, sources, build-file, args, platform}`.

- [ ] **Step 4:** `go test ./internal/tmpl/` → PASS.
- [ ] **Step 5:** commit `tmpl: decode sources on jobs-build image objects`.

---

### Task 3: jobs — the pruned project tree and the widened definition

**Files:** Modify `internal/jobs/jobs.go`; test `internal/jobs/jobs_test.go`.

**Interfaces — consumes:** `BuildSpec.Sources`, `Keep`, `spec.SourceDir`. **Produces:** `(*Local).SourceFor(ctx context.Context, root string, s spec.BuildSpec) (Source, error)`; `definition` sets `Dir` and `Ctx` for specs with sources. `*Client` gets `SourceFor` through its embedded `*Local`.

- [ ] **Step 1: failing tests** — append to `internal/jobs/jobs_test.go`:

```go
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
```

Add the imports the new tests need (`os`, `path/filepath`, `reflect`, `strings`, `time`, `builddef`) where missing.

- [ ] **Step 2:** `go test ./internal/jobs/` → FAIL (`l.SourceFor undefined`, `l.roots undefined`).

- [ ] **Step 3: implement** in `internal/jobs/jobs.go`:

Fields on `Local`:

```go
	// roots memoizes the project-root ingest for specs with sources: one
	// run sees one view of the project, however many images share it.
	rootMu sync.Mutex
	roots  map[string]key.Key
```

After `Ingest`:

```go
// SourceFor returns the source tree s builds from under the project root:
// the ingested subtree at s.Path, or — when s declares sources — the project
// root pruned to s.Path and s.Sources, in the project's layout and with
// normalized metadata (jobs-iroh's covered tree). Every path must be in the
// ingested project tree; one that is missing is an error naming it.
func (l *Local) SourceFor(ctx context.Context, root string, s spec.BuildSpec) (Source, error) {
	if len(s.Sources) == 0 {
		return l.Ingest(ctx, spec.SourceDir(root, s.Path))
	}
	rootKey, err := l.ingestRoot(ctx, root)
	if err != nil {
		return Source{}, err
	}
	for _, p := range append([]string{s.Path}, s.Sources...) {
		ok, err := l.inTree(ctx, rootKey, p)
		if err != nil {
			return Source{}, fmt.Errorf("image %s: resolve %s in the project tree: %w", s.DisplayName(), p, err)
		}
		if !ok {
			hint := ""
			if _, statErr := os.Lstat(spec.SourceDir(root, p)); statErr == nil {
				hint = " (it exists on disk: an .amberignore excludes it)"
			}
			return Source{}, fmt.Errorf("image %s: %s is not in the project tree%s", s.DisplayName(), p, hint)
		}
	}
	k, err := l.store.PruneTree(ctx, rootKey, s.Keep())
	if err != nil {
		return Source{}, fmt.Errorf("image %s: prune the project tree: %w", s.DisplayName(), err)
	}
	return Source{key: k, keyStr: k.String(), valid: true}, nil
}

// ingestRoot ingests the project root (honoring .amberignore files; .git is
// never ingested), once per Local.
func (l *Local) ingestRoot(ctx context.Context, root string) (key.Key, error) {
	l.rootMu.Lock()
	defer l.rootMu.Unlock()
	if k, ok := l.roots[root]; ok {
		return k, nil
	}
	k, err := l.store.IngestSourceDir(ctx, root)
	if err != nil {
		return key.Key{}, fmt.Errorf("ingest project root %s: %w", root, err)
	}
	if l.roots == nil {
		l.roots = map[string]key.Key{}
	}
	l.roots[root] = k
	return k, nil
}

// inTree reports whether the "/"-rooted path p names an entry of the tree.
func (l *Local) inTree(ctx context.Context, root key.Key, p string) (bool, error) {
	dir := ""
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		entries, err := l.store.Ls(ctx, root, dir)
		if err != nil {
			return false, err
		}
		found := false
		for _, e := range entries {
			if e.Name == seg {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
		dir = path.Join(dir, seg)
	}
	return true, nil
}
```

In `definition`, after `def := builddef.Definition{…}` and before `def.Canonical()`:

```go
	if len(s.Sources) > 0 {
		// The source is the project pruned to the spec's paths, so the
		// build root is the service's directory inside it: a widened-context
		// definition, exactly what jobs-iroh's own client builds for a
		// subdirectory (clientcli's treeDefinition).
		def.Dir = strings.TrimPrefix(s.Path, "/")
		if def.Dir == "" {
			return nil, key.Key{}, errors.New("jobs: sources on a root build")
		}
		def.Ctx = builddef.CtxWidened
	}
```

Rewrite `definition`'s comment: the ingested subtree is the build root and `Dir` stays empty *unless the spec has sources*. Add the imports `path`, `strings`, `sync`.

- [ ] **Step 4:** `go test ./internal/jobs/` → PASS.
- [ ] **Step 5:** commit `jobs: source tree and widened definition for specs with sources`.

---

### Task 4: builds and render — resolve sources through SourceFor

**Files:** Modify `internal/builds/builds.go`, `internal/builds/builds_test.go`, `cmd/assimilate/main.go`.

**Interfaces — consumes:** `Local.SourceFor`, `BuildSpec.SourceKey`. **Produces:** `builds.Backend.SourceFor(ctx, root string, s spec.BuildSpec) (jobs.Source, error)` replaces `Backend.Ingest`.

- [ ] **Step 1: failing test** — in `internal/builds/builds_test.go`, rename the fake's `Ingest` method to `SourceFor(ctx context.Context, root string, s spec.BuildSpec) (jobs.Source, error)`; its body starts with `dir := spec.SourceDir(root, s.Path)` and additionally records `f.sources = append(f.sources, s.SourceKey())` (new field `sources []string`). Append:

```go
func TestRunGroupsBySource(t *testing.T) {
	f := &fake{}
	specs := []spec.BuildSpec{
		{Name: "plain", Path: "/services/a", Platform: "linux/amd64"},
		{Name: "with-proto", Path: "/services/a", Sources: []string{"/proto"}, Platform: "linux/amd64"},
		{Name: "with-proto-arm", Path: "/services/a", Sources: []string{"/proto"}, Platform: "linux/arm64"},
		{Name: "plain-arm", Path: "/services/a", Platform: "linux/arm64"},
	}
	events := make(chan spec.Event, 1024)
	results, err := Run(context.Background(), "/root", "reg", specs, f, events)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.State != spec.StateDone {
			t.Errorf("%s: %s %s", r.Spec.Name, r.State, r.Err)
		}
	}
	// One source per (path, sources): the same path with and without
	// sources is two trees; platforms share one.
	want := []string{specs[0].SourceKey(), specs[1].SourceKey()}
	if !reflect.DeepEqual(f.sources, want) {
		t.Errorf("sources resolved: %v, want %v", f.sources, want)
	}
	if len(f.submits) != 4 {
		t.Errorf("%d submits, want 4", len(f.submits))
	}
}
```

If the existing tests drain events differently (an unbuffered channel with a reader goroutine), follow their helper instead of the buffered channel.

- [ ] **Step 2:** `go test ./internal/builds/` → FAIL (`*fake does not implement Backend (missing method Ingest)`, then the grouping assertion).

- [ ] **Step 3: implement.**

`internal/builds/builds.go` — interface method:

```go
	// SourceFor resolves the source tree a spec builds from under the
	// project root (jobs.Local.SourceFor).
	SourceFor(ctx context.Context, root string, s spec.BuildSpec) (jobs.Source, error)
```

In `Run`, the grouping and its use:

```go
	// One group per unique source tree — a path, or a path and its sources —
	// in first-appearance order; a group's builds all submit as soon as its
	// push lands.
	type group struct {
		spec spec.BuildSpec // the first spec: carries the source identity
		idxs []int
	}
	var groups []*group
	bySource := map[string]*group{}
	for i, s := range specs {
		g := bySource[s.SourceKey()]
		if g == nil {
			g = &group{spec: s}
			bySource[s.SourceKey()] = g
			groups = append(groups, g)
		}
		g.idxs = append(g.idxs, i)
	}
```

and in the loop `src, err := b.SourceFor(ctx, root, g.spec)`, with `g.spec.Path` wherever `g.path` was used in messages. Update `Run`'s doc comment (groups by source tree).

`cmd/assimilate/main.go` — in `render`:

```go
		srcs := map[string]jobs.Source{}
		for _, s := range x.Builds {
			src, ok := srcs[s.SourceKey()]
			if !ok {
				if src, err = local.SourceFor(c.Context, root, s); err != nil {
					return fmt.Errorf("ingest %s: %w", s.Path, err)
				}
				srcs[s.SourceKey()] = src
			}
```

- [ ] **Step 4:** `gofmt -l . ; go vet ./... && go test ./...` → all PASS, `gofmt` prints nothing.
- [ ] **Step 5:** commit `builds, render: resolve each spec's source tree, grouped by source`.

---

### Task 5: documentation and a run against a real project

**Files:** Modify `README.md`, `docs/design.md`.

- [ ] **Step 1: README.** In the template section add `sources` to the example image object (`sources: [/proto]  # optional; other project paths the build may reference`) and, after the paragraph on K and tags, a subsection "Sharing source between images" that states: what `sources` is and how entries are written and normalized; that the build then runs in the project root pruned to `path` and `sources`, with `path` as the build directory, so relative references between them (a Go `replace ../../lib`, a cargo path dependency) resolve; the tag table from the spec (what K depends on, with and without sources), including that such tags do not depend on mtimes; that a path the recipe needs but the template does not list fails the build and names the path; that the project root is walked once per run and wants a root `.amberignore`; the server requirement (jobs-iroh v0.11.0, v0.12.0 for `closure=`). Fix the sentence on `.amberignore` files ("one at the monorepo root is not consulted for subtree builds") to say it *is* consulted for images with `sources`.
- [ ] **Step 2: design.md.** Field list of the image object; the build flow's step 1 (two cases); the sentence "Subtree ingest is safe because recipes cannot reference anything outside their build root" (true without `sources`); the definition in step 2 (`Dir`, `Ctx` with sources).
- [ ] **Step 3: run it against validator-watch** (`/Users/dragan/numtide/validator-watch`, read-only use of that checkout): build the binary into the session scratchpad; with a scratch `ASSIMILATE_DATA_DIR`, `render hoodi` must print the same tags as the unmodified v0.7.5 binary for that tree (no template there uses `sources` yet). Then, in a scratch copy of the templates is not possible (the project root is found from the working directory) — so measure only: time of `render hoodi` before and after adding `sources: [/proto]` to one image object in a throwaway git worktree of validator-watch, and that the rendered tag of that image differs while every other tag is unchanged. Remove the worktree and the binary afterwards. Record the timings in the pull request description.
- [ ] **Step 4:** `gofmt -l . ; go vet ./... && go test ./...`; commit `docs: sources on jobs-build images`.
