// Package project locates the monorepo root and loads per-environment
// configuration from assimilate-templates/<env>/assimilate.yaml.
package project

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jobs-build/assimilate/internal/spec"
)

// TemplatesDir is the directory whose presence marks the project root.
const TemplatesDir = "assimilate-templates"

// ConfigFile is the per-environment config file name (not a template).
const ConfigFile = "assimilate.yaml"

// defaultRegistry is the image ref host prefix when the config omits one.
const defaultRegistry = "localhost:5000"

// FindRoot walks up from `from` until it finds a directory containing
// TemplatesDir, returning that directory's absolute path. It returns an
// error describing the search when no root exists.
func FindRoot(from string) (string, error) {
	start, err := filepath.Abs(from)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", from, err)
	}
	for dir := start; ; {
		if fi, err := os.Stat(filepath.Join(dir, TemplatesDir)); err == nil && fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s directory found in %s or any parent directory", TemplatesDir, start)
		}
		dir = parent
	}
}

// EnvDir returns the template directory of one environment and verifies it
// exists, listing available environments in the error when it does not.
func EnvDir(root, env string) (string, error) {
	if env == "" || env == "." || env == ".." || env != filepath.Base(env) {
		return "", fmt.Errorf("invalid environment name %q", env)
	}
	dir := filepath.Join(root, TemplatesDir, env)
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir, nil
	}
	avail := listEnvs(filepath.Join(root, TemplatesDir))
	if len(avail) == 0 {
		return "", fmt.Errorf("environment %q not found: %s contains no environments", env, filepath.Join(root, TemplatesDir))
	}
	return "", fmt.Errorf("environment %q not found; available: %s", env, strings.Join(avail, ", "))
}

// listEnvs returns the environment names (subdirectories of templatesDir),
// sorted; nil when the directory is unreadable or holds none.
func listEnvs(templatesDir string) []string {
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		return nil
	}
	var envs []string
	for _, e := range entries {
		if e.IsDir() {
			envs = append(envs, e.Name())
		}
	}
	sort.Strings(envs)
	return envs
}

// fileConfig mirrors assimilate.yaml exactly; KnownFields(true) turns any
// key outside this schema into a parse error.
type fileConfig struct {
	Git      *gitSection `yaml:"git"`
	Registry string      `yaml:"registry"`
	ArgoCD   []argoEntry `yaml:"argocd"`
}

// gitSection holds one provider key. Extra keys are rejected by strict
// decoding, so "exactly one provider" reduces to exactly one field set.
type gitSection struct {
	GitHub  *githubSection  `yaml:"github"`
	Forgejo *forgejoSection `yaml:"forgejo"`
}

type githubSection struct {
	Repo   string `yaml:"repo"`
	Path   string `yaml:"path"`
	Branch string `yaml:"branch"`
}

type forgejoSection struct {
	URL    string `yaml:"url"`
	Repo   string `yaml:"repo"`
	Path   string `yaml:"path"`
	Branch string `yaml:"branch"`
}

type argoEntry struct {
	URL string `yaml:"url"`
}

