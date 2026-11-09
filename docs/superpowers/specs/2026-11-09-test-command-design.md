# `assimilate test`: run the monorepo's jobs-iroh tests

Date: 2026-11-09
Status: approved

## Problem

jobs-iroh v0.36.0 added tests: a `TESTS.jobs` next to `BUILD.jobs` defines
`test()`, which runs in the build sandbox with the directory's own build
available as an input. A test produces no output; a pass is recorded under
KT, a key derived from what the sandbox actually saw (script, environment,
covered sources, platform, and the *content* of every input). A test whose
KT already carries a pass is not run again. A failed test leaves no record
and runs again on the next submit.

assimilate knows the monorepo: it finds the project root, scans every
environment's templates for `jobs-build` image objects, and already
ingests, pushes, submits and follows builds on a jobs-iroh server. What it
cannot do yet is run the repository's tests. The wish, in the user's words:
`assimilate test` runs "the tests of the sources that have changed", where
changed means "the jobs server has not successfully run the tests for the
source code before, source code meaning whatever TESTS.jobs has defined as
its dependencies".

That definition of changed is exactly jobs-iroh's memo. assimilate's job is
therefore not to diff anything. It is to find every test the monorepo has,
give each one the platform and params it needs, submit all of them, and
show what happened. The server runs the ones with no pass on record and
fast-paths the rest.

## Goals

- `assimilate test [environment]` finds every `TESTS.jobs` the monorepo
  has, submits each as a jobs-iroh test, follows them in the familiar TUI
  (plain lines without a TTY), and exits non-zero when any test fails.
- No new configuration for tests of deployed images: platform and params
  come from the template image object. A library test declares its
  platforms in one small file next to its `TESTS.jobs`.
- A test attached to a deployed image exercises the same build that
  `deploy` would ship: same source tree, same platform, same params, so the
  test's build input joins the image's build node and memo-hits when the
  image was built before.
- `--list` shows the discovered set and where each test came from, without
  a server.

## Non-goals

- Deciding what changed. jobs-iroh's `test-passed:<KT>` memo decides.
  assimilate never reads git history or compares trees.
- Running tests as part of `deploy`, or gating a deploy on tests. A future
  `deploy --test` can build on this command; not now.
- Scoping the run to the current directory. Like `deploy`, the command
  behaves the same from anywhere inside the monorepo.
- Test recipes other than `TESTS.jobs`, test-only params, or a per-image
  `tests:` key in templates. Nothing in the templates changes.
- Tests of a non-default build recipe. jobs-iroh's predeclared `build` in a
  test is always the directory's `BUILD.jobs` build; an image with
  `build-file: BUILD.prod` still gets its tests run against the `BUILD.jobs`
  build of its directory. This is a jobs-iroh rule and is documented, not
  worked around.
- Pulling the pass memo home to the local store (what `jobs-client
  remote-test` does so a later local `jobs-client test` memo-joins).

## Anti-goals

- A test that passes in assimilate but tests a different artifact than the
  one deployed. Image-attached tests must use the image's exact source
  tree, platform and params.
- Re-running tests that jobs-iroh already recorded as passed. Any change
  assimilate makes to how a definition is built must keep K and KT stable
  for an unchanged tree.
- Silently skipping a test. Every `TESTS.jobs` in the project tree ends up
  either in the run or in an explicit error.

## Constraints, from jobs-iroh v0.36.0

- A test definition is `builddef.Definition{Source, Dir, Ctx, Platform,
  Params, BuildFile, Test: true}`. `BuildFile` empty means `TESTS.jobs`. K
  differs from the build's K by construction; submit, watch, logs and
  cancel work unchanged, and a testvalue row folds in the TUI like a
  buildvalue row with its terminal stage named `test`.
- The predeclared `build` input is the build of the same (source, dir,
  platform, params) with the default `BUILD.jobs`. To join the image's
  build node, the test definition must carry the image's exact `Source`,
  `Dir` and `Ctx`.
