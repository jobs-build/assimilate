# `sources`: sibling paths for jobs-build images

Date: 2026-10-06
Status: awaiting review

## Problem

assimilate ingests the directory named by a jobs-build image object's
`path` and builds from that subtree alone: "recipes cannot reference
anything outside their build root" (design.md, build flow). A monorepo
therefore cannot share source between two images. The case that prompted
this: validator-watch has eight Go services that each carry a
byte-identical copy of one `.proto` file and of its 1,200-line generated
bindings, with a test per service to catch drift.

jobs-iroh has had the mechanism since v0.11.0 (sibling sources) and
v0.12.0 (`closure=`): a definition with `Dir` set and `Ctx = CtxWidened`
builds a subdirectory of a wider source context, the pin stage computes
the paths the build really depends on, and the Go plugin's monorepo mode
follows relative `replace` directives into sibling modules. assimilate
never sets `Dir`.

Simply widening the ingest to the project root is not an option: the
whole tree would enter the build key K, which is the image tag, so every
commit anywhere would re-tag — and redeploy — every image.

## Decisions

- **Declared, not discovered.** A jobs-build image object gains an
  optional `sources` list: the other project paths the build may
  reference. Rejected: tagging by the server's covered key (it exists
  only after the server has pinned the build, so `render` could no longer
  work offline, and the registry would have to serve those tags);
  deriving the list from `go.mod` (ecosystem logic in assimilate, again
  for every other ecosystem).
- **Pruned context.** With `sources`, the build's source is the project
  root pruned to `path` plus `sources`, and the definition carries `Dir`
  and the widened-context marker. K is computed offline as today and
  depends only on those paths.
- **Nothing changes without `sources`.** Same subtree ingest, same
  definition bytes, same K. Adopting this release re-tags nothing by
  itself.
- No new command, flag or config key; publishing and ArgoCD are
  untouched.

## Template syntax

```yaml
image:
  type: jobs-build
  path: /services/tally-mev
  sources:            # optional
    - /proto
  platform: linux/amd64
```

`sources` is a YAML sequence of strings, each a file or directory
relative to the project root, written like `path`.

- Each entry is cleaned to a `/`-prefixed slash path. An entry with a
  `..` segment is an error, as for `path`. An empty or non-string entry
  is an error. A duplicate `sources` key is an error, like every other
  key.
- The list is normalized: duplicates removed, entries equal to or inside
  `path` or inside another entry dropped, the rest sorted. An empty
  result is the same as no `sources`. An entry that contains `path` is
  allowed; the context is then that entry and the other sources.
- `/` as an entry is an error: it is the whole project, which is what
  this feature exists to avoid.
- `sources` on a root build (`path: /` or absent) is an error: a root
  build already covers the project.

`BuildSpec` gains `Sources []string` (normalized). `BuildSpec.Key`
appends them, length-prefixed like the other components, so two image
objects that differ only in `sources` are two builds; a spec without
sources keeps the key it has today.

## Build flow

For a spec without `sources`: unchanged.

For a spec with `sources`:

1. **Ingest the project root**, once per run however many specs need
   it, with `IngestSourceDir` — the same call as today, so `.amberignore`
   files at the root and below apply and `.git` is skipped.
2. **Check** that `path` and every source exist in the ingested tree. A
   miss is an error naming the image and the path, with a hint when the
   path exists on disk (then an `.amberignore` excludes it).
3. **Prune**: `PruneTree(root, {path} ∪ sources)` gives the context tree
   — exactly those paths at their project-relative positions, with
   normalized metadata.
4. **Define**: `Definition{Source: TreeInput(context), Dir: path without
   its leading slash, Ctx: builddef.CtxWidened, Platform, Params,
   BuildFile}` — what jobs-iroh's own client builds for a subdirectory.
   `BuildFile` stays relative to the service directory.
5. **Push** the context tree and **submit**, as today.

`render` runs steps 1–4 and needs no server, as today.

### What K depends on

| | without `sources` | with `sources` |
|---|---|---|
| content and modes under `path` | yes | yes |
| content and modes under each source | — | yes |
| anything else in the project | no | no |
| mtime, uid, gid | yes (amber's ingest hashes them) | no (`PruneTree` normalizes them) |

So a tag with `sources` is the same on a fresh clone of the same commit,
which a tag without is not (the caveat in the README stays for those).

### What the server does with it

Stated here because it defines what `sources` means. For a definition
with `Dir` the pin stage computes the build's covered paths inside the
context: the directory itself, or the recipe's `closure=` if it returns
one, plus what the recipe declares in `sources=` and what plugins
contribute. A path the recipe or a plugin needs that is not in the
context fails the build before it runs, naming the path. The sandbox
holds the covered paths in their
project layout, with the working directory at the service.

`sources` in a template is therefore the *bound* on what a build may
reference; what it does depend on is the recipe's business. Listing too
little fails loudly; listing too much costs a new tag when those paths
change.

## Requirements and compatibility

- A spec with `sources` needs a jobs-iroh server that accepts
  widened-context definitions: v0.11.0, or v0.12.0 if the recipe returns
  `closure=`. An older server rejects the definition at submit.
- The first run with any such spec walks the whole project root. A
  project with large untracked directories at the root wants a root
  `.amberignore`.

## Errors

All before any build starts, each naming the template file and line
(syntax) or the image (existence):

- `sources` not a sequence, an entry not a non-empty string, an entry
  with `..`, an entry `/`;
- `sources` on a root build;
- a source, or `path`, that is not in the ingested project tree.

## Code

- `internal/spec`: `BuildSpec.Sources`, `Key`.
- `internal/tmpl`: decode and normalize `sources`; the root-build and
  `..` errors.
- `internal/jobs`: `Local.Context(root Source, keep []string)` (check +
  prune); `definition` sets `Dir` and `Ctx` when the spec has sources.
- `internal/builds` and `cmd/assimilate` (`render`): choose the source of
  each spec — subtree ingest, or the shared root ingest and its pruned
  context.

## Testing

- **tmpl:** a list is decoded, cleaned, de-duplicated, collapsed and
  sorted; entries inside `path` vanish; each error above.
- **spec:** equal keys for equal normalized sources in any written
  order; different keys for different sources; the key of a spec without
  sources is unchanged.
- **jobs:** without sources, the definition bytes and K equal those of
  the previous code path (a fixed expectation, so a refactor cannot move
  them). With sources: `Dir` and `Ctx` are set; K is unchanged by a new
  file outside the covered paths and by a changed mtime inside them; K
  changes with content under `path` and under a source; a missing source
  and an `.amberignore`d source are errors naming the path.
- **builds:** N specs with sources ingest the root once; a run without
  any does not ingest the root.
- **End to end**, by the maintainer against a real server: a Go service
  whose `go.mod` has `replace … => ../../lib` builds and runs.

## Documentation

README (template section: the key, the tag table, the root
`.amberignore` note) and `docs/design.md` (build flow; "recipes cannot
reference anything outside their build root" becomes true only without
`sources`).

## Not in scope

Discovering sources automatically; tags derived from the server's
covered key; sibling builds (`subbuild("//…")`); per-source ignore
rules.