// LoadConfig reads and validates <envDir>/assimilate.yaml.
//
// Schema (see docs/design.md): a `git` mapping with exactly one provider key
// (`github` or `forgejo`) carrying repo (owner/name), path, optional branch,
// and — forgejo only — the instance base url; an optional `registry` (default
// localhost:5000); an optional `argocd` list of {url: <application URL>}
// entries parsed by ParseArgoURL. Unknown top level or provider keys are
// errors.
func LoadConfig(envDir string) (spec.Config, error) {
	file := filepath.Join(envDir, ConfigFile)
	data, err := os.ReadFile(file)
	if err != nil {
		return spec.Config{}, fmt.Errorf("reading config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var fc fileConfig
	if err := dec.Decode(&fc); err != nil && !errors.Is(err, io.EOF) {
		return spec.Config{}, fmt.Errorf("%s: %w", file, err)
	}

	cfg := spec.Config{Registry: fc.Registry}
	if cfg.Registry == "" {
		cfg.Registry = defaultRegistry
	}

	// git absent is allowed (render needs no repo; deploy errors later).
	if fc.Git != nil {
		g, err := gitConfig(fc.Git)
		if err != nil {
			return spec.Config{}, fmt.Errorf("%s: %w", file, err)
		}
		cfg.Git = g
	}

	for i, e := range fc.ArgoCD {
		if e.URL == "" {
			return spec.Config{}, fmt.Errorf("%s: argocd[%d]: missing url", file, i)
		}
		app, err := ParseArgoURL(e.URL)
		if err != nil {
			return spec.Config{}, fmt.Errorf("%s: argocd[%d]: %w", file, i, err)
		}
		cfg.ArgoCD = append(cfg.ArgoCD, app)
	}
	return cfg, nil
}

// gitConfig validates the git section's single provider entry and folds it
// into the provider-neutral spec.GitConfig.
func gitConfig(g *gitSection) (spec.GitConfig, error) {
	switch {
	case g.GitHub != nil && g.Forgejo == nil:
		gh := g.GitHub
		if err := checkRepo(gh.Repo); err != nil {
			return spec.GitConfig{}, fmt.Errorf("git.github.repo: %w", err)
		}
		cleaned, err := cleanRepoPath(gh.Path)
		if err != nil {
			return spec.GitConfig{}, fmt.Errorf("git.github.path: %w", err)
		}
		return spec.GitConfig{Type: "github", Repo: gh.Repo, Path: cleaned, Branch: gh.Branch}, nil
	case g.Forgejo != nil && g.GitHub == nil:
		fj := g.Forgejo
		base, err := checkBaseURL(fj.URL)
		if err != nil {
			return spec.GitConfig{}, fmt.Errorf("git.forgejo.url: %w", err)
		}
		if err := checkRepo(fj.Repo); err != nil {
			return spec.GitConfig{}, fmt.Errorf("git.forgejo.repo: %w", err)
		}
		cleaned, err := cleanRepoPath(fj.Path)
		if err != nil {
			return spec.GitConfig{}, fmt.Errorf("git.forgejo.path: %w", err)
		}
		return spec.GitConfig{Type: "forgejo", URL: base, Repo: fj.Repo, Path: cleaned, Branch: fj.Branch}, nil
	default:
		return spec.GitConfig{}, errors.New("git must contain exactly one provider key (github or forgejo)")
	}
}

// checkBaseURL requires an absolute http(s) URL with a host — the instance
// both the API and the git remote live on — and trims a trailing slash.
func checkBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not an http(s) URL", raw)
	}
	return strings.TrimSuffix(raw, "/"), nil
}

// checkRepo requires the owner/name form: exactly one slash, both sides
// non-empty.
func checkRepo(repo string) error {
	if repo == "" {
		return errors.New("required")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("%q is not of the form owner/name", repo)
	}
	return nil
}

// cleanRepoPath normalizes a repo-relative directory: slash-cleaned, no
// leading slash, "" for the repo root; paths escaping the root are errors.
func cleanRepoPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	c := path.Clean(strings.TrimLeft(p, "/"))
	if c == "." {
		return "", nil
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%q escapes the repository root", p)
	}
	return c, nil
}

// ParseArgoURL parses an ArgoCD application URL as copied from the UI —
// https://<server>/applications/<name> or
// https://<server>/applications/<namespace>/<name> — into an ArgoApp.
// <server> may carry a path prefix (rootpath-hosted UIs, e.g.
// https://host/argocd/applications/my-app → Server https://host/argocd);
// the prefix ends at the last /applications/ segment.
func ParseArgoURL(raw string) (spec.ArgoApp, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return spec.ArgoApp{}, fmt.Errorf("invalid ArgoCD URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return spec.ArgoApp{}, fmt.Errorf("invalid ArgoCD URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return spec.ArgoApp{}, fmt.Errorf("invalid ArgoCD URL %q: missing host", raw)
	}
	// Trailing slashes and query/fragment are UI noise. The app follows the
	// LAST "applications" segment so a UI path prefix (rootpath-hosted
	// ArgoCD) can never be mistaken for it.
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	last := -1 // index of the last "applications" segment
	for i, s := range segs {
		if s == "applications" {
			last = i
		}
	}
	if tail := len(segs) - last - 1; last < 0 || tail < 1 || tail > 2 {
		return spec.ArgoApp{}, fmt.Errorf("invalid ArgoCD URL %q: path must end in /applications/<name> or /applications/<namespace>/<name>", raw)
	}
	for _, s := range segs {
		if s == "" {
			return spec.ArgoApp{}, fmt.Errorf("invalid ArgoCD URL %q: empty path segment", raw)
		}
	}
	app := spec.ArgoApp{Server: u.Scheme + "://" + u.Host}
	if last > 0 {
		app.Server += "/" + strings.Join(segs[:last], "/")
	}
	rest := segs[last+1:]
	if len(rest) == 1 {
		app.Name = rest[0]
	} else {
		app.Namespace, app.Name = rest[0], rest[1]
	}
	return app, nil
}