- `amber.PruneTree` refuses a keep set that resolves to the context root.
  A whole-tree context is the unpruned root tree itself, which is what
  `jobs-client` submits by default (git root as context, `Dir` set,
  `Ctx = widened`).
- The TESTS.jobs test's covered sources are its directory plus its
  `closure` plus what the plugins detect, computed at pin time. A wider
  context therefore does not widen KT: a docs-only edit elsewhere re-keys K
  and F (eval churn on the server) but not KT, so the test does not re-run.
- An old server rejects a definition with the `test` field at the
  canonicality check, loudly. Nothing to handle beyond surfacing the error.

## Decisions

### 1. Command

```
assimilate test [environment] [--plain] [--list]
```

- With an environment: that environment's templates are the image source.
- Without one: every environment under `assimilate-templates/`, in sorted
  name order.
- `--list` prints the discovered tests and exits without connecting to a
  server. `--plain` is `deploy`'s.
- `JOBS_SERVER` (and optional `JOBS_SERVER_ADDR`) as for `deploy`; required
  unless `--list`.
- The per-environment `assimilate.yaml` is not loaded: tests need no
  registry, git or ArgoCD settings. An unknown environment name is still an
  error listing the available ones.
- `reorderArgs` learns the `test` subcommand, so `assimilate test staging
  --plain` parses.

### 2. Discovery is the union of two sources

**Template tests.** For every `jobs-build` image object in scope, in the
order `deploy` would build them (environments in sorted order, then
appearance order within an environment): if the image's `path` contains a
`TESTS.jobs` in the project tree, that is a test with the image's source
tree, platform and args.

**Project-tree tests.** The project root is ingested the way
`Local.ingestRoot` already does for images with `sources` (honoring
`.amberignore`, never `.git`), and the resulting amber tree is walked with
`Store.Ls`. Every directory holding a regular file named `TESTS.jobs` is a
candidate. Candidates whose directory equals some template test's `path`
are dropped: the template already supplies better platform and params for
that directory. The rest are the project-tree tests, sorted by path.

Walking the ingested tree rather than the filesystem or `git ls-files`
means discovery sees exactly what a build can see. A `TESTS.jobs` under an
`.amberignore`d directory is not a test, and nothing found can later fail
with "is not in the project tree".

Template tests come first in the row list, then project-tree tests.

### 3. Identity and dedup

