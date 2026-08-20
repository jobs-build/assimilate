// Package gitops publishes rendered manifests to the GitOps repository:
// shallow clone, branch, write, commit, push, PR — and with rollout, PR
// merge. Provider support is keyed by spec.GitConfig.Type: "github"
// (go-github) and "forgejo" (the Forgejo SDK); the git half is go-git over
// token-authenticated HTTPS for both.
package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	forgejo "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v2"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/google/go-github/v73/github"

	"github.com/jobs-build/assimilate/internal/ownership"
	"github.com/jobs-build/assimilate/internal/spec"
)

// Change is one publication: rendered files (paths relative to
// cfg.Path in the repo) plus the human story for branch/commit/PR.
type Change struct {
	Env     string            // environment name, used in branch name and titles
	Message string            // commit message and PR body core (image list)
	Files   map[string][]byte // repo files under cfg.Path
	// Force overwrites files that fail the ownership check — files without
	// an assimilate marker or edited since assimilate wrote them. Without
	// Force such files are conflicts and the publication is refused.
	Force bool
}

// Result reports what happened.
type Result struct {
	Branch    string
	PRURL     string
	PRNumber  int
	NoChanges bool // rendered files were identical to the base branch — no PR
	Merged    bool // rollout: PR squash-merged (branch deleted)
}

// Test seams. cloneURL lets the git half target a local upstream (a local
// path yields nil auth); apiClient lets the GitHub PR half target an
// httptest server via WithEnterpriseURLs (the Forgejo half needs no seam —
// its base URL already comes from the config); now pins the branch
// timestamp; mergeBackoff is the retry schedule for the not-yet-mergeable
// window.
var (
	cloneURL = func(cfg spec.GitConfig) string {
		if cfg.Type == "forgejo" {
			return cfg.URL + "/" + cfg.Repo + ".git"
		}
		return "https://github.com/" + cfg.Repo + ".git"
	}
	apiClient = func(token string) (*github.Client, error) {
		return github.NewClient(nil).WithAuthToken(token), nil
	}
	now          = func() time.Time { return time.Now().UTC() }
	mergeBackoff = []time.Duration{time.Second, 2 * time.Second, 2 * time.Second}
)

// Publish clones cfg.Repo (depth 1, base branch), writes ch.Files under
// cfg.Path with ownership markers (only files assimilate generated before
// are overwritten — see writeFiles; strays are never pruned), commits on
// branch assimilate/<env>-<timestamp>, pushes, opens a PR to the base
// branch, and with rollout squash-merges it and deletes the branch.
// token authenticates both the git remote (x-access-token basic auth) and
// the provider API. log receives one-line progress notes.
func Publish(ctx context.Context, cfg spec.GitConfig, token string, ch Change, rollout bool, log func(string)) (Result, error) {
	if err := validateGit(cfg); err != nil {
		return Result{}, err
	}
	branch, noChanges, err := pushBranch(ctx, cfg, token, ch, log)
	if err != nil {
		return Result{}, err
	}
	if noChanges {
		log("no manifest changes against " + baseLabel(cfg))
		return Result{NoChanges: true}, nil
	}
	return openPR(ctx, cfg, token, ch, branch, rollout, log)
}

// pushBranch is the git half: shallow-clone, write ch.Files under cfg.Path,
// and — unless the tree is unchanged — commit on a fresh
// assimilate/<env>-<timestamp> branch and push it.
func pushBranch(ctx context.Context, cfg spec.GitConfig, token string, ch Change, log func(string)) (branch string, noChanges bool, err error) {
	dir, err := os.MkdirTemp("", "assimilate-gitops-*")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)

	url := cloneURL(cfg)
	auth := gitAuth(url, token)
	opts := &git.CloneOptions{
		URL:          url,
		Auth:         auth,
		Depth:        1,
		SingleBranch: true,
	}
	if cfg.Branch != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(cfg.Branch)
	}
	log("cloning " + cfg.Repo + " (" + baseLabel(cfg) + ")")
	repo, err := git.PlainCloneContext(ctx, dir, false, opts)
	if err != nil {
		return "", false, fmt.Errorf("clone %s: %w", cfg.Repo, err)
	}

	if err := writeFiles(dir, cfg, ch, log); err != nil {
		return "", false, err
	}

	wt, err := repo.Worktree()
	if err != nil {
		return "", false, err
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return "", false, err
	}
	st, err := wt.Status()
	if err != nil {
		return "", false, err
	}
	if st.IsClean() {
		return "", true, nil
	}

	branch = "assimilate/" + ch.Env + "-" + now().UTC().Format("20060102-150405")
	// Keep preserves the staged files across the branch switch.
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch),
		Create: true,
		Keep:   true,
	}); err != nil {
		return "", false, fmt.Errorf("checkout %s: %w", branch, err)
	}
	sig := &object.Signature{
		Name:  "assimilate",
		Email: "assimilate@users.noreply.github.com",
		When:  time.Now(),
	}
	if _, err := wt.Commit(ch.Message, &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		return "", false, fmt.Errorf("commit: %w", err)
	}
	refspec := gitconfig.RefSpec("refs/heads/" + branch + ":refs/heads/" + branch)
	if err := repo.PushContext(ctx, &git.PushOptions{RefSpecs: []gitconfig.RefSpec{refspec}, Auth: auth}); err != nil {
		return "", false, fmt.Errorf("push %s: %w", branch, err)
	}
	log("pushed " + branch)
	return branch, false, nil
}

