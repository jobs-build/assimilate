# assimilate — design

`assimilate` deploys the current state of a monorepo to Kubernetes: it builds
every image referenced by the k8s resource templates via a jobs-iroh server,
substitutes the resulting image references into the rendered manifests, pushes
them to a GitOps repo as a PR, and (with `--rollout`) merges the PR and
triggers an ArgoCD refresh/sync.

```
assimilate deploy staging            # build → render → push → PR
assimilate deploy staging --rollout  # … → merge PR → argocd refresh + sync
assimilate render staging            # render to stdout without building
```

(`render` resolves exact image references offline: the build key K is the hash
of the canonical build definition, computable from a local source ingest alone
— no server round-trip.)

## Project layout discovered at runtime

The project root is found by walking up from the current directory until a
directory containing `assimilate-templates/` is found. Each environment is a
subdirectory of it:

```
assimilate-templates/
  staging/
    assimilate.yaml        # environment config (not a template)
    api.yaml               # k8s resource templates (any nesting allowed)
    workers/queue.yaml
  prod/
    assimilate.yaml
    …
```

Every `*.yaml`/`*.yml`/`*.json` under the environment directory except
`assimilate.yaml` is a template. Templates are processed in lexical path
order; multi-document YAML files are supported. JSON templates are copied
verbatim — no jobs-build substitution.

## Environment config: `assimilate.yaml`

```yaml
assimilate-domain: monorepo     # required; names this source repo in every generated
                                # file so pruning only touches this repo's own files
git:
  github:                       # key selects the GitOps repo type (github or forgejo)
    repo: fables-for-robots/gitops   # owner/name
    path: clusters/staging           # directory within the repo to write to
    branch: main                     # optional; base branch for PRs (default: repo default branch)

registry: localhost:5000        # optional; host prefix of substituted image refs
                                # (default localhost:5000)

argocd:                         # optional; ArgoCD applications to roll out
  - url: https://argocd.example.com/applications/argocd/my-app
```

A `forgejo` GitOps repo takes the same keys plus the instance base URL
(GitHub needs none — its host is fixed):

```yaml
git:
  forgejo:
    url: https://git.example.com     # instance the API and git remote live on
    repo: my-org/gitops
    path: clusters/staging
    branch: main                     # optional
```

ArgoCD entries are application URLs as copied from the ArgoCD UI —
`https://<server>/applications/<app>` or
`https://<server>/applications/<namespace>/<app>`; assimilate parses the server
and application from them.

## Credentials — environment variables only

| Variable | Used for |
|---|---|
| `JOBS_SERVER` | jobs-iroh server endpoint ID (same var the jobs-client uses) |
| `JOBS_SERVER_ADDR` | optional direct `host:port` for the server (skips discovery) |
| `ASSIMILATE_DATA_DIR` | optional client store dir (default `~/.local/share/assimilate`; assimilate owns its store — `amber.Open`'s flock is single-process, so sharing jobs-client's dir would conflict) |
| `GITHUB_TOKEN` (or `GH_TOKEN`) | GitOps repo push + PR create/merge (github); when unset, `gh auth token` is used if the user is logged in with the GitHub CLI |
| `FORGEJO_TOKEN` (or `GITEA_TOKEN`) | GitOps repo push + PR create/merge (forgejo). When unset, the tea-style CLI configs are consulted — `forgejo/config.yml`, then `tea/config.yml`, resolved like tea does (adrg/xdg) — and an expiring OAuth login is refreshed and persisted in place, speaking tea's exact on-disk protocol: same YAML schema, same `<config>.lock` flock, same `/login/oauth/access_token` exchange with tea's public client ID. Keyring-backed logins (`auth_method: oauth`) are not readable; use the env var for those. The token is re-resolved right before the GitOps publish, since builds can outlast a short-lived access token. |
| `ARGOCD_AUTH_TOKEN` (or `ARGOCD_TOKEN`) | ArgoCD API |
| `ARGOCD_INSECURE=true` | skip TLS verification towards ArgoCD |

## Templates: the `jobs-build` image object

Anywhere in a template, a mapping value of an `image` key that contains
`type: jobs-build` is replaced by an image reference; any other `image` value
is left untouched.

```yaml
containers:
  - name: backend
    image:
      type: jobs-build
      name: backend            # optional; display name in the TUI (default: "<path> <platform>")
      path: services/backend   # source dir relative to the project root (default "/" = the root)
      build-file: BUILD.prod   # optional; recipe path (default BUILD.jobs)
      args:                    # optional; build params, become jobs `--param key=value`
        variant: slim
      platform: linux/amd64    # required
```