A test is identified by (path, sources or context mode, platform, args).
Two template images with the same path, platform and args but different
`build-file` are one test (the test's build is `BUILD.jobs` either way).
The same image object appearing in two environments is one test. A
project-tree directory tested on two platforms is two tests.

In code this is a `spec.BuildSpec` with a `Test bool` (mirroring
`builddef.Definition.Test`) and a context mode for the whole-root case;
`Key()` includes both, and for tests ignores `BuildFile`. `jobs.definition`
sets `def.Test` from the spec. `builds.Run` then orchestrates tests exactly
as it orchestrates builds: grouped by source tree, one ingest and push per
group, concurrent submit and follow. Whether the builder adds the two fields
to `BuildSpec` or introduces a `TestSpec` that converts to one is the
builder's call; the Key semantics above are not.

### 4. Source context per origin

- A template test uses the image's source exactly: the ingested subtree
  when the image has no `sources`; the project root pruned to path plus
  sources, `Dir = path`, `Ctx = widened` when it has. This is what makes
  the test's `build` input the image's build.
- A project-tree test at a non-root path uses the **whole project tree**,
  unpruned, as context: `Source = root tree`, `Dir = path`, `Ctx = widened`.
  This is `jobs-client test`'s own default (the git root as context), so a
  developer who ran `jobs-client remote-test` in that directory against a
  checkout whose git root is the project root produced the very same K.
  Sibling references (`//lib/common`, Go `replace` directives) resolve
  without any declaration, which matters because a library has no template
  object to declare `sources` on. The cost is one delta push of the project
  tree per run and eval churn on the server when unrelated files change;
  KT, and so the memo, is unaffected (constraints above).
- A project-tree test at the root itself (`TESTS.jobs` beside
  `assimilate-templates/`) is a root test: `Source = root tree`, no `Dir`,
  default `Ctx`, like an image with `path: /`.

A template test whose recipe needs a sibling the image does not list in
`sources` fails at evaluation, as the image's own build would. The fix is
the same as for the build: add the path to the image's `sources`.

### 5. Platforms for project-tree tests: `assimilate-tests.yaml`

A project-tree test has no template to say where it runs, and jobs-iroh's
recipe cannot say it either: platform is an input to `test()`, visible as
the read-only `platform` global, not something the recipe returns. So the
directory declares it in a sidecar file next to `TESTS.jobs`:

```yaml
# assimilate-tests.yaml
platforms:
  - linux/amd64
  - linux/arm64
```

- The file name is `assimilate-tests.yaml`. Not `assimilate.yaml`: that
  name already means the environment config under `assimilate-templates/`,
  with a different schema.
- Strict schema, decoded with `KnownFields(true)` like the environment
  config: one key, `platforms`, a non-empty list of non-empty strings.
  Anything else is an error naming the file and line.
- The test runs once per declared platform, with the whole project tree as
  context (decision 4) and no params.
- A `TESTS.jobs` that is neither an image path in scope nor accompanied by
  the file is a **preflight error**, reported before anything is pushed,
  naming the directory and the one-line file to create. Nothing is
  inferred: no default platform, no flag.
- Where the directory is also an image path in scope, the template wins
  for that run (decision 2): platform and args come from the image so the
  test joins the image's build. The sidecar is ignored there, and `--list`
  says so in the origin column. The same directory therefore behaves
  consistently whether or not this environment deploys it: as an image it
  tests the shipped artifact, otherwise it runs where the file says.
- The file is read from disk at the directory the tree walk found. It may
  grow other per-test knobs later (params, exclusions); this spec defines
  only `platforms`.

### 6. Submission and following

Each test is submitted through the existing `Client.Submit` path with a
test definition; the server echoes K and the local cross-check applies as
for builds. `Client.Follow` drives it to the terminal snapshot. Phase
`done` is a pass (the testrun either ran or fast-pathed on the memo),
`failed` is a failure, `cancelled` a cancellation. `builds.Run`'s
concurrency, cancel-on-SIGINT and drain rules apply unchanged.

A pass that was a memo hit is recognisable from the terminal snapshot, in
either of two shapes. An unchanged tree resubmitted fast-paths at the
request's own node: the `testvalue` is created with `Cached: true` and no
`testrun` node ever exists. A tree that changed in ways the test does not
cover (new K and F, same KT) creates a `testrun` node with `Cached: true`.
Both count as "already passed". `builds.Result` records the flag so the
summary can report it.

The submitted test definition carries an empty `BuildFile` even when the
image has `build-file: BUILD.prod`: jobs-iroh reads `BuildFile` as the test
recipe path too, so a copied value would make it look for `BUILD.prod` as
the test recipe. Converting an image spec to a test spec blanks it.

### 7. UI

The TUI is `deploy`'s, fed the same `spec.Event` stream: one row per test,
unfolding into the request's graph (the test's inputs, including the
directory's build, hang under it; the terminal stage is `test`, shown
`(cached)` on a memo hit). All tests passing ends the TUI; a failure holds
the screen on the failing step's output until `q`. Without a TTY, plain
prefixed lines. Nothing in the TUI itself changes except, where the
screen says "builds", wording that fits tests too.

Row labels: a template test is labelled with its image's display name (the
explicit `name`, else `<path> <platform>`); a project-tree test with
`<path> <platform>`. When two rows would carry the same label, both fall
back to `<path> <platform>` followed by the sorted `key=value` args, so
every row is distinguishable.

### 8. Output, summary and exit code

