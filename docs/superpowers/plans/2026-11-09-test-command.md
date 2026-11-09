# `assimilate test` — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `assimilate test [environment]` finds every `TESTS.jobs` in the monorepo, submits each as a jobs-iroh test with the right source tree, platform and params, follows them in the existing TUI, and lets the server's `test-passed` memo decide which ones actually run.

**Architecture:** `spec.BuildSpec` grows two flags, `Test` and `WholeTree`, so a test is just a spec the existing ingest/push/submit/follow machinery already handles; `jobs.definition` turns them into jobs-iroh's `Definition.Test`, `Dir` and `Ctx`. A new `internal/tests` package discovers the set: template image paths that hold a `TESTS.jobs` (platform and args from the image) plus every other `TESTS.jobs` in the ingested project tree (platforms from an `assimilate-tests.yaml` beside it). `builds.Run` runs them unchanged and additionally records memo hits; `cmd/assimilate` adds the `test` command, `--list` and the summary.

**Tech Stack:** Go 1.26, jobs-iroh v0.36.0 (`builddef.Definition.Test`, `amber.Store.Ls`, `api.Snapshot`, `wire.KindTestValue/KindTestRun`), `gopkg.in/yaml.v3` (strict decoding as in `project.LoadConfig`), urfave/cli v2.

**Spec:** `docs/superpowers/specs/2026-11-09-test-command-design.md`

## Global Constraints