Rules:

- `platform` is required — a missing platform is a hard error before any build starts.
- Unknown keys in a `jobs-build` object are an error (typo protection).
- Identical specs (`path`+`build-file`+`args`+`platform`) are built once and
  substituted everywhere they appear.
- Build ordering (for the TUI list) is order of first appearance: files in
  lexical path order, objects in document order within a file.

YAML anchors/aliases and `<<` merge keys must not be used to define
`jobs-build` image objects: substitution rewrites the node in place, so
aliased sharing would silently corrupt the other reference. Scan rejects,
with a hard error, an `image` value that aliases a jobs-build mapping, a
merge key inside a jobs-build image mapping, and an anchor on a substituted
node that is referenced elsewhere.

## Build flow (jobs-iroh integration)

assimilate imports jobs-iroh's exported packages directly — no shelling out:

1. Open assimilate's own store (`amber.Open`), `IngestSourceDir` each unique
   `path` → source tree key. Ingesting the *subtree* (not the monorepo root)
   keeps K — and therefore the image tag — stable while that service's
   sources are unchanged *within one working tree*: amber's ingest hashes
   per-entry mode/uid/gid/mtime alongside content, so a fresh clone or CI
   checkout of the same commit yields a different tree key, a different K, a
   full rebuild and a new tag — which also makes the rendered manifests
   differ and defeats the "no changes → no PR" path. Mitigate on CI with a
   `git-restore-mtime`-style step; registry manifest *digests* depend only on
   the built output and stay stable — only tags churn. Subtree ingest is safe
   because recipes cannot reference anything outside their build root.
   `.amberignore` files apply only at or below the ingested path — a subtree
   ingest never consults a monorepo-root `.amberignore`, so centralized
   exclusions (`node_modules`, `.env`, …) silently don't apply: put a
   `.amberignore` inside each build path.
2. Construct the canonical `builddef.Definition{Source: TreeInput(sourceKey),
   Platform, Params: importdef.CanonicalParams(args), BuildFile}` → canonical
   bytes + build key **K** (known before the build even starts; K is the
   registry tag; the server echoes it back in `Submitted` as a cross-check).
3. Dial the server once on `jobs-amber-admin/1.0` (`amberclient.Dial`) and
   push each unique source tree under a `client-push/<hex>` scratch ref
   (mandatory prefix; pushes are delta — only missing objects transfer). The
   builds of a source tree start as soon as *its* push lands, while later
   trees are still pushing.
4. Dial the server once on `jobs-admin/1.0` (`amberclient.DialConn` + the
   frame protocol from `jobs-iroh/api` — the admin ALPN serves submit, watch,
   logs and cancel, so one API connection suffices), then for every build
   concurrently: `submit` → watch stream (`Snapshot` frames until `Terminal`)
   → per-node log follow streams (`logs` + `follow`), feeding the UI event
   stream. Log follows are capped per build *and* globally: a QUIC connection
   supports ~100 live streams, and N watches + follows + transient calls must
   stay under it.
5. A build is successful when its terminal snapshot phase is `done` (terminal
   phases are final — retries never surface as a terminal `failed`). The
   substituted reference is `<registry>/jobs:<K>` — **tag form**; the registry
   serves a single repository named `jobs` whose tags are build keys, so a
   path-form `…/jobs/<key>` reference would not resolve.
6. No output pull — the cluster pulls from a `jobs-registry` (pointed at the
   same jobs-server) that syncs and serves build outputs on demand. First
   pull of a fresh K is slow (on-demand assembly); rollout probes tolerate it.

## UI

On a PTY, a bubbletea TUI with a **two-level tree: image, then build**.
Level one is the image list (appearance order, live phase + elapsed);
`→`/`enter` unfolds an image into its jobs-iroh **build graph** — the same
logical rows jobs-client's own TUI shows, folded by jobs-iroh's exported
`tui.FoldSnapshot`/`FlattenTree` (v0.29.0) from the watch snapshots'
`NodeSnap.Deps` edges: one row per build/import with its stage
(`eval`/`resolve`/`pin`/`build`/`fetch`), live elapsed (server `ElapsedMs`
plus the client-clock delta since the snapshot arrived — skew-safe), done
durations, `(cached)` markers and failure summaries. A failed image
auto-unfolds so the failing node is one `↓` away.

