// Package spec holds the shared vocabulary of assimilate: build specs
// extracted from templates, build lifecycle states, UI events, and the
// per-environment configuration. Every other package meets here; none of
// them import each other's internals.
package spec

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jobs-build/jobs-iroh/api"
)

// BuildSpec is one jobs-build image object extracted from a template.
type BuildSpec struct {
	Name      string            // display name; empty = derived by DisplayName
	Path      string            // source dir relative to the project root; "/" = the root
	BuildFile string            // recipe path relative to Path; "" = BUILD.jobs
	Args      map[string]string // build params (jobs --param key=value)
	Platform  string            // required, e.g. linux/amd64
}

// Key is the canonical dedupe identity of a spec: two template objects with
// equal keys are the same build (built once, substituted everywhere). Every
// component is length-prefixed, so the encoding is injective — no crafted arg
// value can collide with a different args map; args appear in sorted key
// order, so map order never matters.
func (s BuildSpec) Key() string {
	keys := make([]string, 0, len(s.Args))
	for k := range s.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	comp := func(c string) { fmt.Fprintf(&b, "%d:%s", len(c), c) }
	comp(s.Path)
	comp(s.BuildFile)
	comp(s.Platform)
	for _, k := range keys {
		comp(k)
		comp(s.Args[k])
	}
	return b.String()
}

// DisplayName is the TUI/CLI label: the explicit name, or "<path> <platform>".
func (s BuildSpec) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Path + " " + s.Platform
}

// BuildState is the lifecycle of one build as shown to the user.
type BuildState string

const (
	StatePending   BuildState = "pending"   // not started yet
	StatePushing   BuildState = "pushing"   // source ingest + push to the server
	StateBuilding  BuildState = "building"  // submitted, watch stream running
	StateDone      BuildState = "done"      // terminal: success
	StateFailed    BuildState = "failed"    // terminal: failure
	StateCancelled BuildState = "cancelled" // terminal: cancelled
)

// Terminal reports whether the state is final.
func (s BuildState) Terminal() bool {
	return s == StateDone || s == StateFailed || s == StateCancelled
}

// EventKind discriminates Event.
type EventKind int

const (
	// KindState: Build entered State; Info optionally carries detail
	// (an error summary on failure).
	KindState EventKind = iota
	// KindLog: Line is one log line of Build's output. Node ("" = the
	// build itself) names the graph node the line belongs to, so the TUI
	// can bucket output per node while the combined view prefixes it.
	KindLog
	// KindInfo: Info is a transient progress note (push progress, counts);
	// shown in the build's status column, not appended to the log.
	KindInfo
	// KindSnapshot: Snap is Build's latest coalesced watch snapshot (the
	// raw jobs-iroh closure incl. NodeSnap.Deps/Cached) — the TUI folds it
	// into the image's build-graph subtree. Plain mode ignores it.
	KindSnapshot
)

// Event is one UI-consumable happening. Build indexes the ordered build
// list handed to the UI; -1 means global.
type Event struct {
	Build int
	Kind  EventKind
	State BuildState
	Line  string
	Info  string
	Node  string        // KindLog: source node name ("" = build-level)
	Snap  *api.Snapshot // KindSnapshot only
}

// Config is one environment's assimilate.yaml.
type Config struct {
	Git      GitConfig
	Registry string // image ref host prefix; default "localhost:5000"
	ArgoCD   []ArgoApp
	// Domain is the mandatory assimilate-domain: the name of this source
	// repository, recorded in every generated file so pruning in a shared
	// GitOps directory only ever touches this repository's own files.
	Domain string
}

// GitConfig describes the GitOps repository to publish rendered manifests to.
type GitConfig struct {
	Type   string // provider, from the config key: "github" or "forgejo"
	URL    string // forgejo only: instance base URL, e.g. https://git.example.com
	Repo   string // owner/name
	Path   string // directory within the repo to write rendered files under
	Branch string // base branch for PRs; "" = repository default branch
}

// ArgoApp is one ArgoCD application to refresh/sync on rollout.
type ArgoApp struct {
	Server    string // base URL, e.g. https://argocd.example.com
	Namespace string // application namespace; may be empty
	Name      string
}

// SourceDir resolves a spec path ("/"-rooted, forward slashes) under the
// project root.
func SourceDir(root, p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(p))
}

// ImageRef renders the pullable image reference of a finished build:
// "<registry>/jobs:<K>" — tag form; the jobs-registry serves one repository
// named "jobs" whose tags are build keys.
func ImageRef(registry, k string) string {
	return registry + "/jobs:" + k
}