After the stream ends the command prints one summary line to stdout:
how many tests passed, how many of those were already passed on the server
(memo hits), how many failed, how many were cancelled. Each failed or
cancelled test is also listed on stderr with its state and short error, as
`deploy` does for images. Exit status is non-zero when any test did not end
in `done`, and zero otherwise, including when nothing was found (with a
note saying so).

### 9. `--list`

Prints, one line per discovered test: label, path, platform, args, and
origin. Origin is either the image's display name with the environments it
appears in (with a note when a sidecar file was ignored in its favour), or
`assimilate-tests.yaml`. After the tests, a line naming the template
images in scope whose path has no `TESTS.jobs`, so a missing test is
visible. Requires only the local store (the root ingest for the walk); no
server, no push. Exit status zero.

### 10. No new configuration

`assimilate.yaml` and the template vocabulary are unchanged. The only new
file is the per-directory `assimilate-tests.yaml` of decision 5. If a real
case appears that needs an explicit test list or exclusions, that is a
follow-up spec, most likely as further keys in that file.

## Flow

1. Resolve the project root from the cwd. Resolve the environment(s).
2. Scan each environment's templates (`tmpl.Scan`), collect image specs in
   order, dedupe by test identity into template tests after checking each
   path for `TESTS.jobs` in the project tree.
3. Ingest the project root once; walk it for `TESTS.jobs`; drop directories
   already covered by template tests; read each remaining directory's
   `assimilate-tests.yaml` and produce one test per declared platform. A
   directory without the file, or with an invalid one, stops the command
   here.
4. `--list`: print and exit.
5. Require `JOBS_SERVER`; dial; run everything through `builds.Run` with
   the ordered test specs; render through `ui.RunTUI` or `ui.RunPlain`.
6. Print the summary; exit non-zero on any failure or cancellation.

The root ingest in step 3 is the same memoized ingest `preflightSources`
uses for images with sources, so a monorepo whose images declare sources
pays for it once per run, as today.

## Left to the builder

- Whether tests live in `BuildSpec` with two extra fields or in a
  `TestSpec` type; the Backend interface shape that follows.
- Exact `--list` formatting and the exact summary wording.
- Where the discovery code lives (a new `internal/tests` package is the
  natural home; `internal/jobs` gains the tree walk if it needs store
  access).
- How `builds.Result` learns the memo-hit flag from the terminal snapshot.
- Test strategy details, within the expectations below.

## Testing expectations

- Discovery: unit tests over a synthetic project tree covering the union
  and its dedup rules (template path with and without `TESTS.jobs`, a
  project-tree test declaring two platforms, the same image in two
  environments, two images differing only in `build-file`, a root
  `TESTS.jobs`, an `.amberignore`d `TESTS.jobs` that must not appear, a
  directory that is an image in one environment and sidecar-declared in
  another, a `TESTS.jobs` without the sidecar, a sidecar with an unknown
  key or an empty list).
- Identity: `Key()` and the definition's canonical bytes include `Test`
  and the context mode; a test definition's K differs from its build's K;
  an image test's `build` input definition equals the image's build
  definition (same `Source`, `Dir`, `Ctx`, platform, params).
- Orchestration: `builds.Run` with a fake backend runs tests and records
  the cached flag from both memo shapes: a snapshot whose `testvalue` is
  `Cached`, and one whose `testrun` node is `Cached`.
- Conversion: an image spec with `build-file` set yields a test definition
  with an empty `BuildFile`.
- CLI: `reorderArgs` with `test`, `--list` without `JOBS_SERVER`, the
  missing-sidecar preflight error.
- UI: the label collision rule.

## Documentation

README gains a "Tests" section (the command, the union rule, the
`assimilate-tests.yaml` file, `--list`, the `BUILD.jobs`-only caveat) and
the project layout example shows the file next to a `TESTS.jobs`. `docs/design.md`
gains a "Tests" section next to the build flow and lists the new package.