- A spec with `Test == false && WholeTree == false` must produce byte-identical definition bytes, the same K, the same `Key()` and the same `SourceKey()` as before. `deploy` and `render` behavior is unchanged.
- A test definition has `Test: true`, `BuildFile: ""` (the recipe is `TESTS.jobs`; jobs-iroh reads `BuildFile` as the test recipe path too), and for a template test exactly the image's `Source`, `Dir`, `Ctx`, `Platform` and `Params`.
- Test identity is (path, sources or whole-tree mode, platform, args); `build-file` never enters it.
- The sidecar is `assimilate-tests.yaml`, one key `platforms`, a non-empty list of non-empty strings, strict (`KnownFields(true)`); errors carry the file path.
- A project-tree `TESTS.jobs` with neither an in-scope image at its path nor a sidecar is a preflight error, before any dial or push; nothing is inferred.
- Where a directory is both an image path in scope and sidecar-declared, the template wins and the sidecar is ignored (reported in `--list`).
- Discovery walks the ingested project tree (`.amberignore` honored), never the filesystem or git.
- `assimilate test` never loads `assimilate.yaml`. `JOBS_SERVER` is required unless `--list`.
- Paths shown to the user are `/`-rooted project paths (`/lib/proto`), as elsewhere.
- `gofmt -l .` prints nothing; `go build ./...`, `go vet ./...` and `go test -race ./...` pass (CI's steps).
- Remove every binary you build.

## Review Focus

1. **A root `TESTS.jobs`** (beside `assimilate-templates/`) with no root image and no sidecar must fail naming `/`, and with a sidecar must run as a root test (no `Dir`, no `Ctx`) — Task 3 `TestDiscoverRootTest`, Task 2 `TestWholeTreeTestRootHasNoDir`.
2. **A directory that is an image in staging only**, run as `assimilate test prod` and as `assimilate test`, must come out as a sidecar test in the first and a template test in the second — Task 3 `TestDiscoverTemplateWinsPerScope`.
3. **A sidecar that repeats a platform** must yield one test, not two rows with one label — Task 3 `TestReadSidecar`/`TestDiscoverSidecarDuplicates`.
4. **An image whose path is not in the ingested tree** (an `.amberignore` excludes it) must land in the untested list, not crash or become a sidecar test — Task 3 `TestDiscoverUntestedImages`.
5. **A cancelled run** must print a summary that counts cancellations and exit non-zero — Task 5 `TestTestSummary`.

## File Structure

```
internal/spec/spec.go          BuildSpec.Test, BuildSpec.WholeTree; Key; SourceKey; TestOf; WholeTreeTest
internal/jobs/jobs.go          definition: Test, WholeTree → Dir/Ctx; SourceFor: whole tree; Local.TestDirs
internal/tests/tests.go        sidecar reading, Discover (union, dedup, labels, untested), Origin
internal/builds/builds.go      Result.Cached from the terminal snapshot
internal/project/project.go    Envs(root)
cmd/assimilate/main.go         test command: scope, discovery, --list, run, summary; reorderArgs; shared showRun
README.md, docs/design.md      Tests section, layout, packages
```

---

### Task 1: spec — tests in the build spec

**Files:** Modify `internal/spec/spec.go`; test `internal/spec/spec_test.go`.

**Interfaces — produces:**
- `BuildSpec.Test bool` — a `TESTS.jobs` test of `Path` rather than its build.
- `BuildSpec.WholeTree bool` — the source is the whole project tree and `Path` is the directory inside it (project-tree tests below the root). Never set together with `Sources`, never on `Path == "/"`.
- `(BuildSpec).TestOf() BuildSpec` — a copy with `Test: true`, `BuildFile: ""`, `Name: ""`; everything else kept.
- `WholeTreeTest(path, platform string) BuildSpec` — `{Path: path, Platform: platform, Test: true, WholeTree: path != "/"}`.
- `(BuildSpec).Key()` — additionally covers `Test` and `WholeTree`; unchanged when both are false.
- `(BuildSpec).SourceKey()` — `"wholetree"` for every `WholeTree` spec (they all share the root tree); unchanged otherwise.

- [ ] **Step 1: failing tests** — append to `internal/spec/spec_test.go`:

```go
func TestKeyTestFlags(t *testing.T) {
	img := BuildSpec{Path: "/svc", Platform: "linux/amd64", BuildFile: "BUILD.prod", Args: map[string]string{"v": "1"}}
	if img.Key() != (BuildSpec{Path: "/svc", Platform: "linux/amd64", BuildFile: "BUILD.prod", Args: map[string]string{"v": "1"}}).Key() {
		t.Fatal("key of an unchanged spec moved")
	}
	tst := img.TestOf()
	if !tst.Test || tst.BuildFile != "" || tst.Name != "" || tst.Platform != img.Platform || tst.Args["v"] != "1" {
		t.Fatalf("TestOf = %+v", tst)
	}
	if tst.Key() == img.Key() {
		t.Fatal("a test keys like its build")
	}
	// build-file never enters a test's identity
	if (BuildSpec{Path: "/svc", Platform: "linux/amd64", Args: map[string]string{"v": "1"}}).TestOf().Key() != tst.Key() {
		t.Fatal("build-file leaked into the test key")
	}
	wt := WholeTreeTest("/lib", "linux/amd64")
	if !wt.Test || !wt.WholeTree || wt.Path != "/lib" {
		t.Fatalf("WholeTreeTest = %+v", wt)
	}
	if wt.Key() == (BuildSpec{Path: "/lib", Platform: "linux/amd64", Test: true}).Key() {
		t.Fatal("whole-tree mode not in the key")
	}
	if root := WholeTreeTest("/", "linux/amd64"); root.WholeTree {
		t.Fatal("a root test is not whole-tree")
	}
}

func TestSourceKeyWholeTree(t *testing.T) {
	a, b := WholeTreeTest("/lib", "linux/amd64"), WholeTreeTest("/svc", "linux/arm64")
	if a.SourceKey() != b.SourceKey() {
		t.Fatal("whole-tree specs must share one source")
	}
	if a.SourceKey() == (BuildSpec{Path: "/lib", Platform: "linux/amd64"}).SourceKey() {
		t.Fatal("whole-tree source key collides with the subtree's")
	}
}
```

- [ ] **Step 2: run** `go test ./internal/spec/ -run 'TestKeyTestFlags|TestSourceKeyWholeTree'` — FAIL (unknown fields).
- [ ] **Step 3: implement** the fields, `TestOf`, `WholeTreeTest`; in `Key()` append `"|test"` when `Test` and `"|wholetree"` when `WholeTree` after the sources block (markers no length-prefixed component can produce); in `SourceKey()` return `"wholetree"` first when `WholeTree`. Document both fields on the struct.
- [ ] **Step 4: run** `go test ./internal/spec/` — PASS.
- [ ] **Step 5: commit** `spec: test and whole-tree flags on BuildSpec`.

---

### Task 2: jobs — test definitions, the whole-tree source, TESTS.jobs discovery

**Files:** Modify `internal/jobs/jobs.go`; test `internal/jobs/jobs_test.go` (reuse `project(t)`, `writeFile`, `keyOf`).

**Interfaces — consumes:** Task 1's fields.
**Interfaces — produces:**
- `definition(src, s)` sets `def.Test = s.Test`; for `s.WholeTree`: `def.Dir = strings.TrimPrefix(s.Path, "/")`, `def.Ctx = builddef.CtxWidened`, error `jobs: whole-tree context on a root spec` when `Dir` would be empty. `Sources` handling unchanged.
- `(*Local).SourceFor` — for `s.WholeTree`: the memoized root ingest (`ingestRoot`), unpruned, after checking `s.Path` is a directory in the tree (same error texts as the sources path, prefixed `test <DisplayName>:` instead of `image …:`).
- `(*Local).TestDirs(ctx context.Context, root string) ([]string, error)` — `/`-rooted slash paths of every directory in the ingested project tree holding a regular file named `TESTS.jobs` (`mode&unix.S_IFMT == unix.S_IFREG`), sorted; `"/"` for the root itself. Walks with `l.store.Ls` from `ingestRoot`.

- [ ] **Step 1: failing tests** — append to `internal/jobs/jobs_test.go`:

```go
// decodeDef decodes a spec's canonical definition over root.
func decodeDef(t *testing.T, root string, s spec.BuildSpec) builddef.Definition {
	t.Helper()
	l, err := Open(t.TempDir()); if err != nil { t.Fatal(err) }
	defer l.Close()
	src, err := l.SourceFor(context.Background(), root, s); if err != nil { t.Fatal(err) }
	canon, _, err := definition(src, s); if err != nil { t.Fatal(err) }
	d, err := builddef.DecodeDefinition(canon); if err != nil { t.Fatal(err) }
	return d
}

func TestTestDefinitionMirrorsTheImage(t *testing.T) {
	root := project(t)
	for _, img := range []spec.BuildSpec{
		{Path: "/services/a", Platform: "linux/amd64", BuildFile: "BUILD.prod", Args: map[string]string{"variant": "slim"}},
		{Path: "/services/a", Sources: []string{"/lib"}, Platform: "linux/arm64"},
	} {
		b, tst := decodeDef(t, root, img), decodeDef(t, root, img.TestOf())
		if !tst.Test || tst.BuildFile != "" { t.Fatalf("test def = %+v", tst) }
		if !bytes.Equal(tst.Source.Definition, b.Source.Definition) || tst.Dir != b.Dir || tst.Ctx != b.Ctx ||
			tst.Platform != b.Platform || !bytes.Equal(tst.Params, b.Params) {
			t.Fatalf("test def %+v does not mirror build def %+v", tst, b)
		}
		if keyOf(t, root, img) == keyOf(t, root, img.TestOf()) { t.Fatal("test K equals build K") }
	}
}

func TestWholeTreeTestDefinition(t *testing.T) {
	ctx, root := context.Background(), project(t)
	l, err := Open(t.TempDir()); if err != nil { t.Fatal(err) }
	defer l.Close()
	s := spec.WholeTreeTest("/lib", "linux/amd64")
	src, err := l.SourceFor(ctx, root, s); if err != nil { t.Fatal(err) }
	whole, err := l.Ingest(ctx, root); if err != nil { t.Fatal(err) }
	if src.String() != whole.String() { t.Fatalf("source %s, want the unpruned root %s", src, whole) }
	d := decodeDef(t, root, s)
	if d.Dir != "lib" || d.Ctx != builddef.CtxWidened || !d.Test { t.Fatalf("def = %+v", d) }
	if _, err := l.SourceFor(ctx, root, spec.WholeTreeTest("/nope", "linux/amd64")); err == nil || !strings.Contains(err.Error(), "/nope is not in the project tree") {
		t.Fatalf("missing dir: %v", err)
	}
}

func TestWholeTreeTestRootHasNoDir(t *testing.T) {
	root := project(t)
	d := decodeDef(t, root, spec.WholeTreeTest("/", "linux/amd64"))
	if d.Dir != "" || d.Ctx != 0 || !d.Test { t.Fatalf("root test def = %+v", d) }
}

func TestTestDirs(t *testing.T) {
	ctx, root := context.Background(), project(t)
	writeFile(t, root, "services/a/TESTS.jobs", "def test(): pass\n")
	writeFile(t, root, "lib/TESTS.jobs", "def test(): pass\n")
	writeFile(t, root, "ignored/TESTS.jobs", "def test(): pass\n")
	writeFile(t, root, ".amberignore", "/ignored\n") // the syntax TestSourceForErrors already relies on
	if err := os.MkdirAll(filepath.Join(root, "other", "TESTS.jobs"), 0o755); err != nil { t.Fatal(err) } // a directory, not a test
	l, err := Open(t.TempDir()); if err != nil { t.Fatal(err) }
	defer l.Close()
	dirs, err := l.TestDirs(ctx, root); if err != nil { t.Fatal(err) }
	if want := []string{"/lib", "/services/a"}; !reflect.DeepEqual(dirs, want) { t.Fatalf("TestDirs = %v, want %v", dirs, want) }
	writeFile(t, root, "TESTS.jobs", "def test(): pass\n")
	l2, err := Open(t.TempDir()); if err != nil { t.Fatal(err) }
	defer l2.Close()
	dirs, err = l2.TestDirs(ctx, root); if err != nil { t.Fatal(err) }
	if want := []string{"/", "/lib", "/services/a"}; !reflect.DeepEqual(dirs, want) { t.Fatalf("TestDirs with root = %v, want %v", dirs, want) }
}
```

- [ ] **Step 2: run** `go test ./internal/jobs/ -run 'TestTestDefinition|TestWholeTree|TestTestDirs'` — FAIL.
- [ ] **Step 3: implement** `definition`, `SourceFor`'s whole-tree branch, and `TestDirs` (recursive `Ls` walk; descend into `S_IFDIR` entries only; collect when an entry named `builddef.TestsFile` is `S_IFREG`; sort).
- [ ] **Step 4: run** `go test -race ./internal/jobs/` — PASS, including the existing `TestSourceFor*` tests.
- [ ] **Step 5: commit** `jobs: test definitions, whole-tree sources, TESTS.jobs discovery`.

---

### Task 3: tests — the sidecar and the discovered set

**Files:** Create `internal/tests/tests.go`, `internal/tests/tests_test.go`.

**Interfaces — consumes:** `spec.BuildSpec.TestOf`, `spec.WholeTreeTest` (Task 1); a `Tree` the caller supplies (`*jobs.Local` satisfies it via Task 2's `TestDirs`).
**Interfaces — produces:**

```go
package tests

const SidecarFile = "assimilate-tests.yaml"

// Env is one environment's scanned image specs, in appearance order.
type Env struct { Name string; Builds []spec.BuildSpec }

// Tree lists the TESTS.jobs directories of the ingested project tree.
type Tree interface { TestDirs(ctx context.Context, root string) ([]string, error) }

type Test struct {
	Spec           spec.BuildSpec // Test true; Name is the unique row label
	Image          string         // template test: the image's display name; "" for a sidecar test
	Envs           []string       // template test: environments the image appears in, scope order
	SidecarIgnored bool           // template test: a sidecar exists in the directory but the template won
}

type Set struct {
	Tests    []Test           // template tests in scope order, then sidecar tests by path, platforms in declared order
	Untested []spec.BuildSpec // in-scope images whose path has no TESTS.jobs; unique by Path, first appearance
}

// ReadSidecar returns the platforms declared in file (strict schema).
func ReadSidecar(file string) ([]string, error)

// Discover builds the Set for envs over the project at root.
func Discover(ctx context.Context, root string, envs []Env, tree Tree) (*Set, error)

// Origin describes where a test's platform came from, for --list.
func (t Test) Origin() string
```

Rules to implement:
- Template tests: for each env, each build whose `Path` is in `tree.TestDirs`: `t := b.TestOf()`; dedup by `t.Key()` keeping first; append env name to `Envs` (once per env). `Image = b.DisplayName()`. `SidecarIgnored` when `<root>/<path>/assimilate-tests.yaml` exists.
- Untested: builds whose `Path` is not in `TestDirs`, unique by `Path`.
- Sidecar tests: each dir from `TestDirs` that is not an in-scope image path: `ReadSidecar(<root>/<dir>/assimilate-tests.yaml)`; missing file → error
  `"%s/TESTS.jobs: no platforms declared; add %s/assimilate-tests.yaml with:\n  platforms:\n    - linux/amd64"` (both `%s` the `/`-rooted dir, `"/"` rendered as `""` so the root reads `/TESTS.jobs` and `/assimilate-tests.yaml`); one `spec.WholeTreeTest(dir, p)` per distinct platform, in declared order.
- `ReadSidecar` errors: `<file>: platforms is required and must be a non-empty list`, `<file>: platforms[%d] must be a non-empty string`, yaml/unknown-key errors as `<file>: <yaml error>`.
- Labels: default `Image` for template tests, `<path> <platform>` for sidecar tests. Any label shared by two or more tests is replaced, for each of them, by `<path> <platform>` + `" k=v"` per sorted arg + `" sources=/a,/b"` when `Sources` is non-empty. Set `Spec.Name` to the final label.
- `Origin()`: `"image <Image> (<env1>, <env2>)"`, with `"; assimilate-tests.yaml ignored"` appended when `SidecarIgnored`; `"assimilate-tests.yaml"` for a sidecar test.

- [ ] **Step 1: failing tests** — `internal/tests/tests_test.go`, with a `fakeTree` (`dirs []string; err error`) and a `writeFile(t, root, rel, content)` helper:

```go
func TestReadSidecar(t *testing.T) {
	cases := []struct{ name, yaml string; want []string; wantErr string }{
		{"two platforms", "platforms:\n  - linux/amd64\n  - linux/arm64\n", []string{"linux/amd64", "linux/arm64"}, ""},
		{"duplicates kept for the caller", "platforms: [linux/amd64, linux/amd64]\n", []string{"linux/amd64", "linux/amd64"}, ""},
		{"empty list", "platforms: []\n", nil, "platforms is required"},
		{"missing key", "{}\n", nil, "platforms is required"},
		{"empty entry", "platforms: ['']\n", nil, "platforms[0] must be a non-empty string"},
		{"unknown key", "platforms: [linux/amd64]\nargs: {a: b}\n", nil, "field args not found"},
	}
	// each: write file, call ReadSidecar, compare; every error message starts with the file path
}

func TestDiscoverUnion(t *testing.T) {
	// root with lib/TESTS.jobs + lib/assimilate-tests.yaml (amd64, arm64), services/a/TESTS.jobs, services/b/ (no test)
	// envs: staging {a: /services/a amd64 args v=1, b: /services/b amd64}, prod {a: /services/a amd64 args v=1 build-file BUILD.prod}
	// tree: /lib, /services/a
	// want Tests: [ {a image, Envs [staging prod], Spec.Key == a.TestOf().Key}, {/lib amd64 wholetree}, {/lib arm64 wholetree} ]
	// want Untested: [b]; labels "a", "/lib linux/amd64", "/lib linux/arm64"; Origin strings as specified
}

func TestDiscoverSidecarDuplicates(t *testing.T) { /* sidecar [amd64, amd64] → one test */ }

func TestDiscoverMissingSidecar(t *testing.T) {
	// lib/TESTS.jobs without sidecar → error contains "/lib/TESTS.jobs: no platforms declared" and "/lib/assimilate-tests.yaml"
}

func TestDiscoverRootTest(t *testing.T) {
	// tree ["/"], no root image: without sidecar → error mentions "/TESTS.jobs"; with root sidecar → one test, Spec.WholeTree false, Path "/"
}

func TestDiscoverTemplateWinsPerScope(t *testing.T) {
	// services/a has TESTS.jobs and a sidecar [arm64]; staging has image a (amd64); prod has none.
	// envs [prod] → one sidecar test /services/a arm64 (whole tree). envs [staging, prod] → one template test, SidecarIgnored true, Origin ends "; assimilate-tests.yaml ignored"
}

func TestDiscoverUntestedImages(t *testing.T) {
	// image path /svc/x not in tree → in Untested once although it appears in two envs; not an error
}

func TestDiscoverLabelCollision(t *testing.T) {
	// staging image "api" /svc amd64 sources [/proto]; prod image "api" /svc amd64 (no sources) → two tests,
	// labels "/svc linux/amd64 sources=/proto" and "/svc linux/amd64"; an args collision renders " k=v" sorted
}

func TestDiscoverTreeError(t *testing.T) { /* fakeTree.err is returned wrapped */ }
```

- [ ] **Step 2: run** `go test ./internal/tests/` — FAIL (package missing).
- [ ] **Step 3: implement** `tests.go` per the rules; keep the label pass a separate unexported function `label(tests []Test)` called at the end of `Discover`.
- [ ] **Step 4: run** `go test -race ./internal/tests/` — PASS.
- [ ] **Step 5: commit** `tests: discover TESTS.jobs from templates and the project tree`.

---

### Task 4: builds — memo hits in the result

**Files:** Modify `internal/builds/builds.go`; test `internal/builds/builds_test.go` (the `fake` and `runCollect` helpers).

**Interfaces — produces:**
- `Result.Cached bool` — set with `StateDone` when the terminal snapshot shows the test fast-pathed on the memo: any node whose name starts with `wire.KindTestValue + "_"` or `wire.KindTestRun + "_"` has `Cached: true`. Builds leave it false.
- `cachedTest(snap *api.Snapshot) bool` — that predicate; false for nil.

The runner's `sink` keeps the latest snapshot it forwarded (a pointer field it owns per build); `build()` reads it after `Follow` returns `StateDone`.

- [ ] **Step 1: failing test** — append to `internal/builds/builds_test.go`:

```go
func TestResultCached(t *testing.T) {
	snapWith := func(nodes ...api.NodeSnap) followFn {
		return func(_ context.Context, sink jobs.Sink) (spec.BuildState, error) {
			sink.Snapshot(api.Snapshot{Phase: "done", Terminal: true, Nodes: nodes})
			return spec.StateDone, nil
		}
	}
	specs := []spec.BuildSpec{
		{Name: "value-hit", Path: "/a", Platform: "linux/amd64", Test: true},
		{Name: "run-hit", Path: "/b", Platform: "linux/amd64", Test: true},
		{Name: "ran", Path: "/c", Platform: "linux/amd64", Test: true},
		{Name: "build", Path: "/d", Platform: "linux/amd64"},
	}
	f := &fake{follow: map[string]followFn{
		"value-hit": snapWith(api.NodeSnap{Node: "testvalue_00", Phase: "done", Cached: true}),
		"run-hit":   snapWith(api.NodeSnap{Node: "testvalue_00", Phase: "done"}, api.NodeSnap{Node: "testrun_00", Phase: "done", Cached: true}),
		"ran":       snapWith(api.NodeSnap{Node: "testvalue_00", Phase: "done"}, api.NodeSnap{Node: "testrun_00", Phase: "done"}, api.NodeSnap{Node: "import_00", Phase: "done", Cached: true}),
		"build":     snapWith(api.NodeSnap{Node: "buildvalue_00", Phase: "done", Cached: true}),
	}}
	results, _, err := runCollect(t, context.Background(), "/proj", "", specs, f)
	if err != nil { t.Fatal(err) }
	for i, want := range []bool{true, true, false, false} {
		if results[i].Cached != want { t.Errorf("%s: Cached = %v, want %v", specs[i].Name, results[i].Cached, want) }
	}
}
```

- [ ] **Step 2: run** `go test ./internal/builds/ -run TestResultCached` — FAIL (no `Cached`).
- [ ] **Step 3: implement** `Result.Cached`, `cachedTest`, the sink's latest-snapshot field, and the read in `build()`.
- [ ] **Step 4: run** `go test -race ./internal/builds/` — PASS (the exact-event-sequence tests must be untouched: no new events).
- [ ] **Step 5: commit** `builds: record test memo hits in the result`.

---

### Task 5: cmd — the `test` command

**Files:** Modify `internal/project/project.go` (+ `project_test.go`), `cmd/assimilate/main.go` (+ `main_test.go`).

**Interfaces — consumes:** `tests.Discover`, `tests.Env`, `tests.Set`, `Test.Origin()` (Task 3); `builds.Result.Cached` (Task 4); `jobs.Local.TestDirs` (Task 2).
**Interfaces — produces:**
- `project.Envs(root string) ([]string, error)` — sorted environment names (the existing `listEnvs` exported with an error when there are none: `"%s contains no environments"`).
- `test` command: `ArgsUsage: "[environment]"`, flags `--plain`, `--list`; `reorderArgs` handles `"test"`.
- `testScope(c *cli.Context) (root string, envs []tests.Env, err error)` — 0 or 1 positional; one env via `project.EnvDir` + `tmpl.Scan`; none via `project.Envs`, each scanned. No `LoadConfig`.
- `showRun(c *cli.Context, bctx context.Context, cancel context.CancelFunc, names []string, events <-chan spec.Event) error` — the TUI-or-plain block extracted from `deploy` and shared.
- `testSummary(results []builds.Result) string` — exactly:
  - no results: `no tests found`
  - otherwise `"%d passed (%d already passed), %d failed"`; append `", %d cancelled"` only when cancelled > 0.
- No change in `internal/ui`: the TUI has no build-specific wording (the footer counts `done`), and jobs-iroh's `tui.FoldSnapshot` already folds a testvalue row with the `test` stage and `(cached)`.
- `runTests(c *cli.Context) error` — the flow: scope → `jobs.Open` → `tests.Discover(ctx, root, envs, local)` → `--list`: print rows (`label`, `path`, `platform`, `k=v …`, `Origin()`, tab-separated via `text/tabwriter`) then `images without TESTS.jobs: a, b` (or nothing when none) and return nil → require `JOBS_SERVER` → `preflightSources` on the specs → `jobs.Dial` → `builds.Run` with registry `""` over `[]spec.BuildSpec` from `set.Tests` and `names` from `Spec.Name` → `showRun` → per-failure lines on stderr as `deploy` → `fmt.Println(testSummary(results))` → `local.MaybeGC` → error `"%d of %d tests failed"` (or `cancelled` when every bad one was cancelled) when any result is not `StateDone`.

- [ ] **Step 1: failing tests**

`internal/project/project_test.go`:
```go
func TestEnvs(t *testing.T) {
	root := mkTree(t, []string{"assimilate-templates/staging", "assimilate-templates/prod"}, nil)
	got, err := Envs(root); if err != nil { t.Fatal(err) }
	if want := []string{"prod", "staging"}; !reflect.DeepEqual(got, want) { t.Fatalf("Envs = %v, want %v", got, want) }
	if _, err := Envs(mkTree(t, []string{"assimilate-templates"}, nil)); err == nil || !strings.Contains(err.Error(), "contains no environments") { t.Fatalf("empty: %v", err) }
}
```

`cmd/assimilate/main_test.go`: add to `TestReorderArgs` the cases `{"assimilate","test","staging","--plain"} → {"assimilate","test","--plain","staging"}` and `{"assimilate","test","--list"} → unchanged`; add

```go
func TestAppParsesTestCommand(t *testing.T) {
	// stub the "test" command's Action like TestAppParsesTrailingRolloutFlag; run reorderArgs([]string{"assimilate","test","staging","--list","--plain"});
	// assert c.Bool("list"), c.Bool("plain"), NArg()==1, First()=="staging"; then run {"assimilate","test"} and assert NArg()==0
}

func TestTestSummary(t *testing.T) {
	done := builds.Result{State: spec.StateDone}
	hit := builds.Result{State: spec.StateDone, Cached: true}
	failed := builds.Result{State: spec.StateFailed}
	cancelled := builds.Result{State: spec.StateCancelled}
	for _, tc := range []struct{ in []builds.Result; want string }{
		{nil, "no tests found"},
		{[]builds.Result{done, hit, hit}, "3 passed (2 already passed), 0 failed"},
		{[]builds.Result{done, failed}, "1 passed (0 already passed), 1 failed"},
		{[]builds.Result{hit, cancelled, cancelled}, "1 passed (1 already passed), 0 failed, 2 cancelled"},
	} {
		if got := testSummary(tc.in); got != tc.want { t.Errorf("testSummary = %q, want %q", got, tc.want) }
	}
}
```

- [ ] **Step 2: run** `go test ./internal/project/ ./cmd/assimilate/ -run 'TestEnvs|TestReorderArgs|TestAppParsesTestCommand|TestTestSummary'` — FAIL.
- [ ] **Step 3: implement** `project.Envs`; in `main.go` the command, `reorderArgs` case, `testScope`, `showRun` (and make `deploy` call it), `testSummary`, `runTests`. Update the app `Usage` string to mention tests.
- [ ] **Step 4: run** `gofmt -l . ; go vet ./... ; go test -race ./...` — nothing listed, PASS, PASS.
- [ ] **Step 5: commit** `assimilate test: run the monorepo's TESTS.jobs through jobs-iroh`.

---

### Task 6: documentation and a smoke run

**Files:** Modify `README.md`, `docs/design.md`.

- [ ] **Step 1: README** — add `assimilate test staging` and `assimilate test` lines to the command block at the top with one-line comments; in "Project layout" add `TESTS.jobs` under `services/api/` and a `lib/proto/` entry with `TESTS.jobs` and `assimilate-tests.yaml  # platforms for a test that is not an image`; add a `## Tests` section before `## Development` covering: the command and the `[environment]` scope rule, the union (template paths with platform and args from the image, so the test joins the image's build; every other `TESTS.jobs` with its sidecar's platforms and the whole project tree as context), the sidecar's exact schema, that nothing is inferred and a missing sidecar is an error, what "changed" means (the server's `test-passed` memo; `(cached)` rows and the "already passed" count), `--list` and its untested-images trailer, `--plain`, the exit code, and the `BUILD.jobs`-only caveat for `build-file` images.
- [ ] **Step 2: design.md** — add the two commands to the intro block; add a `## Tests` section after "Build flow" summarizing decisions 2–9 of the spec in the document's voice (identity, whole-tree context and why K churn is acceptable for tests, the sidecar, the memo shapes that count as hits); add `internal/tests/   TESTS.jobs discovery: template paths ∪ project tree, sidecar platforms, labels` to the Packages list.
- [ ] **Step 3: smoke run** — build into the scratchpad and run against a synthetic project with no server:

```bash
go build -o "$SCRATCH/assimilate" ./cmd/assimilate
P="$SCRATCH/proj"; mkdir -p "$P/assimilate-templates/staging" "$P/services/api" "$P/lib/proto"
printf 'assimilate-domain: demo\n' > "$P/assimilate-templates/staging/assimilate.yaml"
printf 'containers:\n  - name: api\n    image:\n      type: jobs-build\n      name: api\n      path: /services/api\n      platform: linux/amd64\n' > "$P/assimilate-templates/staging/api.yaml"
printf 'def test(): pass\n' > "$P/services/api/TESTS.jobs"; printf 'def test(): pass\n' > "$P/lib/proto/TESTS.jobs"
(cd "$P" && ASSIMILATE_DATA_DIR="$SCRATCH/data" "$SCRATCH/assimilate" test --list; echo "exit $?")          # expect the missing-sidecar error for /lib/proto, exit 1
printf 'platforms:\n  - linux/amd64\n' > "$P/lib/proto/assimilate-tests.yaml"
(cd "$P" && ASSIMILATE_DATA_DIR="$SCRATCH/data" "$SCRATCH/assimilate" test --list; echo "exit $?")          # expect two rows: "api … image api (staging)" and "/lib/proto linux/amd64 … assimilate-tests.yaml"; exit 0
(cd "$P" && ASSIMILATE_DATA_DIR="$SCRATCH/data" "$SCRATCH/assimilate" test; echo "exit $?")                 # expect "JOBS_SERVER must be set", exit 1
rm -f "$SCRATCH/assimilate"
```

- [ ] **Step 4: run** `gofmt -l . ; go vet ./... ; go test -race ./...` once more — clean.
- [ ] **Step 5: commit** `docs: the assimilate test command`.
