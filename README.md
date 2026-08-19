<p align="center">
  <img src="docs/logo.jpg" alt="assimilate — Jonas' Own Build System" width="600">
</p>

# assimilate

Deploy the current state of your monorepo to Kubernetes: assimilate builds
every image referenced by your k8s resource templates on a
[jobs-iroh](https://github.com/jobs-build/jobs-iroh) server, renders
the manifests with the resulting image references, publishes them to your
GitOps repo as a PR, and — with `--rollout` — merges the PR and triggers the
ArgoCD refresh/sync.

```sh
assimilate deploy staging            # build → render → push → PR
assimilate deploy staging --rollout  # … and merge the PR + argocd refresh/sync
assimilate render staging            # print rendered manifests (offline — no server, no build)
```

Run it from anywhere inside the monorepo; the root is found by walking up
until a directory containing `assimilate-templates/` appears.

On a terminal, `deploy` shows a TUI with two levels — image, then build:
the image list on the left (↑/↓ to select, ordered by appearance in the
templates), and `→`/`enter` unfolds an image into its live jobs-iroh build
graph (per-step state, elapsed, `(cached)`, failures — a failed image
unfolds itself). The right pane follows the selected row's output: the
whole build's log for an image row, that step's own output for a graph
row. `←` folds, `q` cancels. Without a TTY (CI), plain prefixed lines.

## Project layout

```
assimilate-templates/
  staging/
    assimilate.yaml        # environment config (not a template)
    api.yaml               # k8s resource templates (nesting + multi-doc OK)
    workers/queue.yaml
  prod/
    …
services/
  api/
    BUILD.jobs             # a normal jobs-iroh recipe
    …
```

## Templates

Anywhere in a template, an `image` key whose value is a mapping with
`type: jobs-build` is replaced by the image reference of a jobs-iroh build:

```yaml
containers:
  - name: api
    image:
      type: jobs-build
      name: api              # optional; display name in the TUI
      path: /services/api    # source dir relative to the repo root (default "/")
      build-file: BUILD.prod # optional; recipe path (default BUILD.jobs)
      args:                  # optional; jobs build params (--param key=value)
        variant: slim
      platform: linux/amd64  # required
```

`*.json` files under the environment directory are templates too, copied
verbatim — JSON holds no jobs-build objects.

Every other `image` value is left untouched. Identical specs are built once
and substituted everywhere. The substituted reference is
`<registry>/jobs:<K>` — the jobs-registry serves one repository named `jobs`
whose tags are build keys, and K depends only on the service's source tree,
params, platform and recipe, so untouched services keep their tag. Caveat:
the source tree hash includes file metadata (mode/uid/gid/mtime), so K is
stable only within one working tree — a fresh clone or CI checkout of the
same commit yields new tags and full rebuilds (use a `git-restore-mtime`-style
step in CI; registry manifest digests are unaffected, only tags churn).

Exclusions come from `.amberignore` files at or below the build path — put a
`.amberignore` inside each build path; one at the monorepo root is not
consulted for subtree builds. Don't define jobs-build image objects via YAML
anchors/aliases or `<<` merge keys — the template scanner rejects them.

Files without substitutions are copied byte-identical; substituted files keep
their comments.

## Ownership markers

Every file assimilate publishes carries a checksum of its content: YAML files
as a `# assimilate-hash: <sha256>` first line, JSON files in a committed
sidecar named `<file>.json.assimilate`. On the next deploy, assimilate only
overwrites files whose marker is present and still matches — a file somebody
created or edited by hand is a conflict, listed in the error, and nothing is
pushed. `deploy --force` overwrites conflicting files anyway (each one is
logged). Files assimilate no longer renders are never pruned.

## Environment config (`assimilate-templates/<env>/assimilate.yaml`)

```yaml
git:
  github:                            # the key selects the provider
    repo: my-org/gitops
    path: clusters/staging           # directory within the repo
    branch: main                     # optional; base branch (default: repo default)

registry: localhost:5000             # optional; image ref prefix (default localhost:5000)

argocd:                              # optional; application URLs as copied from the ArgoCD UI
  - url: https://argocd.example.com/applications/argocd/my-app
```

## Credentials (environment variables only)

| Variable | Used for |
|---|---|
| `JOBS_SERVER` | jobs-iroh server endpoint ID |
| `JOBS_SERVER_ADDR` | optional direct `host:port` (skips discovery; comma-separable) |
| `GITHUB_TOKEN` / `GH_TOKEN` | GitOps repo push + PR create/merge; falls back to `gh auth token` when unset |
| `ARGOCD_AUTH_TOKEN` / `ARGOCD_TOKEN` | ArgoCD API (`--rollout`) |
| `ARGOCD_INSECURE=true` | skip TLS verification towards ArgoCD |
| `ASSIMILATE_DATA_DIR` | local store dir (default `~/.local/share/assimilate`) |

## What `deploy` does

1. Scan the environment's templates, extract and dedupe the build specs.
2. Ingest each source dir into a local content-addressed store, push it to
   the server (delta), and run all builds concurrently — the image tag K is
   known before the build even starts.
3. When everything is `done`, render the manifests and publish them under the
   configured path of the GitOps repo: branch `assimilate/<env>-<timestamp>`,
   commit listing every image, PR against the base branch.
4. Without `--rollout`: print the PR URL and stop. With `--rollout`:
   squash-merge the PR, delete the branch, then refresh + sync each
   configured ArgoCD application.

No manifest changes → no PR (a `--rollout` still triggers the ArgoCD
refresh/sync). Build outputs are never pulled locally: the cluster pulls from
a jobs-registry pointed at the same server.

## Development

```sh
go build ./...
go test -race ./...
```

See [docs/design.md](docs/design.md) for the architecture.

## License

[GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0-only).