```
┌ builds ─────────────────────┬ backend › app — running · build 41s ────┐
│ >▾backend           12s ⠋   │ go build ./…                            │
│    ▾ ⠋ app build 41s        │ …                                       │
│        ✓ fetch github (cached)                                        │
│  ▸worker           done ✓   │                                         │
├─────────────────────────────┴─────────────────────────────────────────┤
│ 1/3 done · ↑/↓ select · ←/→ fold · PgUp/PgDn scroll · q cancel        │
└───────────────────────────────────────────────────────────────────────┘
```

Right pane: for an **image row**, the combined build log (every followed
node's lines, `kind:key8 │ `-prefixed; per-build ring buffer); for a
**graph row**, that node's own output only (per-node rings fed by the same
follow streams — a node whose follow never attached under the budget shows
nothing). Log follows still attach per active node, capped per build and
client-wide; the tree opens no extra streams. When every build is finished
the TUI exits and the GitOps phase prints plain progress lines. Without a
PTY, everything is plain prefixed lines (`[backend] kind:key8 │ …`), like
jobs-client's non-TTY mode; snapshots are ignored there.

The graph edges need a jobs-iroh ≥ v0.28.0 server (`NodeSnap.Deps`);
against an older server the image rows simply never become expandable.

## GitOps push, PR, rollout

1. Shallow-clone the GitOps repo (token auth), branch
   `assimilate/<env>-<timestamp>`.
2. Render every template — the `jobs-build` object replaced by its image ref —
   into `<path>/<template-relative-path>`, preserving YAML comments and
   structure. Every written file carries an ownership marker: the SHA-256 of
   its body and the `assimilate-domain`. YAML gets a two-line comment header
   (`# assimilate-hash: <hex>`, `# assimilate-domain: <domain>`); JSON (which
   can't hold comments) gets a committed `<file>.json.assimilate` sidecar
   holding the same two lines (a sidecar from before domains holds the bare
   hash and is still recognized). An existing target file whose marker is
   missing (not generated by assimilate), stale (edited since) or of another
   domain is a conflict; a marker without a domain is adopted.
   Then every YAML/JSON file under `<path>` that carries this domain's
   marker but was not rendered is pruned (JSON with its sidecar); an edited
   one is a conflict. Other domains' files and unmarked files are never
   pruned, so several source repos can share one GitOps directory. Stale
   files with a pre-domain marker name no owner: they are left alone and
   listed in the log, unless the deploy runs with `--adopt-legacy=<dir>`,
   which prunes the ones under that subtree of `<path>` as this domain's
   (the one-time migration from ≤ 0.5; the operator picks a subtree holding
   only this repo's and environment's files). All conflicts are listed and
   the publication is refused, unless `--force` overwrites/prunes them
   (logged per file).
   A missing `assimilate-domain` fails config loading with a suggestion:
   the git root directory's name, plus the project root's path below it.
3. No diff against the base branch → report "no changes", skip the PR (a
   `--rollout` still triggers the ArgoCD refresh/sync).
4. Commit (message lists `name → image-ref` per build), push, open a PR.
   - without `--rollout`: print the PR URL and stop.
   - with `--rollout`: squash-merge the PR, delete the branch, then for each
     configured ArgoCD application: refresh (`GET
     /api/v1/applications/<app>?refresh=normal`) and sync (`POST
     /api/v1/applications/<app>/sync`), reporting per-app results.

## Packages

```
cmd/assimilate/       CLI entry (urfave/cli/v2, matching the jobs-iroh ecosystem)
internal/spec/        shared types: BuildSpec, BuildStatus, Event, Config, ImageRef
internal/project/     root discovery, env enumeration, assimilate.yaml loading
internal/tmpl/        template scan/extract (yaml.v3 node level), render with substitutions
internal/jobs/        jobs-iroh client: store, ingest, push, submit, watch, log follow
internal/builds/      orchestration: dedupe, run all builds, emit spec.Event stream
internal/ui/          bubbletea TUI + plain-line renderer, both consuming spec.Events
internal/ownership/   generated-file markers (hash + domain): YAML comment header, JSON sidecar; scan, remove
internal/gitops/      go-git clone/branch/commit/push + provider PR create/merge (GitHub, Forgejo)
internal/argocd/      minimal ArgoCD REST client (refresh, sync)
```

Dependency direction: `cmd` → everything; `ui` and `builds` meet only at
`spec` types (the TUI is testable by feeding synthetic events; orchestration is
testable with a fake jobs client).