// writeFiles writes ch.Files into the clone at dir under cfg.Path via
// ownership markers: YAML files gain a first-line hash comment, JSON files a
// hash sidecar. A destination that already exists but carries no marker (not
// generated by assimilate) or whose body no longer matches its marker (edited
// since) is a conflict: without ch.Force the whole publication is refused
// listing every conflict; with ch.Force the file is overwritten with a log
// note.
func writeFiles(dir string, cfg spec.GitConfig, ch Change, log func(string)) error {
	rels := make([]string, 0, len(ch.Files))
	for rel := range ch.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	var conflicts []string
	for _, rel := range rels {
		repoPath := path.Join(filepath.ToSlash(cfg.Path), rel)
		dst := filepath.Join(dir, filepath.FromSlash(cfg.Path), filepath.FromSlash(rel))
		st, err := ownership.Status(dst)
		if err != nil {
			return err
		}
		if st.Exists && (!st.Owned || !st.Matches) {
			reason := "no assimilate marker"
			if st.Owned {
				reason = "edited since assimilate generated it"
			}
			if !ch.Force {
				conflicts = append(conflicts, "  "+repoPath+": "+reason)
				continue
			}
			log("overwriting " + repoPath + " (" + reason + ")")
		}
		if err := ownership.WriteMarked(dst, ch.Files[rel]); err != nil {
			return err
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("refusing to overwrite files not generated by assimilate (re-run with --force to overwrite):\n%s",
			strings.Join(conflicts, "\n"))
	}
	return nil
}

// validateGit rejects configs Publish cannot serve, before any git work
// happens: unknown providers, malformed repos, a forgejo config without its
// instance URL.
func validateGit(cfg spec.GitConfig) error {
	switch cfg.Type {
	case "github":
	case "forgejo":
		if cfg.URL == "" {
			return errors.New("gitops: forgejo requires the instance URL")
		}
	default:
		return fmt.Errorf("gitops: unsupported provider %q (github or forgejo)", cfg.Type)
	}
	_, _, err := splitRepo(cfg.Repo)
	return err
}

// provider is the API half of one GitOps host, hiding the SDK differences
// behind the four calls openPR needs.
type provider interface {
	defaultBranch(ctx context.Context) (string, error)
	createPR(ctx context.Context, title, head, base, body string) (prURL string, number int, err error)
	// tryMerge attempts one squash-merge; retry reports whether the failure
	// is the transient not-yet-mergeable window worth another attempt.
	tryMerge(ctx context.Context, number int) (retry bool, err error)
	deleteBranch(ctx context.Context, branch string) error
}

// newProvider builds the API client for cfg. The Forgejo SDK probes the
// server version at construction and carries ctx for every later call.
func newProvider(ctx context.Context, cfg spec.GitConfig, token string) (provider, error) {
	owner, name, err := splitRepo(cfg.Repo)
	if err != nil {
		return nil, err
	}
	if cfg.Type == "forgejo" {
		client, err := forgejo.NewClient(cfg.URL, forgejo.SetToken(token), forgejo.SetContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("forgejo client for %s: %w", cfg.URL, err)
		}
		return &forgejoProvider{client: client, owner: owner, name: name}, nil
	}
	client, err := apiClient(token)
	if err != nil {
		return nil, err
	}
	return &githubProvider{client: client, owner: owner, name: name}, nil
}

// openPR is the provider half: open the PR against the base branch and, on
// rollout, squash-merge it (retrying the not-yet-mergeable window) and
// best-effort delete the branch.
func openPR(ctx context.Context, cfg spec.GitConfig, token string, ch Change, branch string, rollout bool, log func(string)) (Result, error) {
	p, err := newProvider(ctx, cfg, token)
	if err != nil {
		return Result{}, err
	}

	base := cfg.Branch
	if base == "" {
		if base, err = p.defaultBranch(ctx); err != nil {
			return Result{}, fmt.Errorf("resolve default branch of %s: %w", cfg.Repo, err)
		}
	}

	prURL, number, err := p.createPR(ctx, "assimilate: deploy "+ch.Env, branch, base, ch.Message)
	if err != nil {
		return Result{}, fmt.Errorf("create PR for %s: %w", branch, err)
	}
	res := Result{Branch: branch, PRURL: prURL, PRNumber: number}
	log(fmt.Sprintf("PR #%d opened", number))
	if !rollout {
		return res, nil
	}

	if err := mergePR(ctx, p, number); err != nil {
		return res, fmt.Errorf("merge PR #%d: %w", number, err)
	}
	res.Merged = true
	log(fmt.Sprintf("merged PR #%d", number))
	if err := p.deleteBranch(ctx, branch); err != nil {
		log("branch delete failed (ignored): " + err.Error())
	}
	return res, nil
}

// mergePR squash-merges via the provider, retrying the transient
// not-yet-mergeable window a fresh PR reports while its mergeability is
// still being computed.
func mergePR(ctx context.Context, p provider, number int) error {
	for attempt := 0; ; attempt++ {
		retry, err := p.tryMerge(ctx, number)
		if err == nil || attempt >= len(mergeBackoff) || !retry {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(mergeBackoff[attempt]):
		}
	}
}

// githubProvider is the GitHub API half, via go-github.
type githubProvider struct {
	client      *github.Client
	owner, name string
}

func (p *githubProvider) defaultBranch(ctx context.Context) (string, error) {
	r, _, err := p.client.Repositories.Get(ctx, p.owner, p.name)
	if err != nil {
		return "", err
	}
	return r.GetDefaultBranch(), nil
}

func (p *githubProvider) createPR(ctx context.Context, title, head, base, body string) (string, int, error) {
	pr, _, err := p.client.PullRequests.Create(ctx, p.owner, p.name, &github.NewPullRequest{
		Title: github.Ptr(title),
		Head:  github.Ptr(head),
		Base:  github.Ptr(base),
		Body:  github.Ptr(body),
	})
	if err != nil {
		return "", 0, err
	}
	return pr.GetHTMLURL(), pr.GetNumber(), nil
}

func (p *githubProvider) tryMerge(ctx context.Context, number int) (bool, error) {
	_, _, err := p.client.PullRequests.Merge(ctx, p.owner, p.name, number, "",
		&github.PullRequestOptions{MergeMethod: "squash"})
	return notYetMergeable(err), err
}

func (p *githubProvider) deleteBranch(ctx context.Context, branch string) error {
	_, err := p.client.Git.DeleteRef(ctx, p.owner, p.name, "heads/"+branch)
	return err
}

// notYetMergeable reports the transient merge statuses worth retrying.
func notYetMergeable(err error) bool {
	var er *github.ErrorResponse
	if !errors.As(err, &er) || er.Response == nil {
		return false
	}
	return er.Response.StatusCode == 405 || er.Response.StatusCode == 409
}

// forgejoProvider is the Forgejo API half, via the Forgejo SDK. The SDK
// carries the context given at construction, so the per-call ctx arguments
// are unused; its calls surface HTTP failures as statuses, not errors, so
// the helpers below turn a non-2xx status into one.
type forgejoProvider struct {
	client      *forgejo.Client
	owner, name string
}

func (p *forgejoProvider) defaultBranch(context.Context) (string, error) {
	r, _, err := p.client.GetRepo(p.owner, p.name)
	if err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

func (p *forgejoProvider) createPR(_ context.Context, title, head, base, body string) (string, int, error) {
	pr, _, err := p.client.CreatePullRequest(p.owner, p.name, forgejo.CreatePullRequestOption{
		Title: title,
		Head:  head,
		Base:  base,
		Body:  body,
	})
	if err != nil {
		return "", 0, err
	}
	return pr.HTMLURL, int(pr.Index), nil
}

func (p *forgejoProvider) tryMerge(_ context.Context, number int) (bool, error) {
	merged, resp, err := p.client.MergePullRequest(p.owner, p.name, int64(number),
		forgejo.MergePullRequestOption{Style: forgejo.MergeStyleSquash})
	if err != nil || merged {
		return false, err
	}
	status := statusOf(resp)
	return status == 405 || status == 409, fmt.Errorf("server returned status %d", status)
}

func (p *forgejoProvider) deleteBranch(_ context.Context, branch string) error {
	ok, resp, err := p.client.DeleteRepoBranch(p.owner, p.name, branch)
	if err != nil || ok {
		return err
	}
	return fmt.Errorf("server returned status %d", statusOf(resp))
}

func statusOf(resp *forgejo.Response) int {
	if resp == nil || resp.Response == nil {
		return 0
	}
	return resp.StatusCode
}

// splitRepo splits "owner/name", rejecting anything else.
func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("gitops: repo %q is not owner/name", repo)
	}
	return owner, name, nil
}

// gitAuth returns token auth for remote URLs and nil for local paths (the
// test seam: a local upstream needs no credentials).
func gitAuth(url, token string) transport.AuthMethod {
	if token != "" && (strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")) {
		return &githttp.BasicAuth{Username: "x-access-token", Password: token}
	}
	return nil
}

// baseLabel names the base branch for log lines.
func baseLabel(cfg spec.GitConfig) string {
	if cfg.Branch != "" {
		return cfg.Branch
	}
	return "default branch"
}
