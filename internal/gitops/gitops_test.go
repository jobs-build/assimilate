package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-github/v73/github"

	"github.com/jobs-build/assimilate/internal/ownership"
	"github.com/jobs-build/assimilate/internal/spec"
)

// markedAs returns body as ownership.WriteMarked stores a YAML file for
// domain: the hash line, the domain line (omitted for "", the pre-domain
// format) and the body.
func markedAs(domain, body string) string {
	hdr := ownership.MarkerPrefix + ownership.ComputeBodyHash("x.yaml", []byte(body)) + "\n"
	if domain != "" {
		hdr += ownership.DomainPrefix + domain + "\n"
	}
	return hdr + body
}

// marked is markedAs for testChange's domain.
func marked(body string) string { return markedAs("mono", body) }

var testStamp = time.Date(2026, 7, 24, 12, 30, 45, 0, time.UTC)

const testBranch = "assimilate/staging-20260724-123045"

// stubGit points the git half at a local bare upstream and pins the branch
// timestamp. Restores on cleanup; tests using seams must not be parallel.
func stubGit(t *testing.T, upstream string) {
	t.Helper()
	origURL, origNow := cloneURL, now
	t.Cleanup(func() { cloneURL, now = origURL, origNow })
	cloneURL = func(spec.GitConfig) string { return upstream }
	now = func() time.Time { return testStamp }
}

// stubAPI points the GitHub PR half at an httptest server via enterprise
// URLs and collapses the merge retry backoff.
func stubAPI(t *testing.T, srv *httptest.Server) {
	t.Helper()
	origAPI := apiClient
	t.Cleanup(func() { apiClient = origAPI })
	apiClient = func(string) (*github.Client, error) {
		return github.NewClient(nil).WithEnterpriseURLs(srv.URL, srv.URL)
	}
	stubBackoff(t)
}

// stubBackoff collapses the merge retry backoff.
func stubBackoff(t *testing.T) {
	t.Helper()
	origBackoff := mergeBackoff
	t.Cleanup(func() { mergeBackoff = origBackoff })
	mergeBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
}

var seedSig = &object.Signature{Name: "seed", Email: "seed@example.com", When: time.Now()}

// seedUpstream builds a bare repo whose main branch carries files on top of
// an earlier seed commit, so depth-1 clones are genuinely shallow.
func seedUpstream(t *testing.T, files map[string]string) string {
	t.Helper()
	bare := t.TempDir()
	if _, err := git.PlainInitWithOptions(bare, &git.PlainInitOptions{
		Bare:        true,
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	}); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	repo, err := git.PlainInitWithOptions(work, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(msg string, files map[string]string) {
		for rel, content := range files {
			dst := filepath.Join(work, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Commit(msg, &git.CommitOptions{Author: seedSig, Committer: seedSig}); err != nil {
			t.Fatal(err)
		}
	}
	commit("seed", map[string]string{".seed": "1\n"})
	commit("manifests", files)
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{
		RefSpecs: []gitconfig.RefSpec{"refs/heads/main:refs/heads/main"},
	}); err != nil {
		t.Fatal(err)
	}
	return bare
}

// branchTip returns the tip commit of branch in the bare upstream and its
// full tree as path→content.
func branchTip(t *testing.T, bare, branch string) (*object.Commit, map[string]string) {
	t.Helper()
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("branch %s: %v", branch, err)
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := c.Tree()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	err = tree.Files().ForEach(func(f *object.File) error {
		s, err := f.Contents()
		files[f.Name] = s
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, files
}

// branchNames lists refs/heads/* of the bare upstream.
func branchNames(t *testing.T, bare string) []string {
	t.Helper()
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	iter, err := repo.References()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsBranch() {
			names = append(names, ref.Name().Short())
		}
		return nil
	})
	return names
}

func testChange() Change {
	return Change{
		Env:     "staging",
		Domain:  "mono",
		Message: "assimilate: deploy staging\n\nbackend → localhost:5000/jobs:abc\n",
		Files: map[string][]byte{
			"api.yaml":           []byte("kind: Deployment\nimage: new\n"),
			"workers/queue.yaml": []byte("kind: Job\n"),
		},
	}
}

func discard(string) {}

func TestPushBranch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfgBranch string
	}{
		{"explicit base branch", "main"},
		{"default branch via HEAD", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := seedUpstream(t, map[string]string{
				"README.md":                  "hi\n",
				"clusters/staging/api.yaml":  marked("kind: Deployment\nimage: old\n"),
				"clusters/staging/keep.yaml": "stray\n",
			})
			stubGit(t, upstream)
			cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: tc.cfgBranch}
			ch := testChange()

			mainTip, _ := branchTip(t, upstream, "main")
			var logs []string
			branch, noChanges, err := pushBranch(context.Background(), cfg, "", ch, func(s string) { logs = append(logs, s) })
			if err != nil {
				t.Fatal(err)
			}
			if noChanges {
				t.Fatal("unexpected noChanges")
			}
			if branch != testBranch {
				t.Fatalf("branch = %q, want %q", branch, testBranch)
			}

			c, files := branchTip(t, upstream, branch)
			for path, want := range map[string]string{
				"clusters/staging/api.yaml":           marked(string(ch.Files["api.yaml"])),
				"clusters/staging/workers/queue.yaml": marked(string(ch.Files["workers/queue.yaml"])),
				"clusters/staging/keep.yaml":          "stray\n", // unmarked strays are never pruned
				"README.md":                           "hi\n",    // files outside cfg.Path untouched
			} {
				if got := files[path]; got != want {
					t.Errorf("%s = %q, want %q", path, got, want)
				}
			}
			if c.Message != ch.Message {
				t.Errorf("commit message = %q, want %q", c.Message, ch.Message)
			}
			if c.Author.Name != "assimilate" || c.Author.Email != "assimilate@users.noreply.github.com" {
				t.Errorf("author = %s <%s>", c.Author.Name, c.Author.Email)
			}
			if c.NumParents() != 1 || c.ParentHashes[0] != mainTip.Hash {
				t.Errorf("parents = %v, want [%s]", c.ParentHashes, mainTip.Hash)
			}
			// base branch itself must be untouched
			if tip, _ := branchTip(t, upstream, "main"); tip.Hash != mainTip.Hash {
				t.Errorf("main moved: %s -> %s", mainTip.Hash, tip.Hash)
			}
			if len(logs) < 2 || !strings.HasPrefix(logs[0], "cloning acme/gitops") || logs[len(logs)-1] != "pushed "+testBranch {
				t.Errorf("logs = %q", logs)
			}
		})
	}
}

// Publishing the same content twice: once the first branch is merged into
// main, a re-publish must short-circuit with no branch or push.
func TestPushBranchNoChanges(t *testing.T) {
	upstream := seedUpstream(t, map[string]string{"clusters/staging/api.yaml": marked("old\n")})
	stubGit(t, upstream)
	cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}
	ch := testChange()

	branch, noChanges, err := pushBranch(context.Background(), cfg, "", ch, discard)
	if err != nil || noChanges {
		t.Fatalf("first publish: branch=%q noChanges=%v err=%v", branch, noChanges, err)
	}

	// "merge" the PR: fast-forward upstream main to the pushed commit.
	c, _ := branchTip(t, upstream, branch)
	repo, err := git.PlainOpen(upstream)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), c.Hash)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(branch)); err != nil {
		t.Fatal(err)
	}

	branch2, noChanges2, err := pushBranch(context.Background(), cfg, "", ch, discard)
	if err != nil {
		t.Fatal(err)
	}
	if !noChanges2 || branch2 != "" {
		t.Fatalf("second publish: branch=%q noChanges=%v", branch2, noChanges2)
	}
	for _, name := range branchNames(t, upstream) {
		if strings.HasPrefix(name, "assimilate/") {
			t.Errorf("stray branch %s", name)
		}
	}
}

// A target file without a marker (not assimilate's) or with a stale marker
// (edited since generated) blocks the publication unless Force is set.
func TestPushBranchOwnershipConflicts(t *testing.T) {
	// queue.yaml carries a marker for a different body: edited after generation.
	tampered := ownership.MarkerPrefix + ownership.ComputeBodyHash("x.yaml", []byte("original\n")) + "\nedited by hand\n"
	seed := map[string]string{
		"clusters/staging/api.yaml":           "kind: Deployment\nimage: old\n", // no marker
		"clusters/staging/workers/queue.yaml": tampered,
	}

	t.Run("without force", func(t *testing.T) {
		upstream := seedUpstream(t, seed)
		stubGit(t, upstream)
		cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}

		_, _, err := pushBranch(context.Background(), cfg, "", testChange(), discard)
		if err == nil {
			t.Fatal("no error")
		}
		for _, want := range []string{
			"clusters/staging/api.yaml: no assimilate marker",
			"clusters/staging/workers/queue.yaml: edited since assimilate generated it",
			"--force",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
		for _, name := range branchNames(t, upstream) {
			if strings.HasPrefix(name, "assimilate/") {
				t.Errorf("branch %s pushed despite conflicts", name)
			}
		}
	})

	t.Run("with force", func(t *testing.T) {
		upstream := seedUpstream(t, seed)
		stubGit(t, upstream)
		cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}
		ch := testChange()
		ch.Force = true

		var logs []string
		branch, noChanges, err := pushBranch(context.Background(), cfg, "", ch, func(s string) { logs = append(logs, s) })
		if err != nil || noChanges {
			t.Fatalf("noChanges=%v err=%v", noChanges, err)
		}
		_, files := branchTip(t, upstream, branch)
		for path, want := range map[string]string{
			"clusters/staging/api.yaml":           marked(string(ch.Files["api.yaml"])),
			"clusters/staging/workers/queue.yaml": marked(string(ch.Files["workers/queue.yaml"])),
		} {
			if got := files[path]; got != want {
				t.Errorf("%s = %q, want %q", path, got, want)
			}
		}
		joined := strings.Join(logs, "\n")
		for _, want := range []string{
			"overwriting clusters/staging/api.yaml (no assimilate marker)",
			"overwriting clusters/staging/workers/queue.yaml (edited since assimilate generated it)",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("logs %q missing %q", logs, want)
			}
		}
	})
}

// JSON files are pushed verbatim with their hash in a committed sidecar; a
// re-publish of identical content is a no-change, and a JSON file present
// without its sidecar is a conflict.
func TestPushBranchJSONSidecar(t *testing.T) {
	body := `{"replicas": 2}` + "\n"
	hash := ownership.ComputeBodyHash("x.json", []byte(body))
	sidecar := ownership.MarkerPrefix + hash + "\n" + ownership.DomainPrefix + "mono\n"
	ch := Change{Env: "staging", Domain: "mono", Message: "m", Files: map[string][]byte{"config.json": []byte(body)}}
	cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}

	upstream := seedUpstream(t, map[string]string{"README.md": "hi\n"})
	stubGit(t, upstream)
	branch, noChanges, err := pushBranch(context.Background(), cfg, "", ch, discard)
	if err != nil || noChanges {
		t.Fatalf("noChanges=%v err=%v", noChanges, err)
	}
	_, files := branchTip(t, upstream, branch)
	if got := files["clusters/staging/config.json"]; got != body {
		t.Errorf("config.json = %q, want %q", got, body)
	}
	if got := files["clusters/staging/config.json"+ownership.SidecarExt]; got != sidecar {
		t.Errorf("sidecar = %q, want %q", got, sidecar)
	}

	t.Run("identical content is no-change", func(t *testing.T) {
		upstream := seedUpstream(t, map[string]string{
			"clusters/staging/config.json":                        body,
			"clusters/staging/config.json" + ownership.SidecarExt: sidecar,
		})
		stubGit(t, upstream)
		_, noChanges, err := pushBranch(context.Background(), cfg, "", ch, discard)
		if err != nil {
			t.Fatal(err)
		}
		if !noChanges {
			t.Error("expected noChanges")
		}
	})

	// A sidecar from before domains (the bare hash) still proves ownership;
	// the re-publish upgrades it to the header format.
	t.Run("legacy sidecar adopted", func(t *testing.T) {
		upstream := seedUpstream(t, map[string]string{
			"clusters/staging/config.json":                        body,
			"clusters/staging/config.json" + ownership.SidecarExt: hash + "\n",
		})
		stubGit(t, upstream)
		branch, noChanges, err := pushBranch(context.Background(), cfg, "", ch, discard)
		if err != nil || noChanges {
			t.Fatalf("noChanges=%v err=%v", noChanges, err)
		}
		if _, files := branchTip(t, upstream, branch); files["clusters/staging/config.json"+ownership.SidecarExt] != sidecar {
			t.Errorf("sidecar not upgraded: %q", files["clusters/staging/config.json"+ownership.SidecarExt])
		}
	})

	t.Run("missing sidecar is a conflict", func(t *testing.T) {
		upstream := seedUpstream(t, map[string]string{"clusters/staging/config.json": body})
		stubGit(t, upstream)
		_, _, err := pushBranch(context.Background(), cfg, "", ch, discard)
		if err == nil || !strings.Contains(err.Error(), "clusters/staging/config.json: no assimilate marker") {
			t.Fatalf("err = %v", err)
		}
	})
}

// ghCall is one recorded API request.
type ghCall struct {
	method, path string
	body         map[string]any
}

// fakeGitHub serves the canned go-github endpoints Publish uses.
type fakeGitHub struct {
	t             *testing.T
	defaultBranch string
	prNumber      int
	prURL         string
	mergeStatuses []int // per-attempt status; past the end → 200
	calls         []ghCall
}

func (f *fakeGitHub) merges() int {
	n := 0
	for _, c := range f.calls {
		if c.method == "PUT" && strings.HasSuffix(c.path, "/merge") {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) find(method, pathSuffix string) *ghCall {
	for i, c := range f.calls {
		if c.method == method && strings.HasSuffix(c.path, pathSuffix) {
			return &f.calls[i]
		}
	}
	return nil
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := ghCall{method: r.Method, path: r.URL.Path}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&call.body)
	}
	merges := f.merges()
	f.calls = append(f.calls, call)

	w.Header().Set("Content-Type", "application/json")
	p, m := r.URL.Path, r.Method
	switch {
	case m == "GET" && p == "/api/v3/repos/acme/gitops":
		fmt.Fprintf(w, `{"default_branch": %q}`, f.defaultBranch)
	case m == "POST" && p == "/api/v3/repos/acme/gitops/pulls":
		fmt.Fprintf(w, `{"number": %d, "html_url": %q}`, f.prNumber, f.prURL)
	case m == "PUT" && p == fmt.Sprintf("/api/v3/repos/acme/gitops/pulls/%d/merge", f.prNumber):
		if merges < len(f.mergeStatuses) && f.mergeStatuses[merges] != 200 {
			w.WriteHeader(f.mergeStatuses[merges])
			fmt.Fprint(w, `{"message": "Pull Request is not mergeable"}`)
			return
		}
		fmt.Fprint(w, `{"sha": "deadbeef", "merged": true}`)
	case m == "DELETE" && strings.HasPrefix(p, "/api/v3/repos/acme/gitops/git/refs/heads/"):
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected request %s %s", m, p)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message": "not found"}`)
	}
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{t: t, defaultBranch: "main", prNumber: 12, prURL: "https://github.com/acme/gitops/pull/12"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func TestOpenPR(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfgBranch string
		wantBase  string
		wantGets  int // repo lookups to resolve the default branch
	}{
		{"explicit base", "main", "main", 0},
		{"default branch resolved", "", "trunk", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeGitHub(t)
			f.defaultBranch = "trunk"
			stubAPI(t, srv)
			cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: tc.cfgBranch}
			ch := testChange()

			var logs []string
			res, err := openPR(context.Background(), cfg, "tok", ch, testBranch, false, func(s string) { logs = append(logs, s) })
			if err != nil {
				t.Fatal(err)
			}
			want := Result{Branch: testBranch, PRURL: f.prURL, PRNumber: 12}
			if res != want {
				t.Fatalf("res = %+v, want %+v", res, want)
			}

			gets := 0
			for _, c := range f.calls {
				if c.method == "GET" {
					gets++
				}
			}
			if gets != tc.wantGets {
				t.Errorf("repo GETs = %d, want %d", gets, tc.wantGets)
			}
			create := f.find("POST", "/pulls")
			if create == nil {
				t.Fatalf("no PR create call in %+v", f.calls)
			}
			for k, want := range map[string]string{
				"title": "assimilate: deploy staging",
				"head":  testBranch,
				"base":  tc.wantBase,
				"body":  ch.Message,
			} {
				if got := create.body[k]; got != want {
					t.Errorf("create body %s = %v, want %q", k, got, want)
				}
			}
			if n := f.merges(); n != 0 {
				t.Errorf("merge calls = %d without rollout", n)
			}
			if len(logs) != 1 || logs[0] != "PR #12 opened" {
				t.Errorf("logs = %q", logs)
			}
		})
	}
}

func TestOpenPRRollout(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mergeStatuses []int
		wantMerges    int
		wantErr       bool
	}{
		{"merges immediately", nil, 1, false},
		{"retries 409 then 405", []int{409, 405, 200}, 3, false},
		{"gives up after retries", []int{409, 409, 409, 409}, 4, true},
		{"non-retryable status", []int{403}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeGitHub(t)
			f.mergeStatuses = tc.mergeStatuses
			stubAPI(t, srv)
			cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}

			var logs []string
			res, err := openPR(context.Background(), cfg, "tok", testChange(), testBranch, true, func(s string) { logs = append(logs, s) })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if n := f.merges(); n != tc.wantMerges {
				t.Errorf("merge attempts = %d, want %d", n, tc.wantMerges)
			}
			merge := f.find("PUT", "/merge")
			if merge == nil {
				t.Fatal("no merge call")
			}
			if got := merge.body["merge_method"]; got != "squash" {
				t.Errorf("merge_method = %v, want squash", got)
			}
			del := f.find("DELETE", "/git/refs/heads/"+testBranch)
			if tc.wantErr {
				if res.Merged {
					t.Error("Merged = true on failed merge")
				}
				if del != nil {
					t.Error("branch deleted despite failed merge")
				}
				return
			}
			if !res.Merged {
				t.Error("Merged = false")
			}
			if del == nil {
				t.Errorf("no branch delete call in %+v", f.calls)
			}
			if want := []string{"PR #12 opened", "merged PR #12"}; len(logs) != 2 || logs[0] != want[0] || logs[1] != want[1] {
				t.Errorf("logs = %q, want %q", logs, want)
			}
		})
	}
}

// Failing to delete the branch after a merge is logged, not fatal.
func TestOpenPRBranchDeleteBestEffort(t *testing.T) {
	f, srv := newFakeGitHub(t)
	stubAPI(t, srv)
	// Serve 422 for the delete by wrapping the handler.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message": "Reference does not exist"}`)
			return
		}
		f.ServeHTTP(w, r)
	})
	cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Branch: "main"}
	res, err := openPR(context.Background(), cfg, "tok", testChange(), testBranch, true, discard)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged {
		t.Error("Merged = false")
	}
}

// fakeForgejo serves the canned Forgejo API endpoints Publish uses. The
// version probe the SDK issues at client construction is answered but not
// recorded, so call assertions see only the real work.
type fakeForgejo struct {
	t             *testing.T
	defaultBranch string
	prNumber      int
	prURL         string
	mergeStatuses []int // per-attempt status; past the end → 200
	calls         []ghCall
}

func (f *fakeForgejo) merges() int {
	n := 0
	for _, c := range f.calls {
		if c.method == "POST" && strings.HasSuffix(c.path, "/merge") {
			n++
		}
	}
	return n
}

func (f *fakeForgejo) find(method, pathSuffix string) *ghCall {
	for i, c := range f.calls {
		if c.method == method && strings.HasSuffix(c.path, pathSuffix) {
			return &f.calls[i]
		}
	}
	return nil
}

func (f *fakeForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p, m := r.URL.Path, r.Method
	if m == "GET" && p == "/api/v1/version" {
		fmt.Fprint(w, `{"version": "12.0.0"}`)
		return
	}

	call := ghCall{method: m, path: p}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&call.body)
	}
	merges := f.merges()
	f.calls = append(f.calls, call)

	switch {
	case m == "GET" && p == "/api/v1/repos/acme/gitops":
		fmt.Fprintf(w, `{"default_branch": %q}`, f.defaultBranch)
	case m == "POST" && p == "/api/v1/repos/acme/gitops/pulls":
		fmt.Fprintf(w, `{"number": %d, "html_url": %q}`, f.prNumber, f.prURL)
	case m == "POST" && p == fmt.Sprintf("/api/v1/repos/acme/gitops/pulls/%d/merge", f.prNumber):
		if merges < len(f.mergeStatuses) && f.mergeStatuses[merges] != 200 {
			w.WriteHeader(f.mergeStatuses[merges])
			fmt.Fprint(w, `{"message": "Please try again later"}`)
			return
		}
	case m == "DELETE" && strings.HasPrefix(p, "/api/v1/repos/acme/gitops/branches/"):
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected request %s %s", m, p)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message": "not found"}`)
	}
}

func newFakeForgejo(t *testing.T) (*fakeForgejo, *httptest.Server) {
	f := &fakeForgejo{t: t, defaultBranch: "main", prNumber: 7, prURL: "https://git.example.com/acme/gitops/pulls/7"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func forgejoCfg(url, branch string) spec.GitConfig {
	return spec.GitConfig{Type: "forgejo", URL: url, Repo: "acme/gitops", Path: "clusters/staging", Branch: branch}
}

func TestOpenPRForgejo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfgBranch string
		wantBase  string
		wantGets  int // repo lookups to resolve the default branch
	}{
		{"explicit base", "main", "main", 0},
		{"default branch resolved", "", "trunk", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeForgejo(t)
			f.defaultBranch = "trunk"
			cfg := forgejoCfg(srv.URL, tc.cfgBranch)
			ch := testChange()

			var logs []string
			res, err := openPR(context.Background(), cfg, "tok", ch, testBranch, false, func(s string) { logs = append(logs, s) })
			if err != nil {
				t.Fatal(err)
			}
			want := Result{Branch: testBranch, PRURL: f.prURL, PRNumber: 7}
			if res != want {
				t.Fatalf("res = %+v, want %+v", res, want)
			}

			gets := 0
			for _, c := range f.calls {
				if c.method == "GET" {
					gets++
				}
			}
			if gets != tc.wantGets {
				t.Errorf("repo GETs = %d, want %d", gets, tc.wantGets)
			}
			create := f.find("POST", "/pulls")
			if create == nil {
				t.Fatalf("no PR create call in %+v", f.calls)
			}
			for k, want := range map[string]string{
				"title": "assimilate: deploy staging",
				"head":  testBranch,
				"base":  tc.wantBase,
				"body":  ch.Message,
			} {
				if got := create.body[k]; got != want {
					t.Errorf("create body %s = %v, want %q", k, got, want)
				}
			}
			if n := f.merges(); n != 0 {
				t.Errorf("merge calls = %d without rollout", n)
			}
			if len(logs) != 1 || logs[0] != "PR #7 opened" {
				t.Errorf("logs = %q", logs)
			}
		})
	}
}

func TestOpenPRForgejoRollout(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mergeStatuses []int
		wantMerges    int
		wantErr       bool
	}{
		{"merges immediately", nil, 1, false},
		{"retries 409 then 405", []int{409, 405, 200}, 3, false},
		{"gives up after retries", []int{409, 409, 409, 409}, 4, true},
		{"non-retryable status", []int{403}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeForgejo(t)
			f.mergeStatuses = tc.mergeStatuses
			stubBackoff(t)
			cfg := forgejoCfg(srv.URL, "main")

			var logs []string
			res, err := openPR(context.Background(), cfg, "tok", testChange(), testBranch, true, func(s string) { logs = append(logs, s) })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if n := f.merges(); n != tc.wantMerges {
				t.Errorf("merge attempts = %d, want %d", n, tc.wantMerges)
			}
			merge := f.find("POST", "/merge")
			if merge == nil {
				t.Fatal("no merge call")
			}
			if got := merge.body["Do"]; got != "squash" {
				t.Errorf("merge style = %v, want squash", got)
			}
			del := f.find("DELETE", "/branches/"+testBranch)
			if tc.wantErr {
				if res.Merged {
					t.Error("Merged = true on failed merge")
				}
				if del != nil {
					t.Error("branch deleted despite failed merge")
				}
				return
			}
			if !res.Merged {
				t.Error("Merged = false")
			}
			if del == nil {
				t.Errorf("no branch delete call in %+v", f.calls)
			}
			if want := []string{"PR #7 opened", "merged PR #7"}; len(logs) != 2 || logs[0] != want[0] || logs[1] != want[1] {
				t.Errorf("logs = %q, want %q", logs, want)
			}
		})
	}
}

// Failing to delete the branch after a merge is logged, not fatal.
func TestOpenPRForgejoBranchDeleteBestEffort(t *testing.T) {
	f, srv := newFakeForgejo(t)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message": "protected"}`)
			return
		}
		f.ServeHTTP(w, r)
	})
	cfg := forgejoCfg(srv.URL, "main")
	res, err := openPR(context.Background(), cfg, "tok", testChange(), testBranch, true, discard)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged {
		t.Error("Merged = false")
	}
}

func TestPublishForgejo(t *testing.T) {
	upstream := seedUpstream(t, map[string]string{"clusters/staging/api.yaml": marked("old\n")})
	f, srv := newFakeForgejo(t)
	stubGit(t, upstream)
	stubBackoff(t)
	cfg := forgejoCfg(srv.URL, "main")
	ch := testChange()

	res, err := Publish(context.Background(), cfg, "", ch, true, discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Result{Branch: testBranch, PRURL: f.prURL, PRNumber: 7, Merged: true}
	if res != want {
		t.Fatalf("res = %+v, want %+v", res, want)
	}
	if _, files := branchTip(t, upstream, testBranch); files["clusters/staging/api.yaml"] != marked(string(ch.Files["api.yaml"])) {
		t.Error("pushed branch missing rendered file")
	}
	create := f.find("POST", "/pulls")
	if create == nil || create.body["head"] != res.Branch {
		t.Errorf("PR create = %+v", create)
	}
	if f.merges() != 1 || f.find("DELETE", "/branches/"+testBranch) == nil {
		t.Errorf("rollout calls = %+v", f.calls)
	}
}

func TestCloneURL(t *testing.T) {
	for _, tc := range []struct {
		cfg  spec.GitConfig
		want string
	}{
		{spec.GitConfig{Type: "github", Repo: "acme/gitops"}, "https://github.com/acme/gitops.git"},
		{spec.GitConfig{Type: "forgejo", URL: "https://git.example.com", Repo: "acme/gitops"}, "https://git.example.com/acme/gitops.git"},
	} {
		if got := cloneURL(tc.cfg); got != tc.want {
			t.Errorf("cloneURL(%+v) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

func TestPublish(t *testing.T) {
	upstream := seedUpstream(t, map[string]string{"clusters/staging/api.yaml": marked("old\n")})
	f, srv := newFakeGitHub(t)
	stubGit(t, upstream)
	stubAPI(t, srv)
	cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}
	ch := testChange()

	var logs []string
	res, err := Publish(context.Background(), cfg, "", ch, true, func(s string) { logs = append(logs, s) })
	if err != nil {
		t.Fatal(err)
	}
	want := Result{Branch: testBranch, PRURL: f.prURL, PRNumber: 12, Merged: true}
	if res != want {
		t.Fatalf("res = %+v, want %+v", res, want)
	}
	if _, files := branchTip(t, upstream, testBranch); files["clusters/staging/api.yaml"] != marked(string(ch.Files["api.yaml"])) {
		t.Error("pushed branch missing rendered file")
	}
	create := f.find("POST", "/pulls")
	if create == nil || create.body["head"] != res.Branch {
		t.Errorf("PR create = %+v", create)
	}
	if f.merges() != 1 || f.find("DELETE", "/git/refs/heads/"+testBranch) == nil {
		t.Errorf("rollout calls = %+v", f.calls)
	}
}

func TestPublishNoChanges(t *testing.T) {
	ch := testChange()
	upstream := seedUpstream(t, map[string]string{
		"clusters/staging/api.yaml":           marked(string(ch.Files["api.yaml"])),
		"clusters/staging/workers/queue.yaml": marked(string(ch.Files["workers/queue.yaml"])),
	})
	f, srv := newFakeGitHub(t)
	stubGit(t, upstream)
	stubAPI(t, srv)
	cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}

	res, err := Publish(context.Background(), cfg, "", ch, true, discard)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NoChanges || res.Merged || res.PRURL != "" {
		t.Fatalf("res = %+v", res)
	}
	if len(f.calls) != 0 {
		t.Errorf("API calls on no-change publish: %+v", f.calls)
	}
}

func TestPublishRejectsConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  spec.GitConfig
	}{
		{"unknown provider", spec.GitConfig{Type: "gitlab", Repo: "acme/gitops"}},
		{"empty provider", spec.GitConfig{Repo: "acme/gitops"}},
		{"malformed repo", spec.GitConfig{Type: "github", Repo: "gitops"}},
		{"repo with extra segment", spec.GitConfig{Type: "github", Repo: "a/b/c"}},
		{"forgejo without url", spec.GitConfig{Type: "forgejo", Repo: "acme/gitops"}},
		{"forgejo malformed repo", spec.GitConfig{Type: "forgejo", URL: "https://git.example.com", Repo: "gitops"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Publish(context.Background(), tc.cfg, "", testChange(), false, discard); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestGitAuth(t *testing.T) {
	for _, tc := range []struct {
		url, token string
		want       bool
	}{
		{"https://github.com/a/b.git", "tok", true},
		{"http://github.local/a/b.git", "tok", true},
		{"/tmp/local-upstream", "tok", false},
		{"https://github.com/a/b.git", "", false},
	} {
		if got := gitAuth(tc.url, tc.token) != nil; got != tc.want {
			t.Errorf("gitAuth(%q, %q) auth=%v, want %v", tc.url, tc.token, got, tc.want)
		}
	}
}

// Files under cfg.Path that assimilate generated for this domain but no
// longer renders are pruned (JSON together with its sidecar); files of
// another domain, pre-domain markers, unmarked files, other extensions and
// anything outside cfg.Path stay. Pruning alone is a change worth a commit.
func TestPushBranchPrunes(t *testing.T) {
	ch := testChange()
	jsonBody := `{"a":1}` + "\n"
	jsonSidecar := ownership.MarkerPrefix + ownership.ComputeBodyHash("x.json", []byte(jsonBody)) + "\n" + ownership.DomainPrefix + "mono\n"
	seed := map[string]string{
		"clusters/staging/api.yaml":                               marked(string(ch.Files["api.yaml"])),
		"clusters/staging/workers/queue.yaml":                     marked(string(ch.Files["workers/queue.yaml"])),
		"clusters/staging/old.yaml":                               marked("kind: Gone\n"),
		"clusters/staging/workers/old.yml":                        marked("kind: Gone\n"),
		"clusters/staging/old/config.json":                        jsonBody,
		"clusters/staging/old/config.json" + ownership.SidecarExt: jsonSidecar,
		"clusters/staging/other.yaml":                             markedAs("other", "kind: Theirs\n"),
		"clusters/staging/legacy.yaml":                            markedAs("", "kind: Legacy\n"),
		"clusters/staging/hand.yaml":                              "kind: Handwritten\n",
		"clusters/staging/notes.txt":                              "notes\n",
		"clusters/prod/old.yaml":                                  marked("kind: Prod\n"),
	}
	upstream := seedUpstream(t, seed)
	stubGit(t, upstream)
	cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}

	var logs []string
	branch, noChanges, err := pushBranch(context.Background(), cfg, "", ch, func(s string) { logs = append(logs, s) })
	if err != nil || noChanges {
		t.Fatalf("noChanges=%v err=%v", noChanges, err)
	}
	_, files := branchTip(t, upstream, branch)
	for _, gone := range []string{
		"clusters/staging/old.yaml",
		"clusters/staging/workers/old.yml",
		"clusters/staging/old/config.json",
		"clusters/staging/old/config.json" + ownership.SidecarExt,
	} {
		if _, ok := files[gone]; ok {
			t.Errorf("%s not pruned", gone)
		}
	}
	for _, kept := range []string{
		"clusters/staging/api.yaml",
		"clusters/staging/workers/queue.yaml",
		"clusters/staging/other.yaml",
		"clusters/staging/legacy.yaml",
		"clusters/staging/hand.yaml",
		"clusters/staging/notes.txt",
		"clusters/prod/old.yaml",
		".seed",
	} {
		if _, ok := files[kept]; !ok {
			t.Errorf("%s wrongly pruned", kept)
		}
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{
		"pruning clusters/staging/old.yaml",
		"pruning clusters/staging/workers/old.yml",
		"pruning clusters/staging/old/config.json",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("logs %q missing %q", logs, want)
		}
	}
	if strings.Contains(joined, "other.yaml") || strings.Contains(joined, "legacy.yaml") {
		t.Errorf("logs mention files that must not be pruned: %q", logs)
	}
}

// With cfg.Path at the repository root the prune walk must skip .git.
func TestPushBranchPrunesAtRepoRoot(t *testing.T) {
	ch := testChange()
	upstream := seedUpstream(t, map[string]string{
		"README.md": "hi\n",
		"old.yaml":  marked("kind: Gone\n"),
	})
	stubGit(t, upstream)
	cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "", Branch: "main"}

	branch, noChanges, err := pushBranch(context.Background(), cfg, "", ch, discard)
	if err != nil || noChanges {
		t.Fatalf("noChanges=%v err=%v", noChanges, err)
	}
	_, files := branchTip(t, upstream, branch)
	if _, ok := files["old.yaml"]; ok {
		t.Error("old.yaml not pruned")
	}
	for _, kept := range []string{"README.md", ".seed", "api.yaml", "workers/queue.yaml"} {
		if _, ok := files[kept]; !ok {
			t.Errorf("%s missing", kept)
		}
	}
}

// A stale file of this domain that was edited since assimilate generated it
// is a conflict like an edited target: listed, refused without Force, and
// pruned with Force.
func TestPushBranchPruneEditedConflict(t *testing.T) {
	edited := ownership.MarkerPrefix + ownership.ComputeBodyHash("x.yaml", []byte("original\n")) + "\n" + ownership.DomainPrefix + "mono\nedited by hand\n"
	seed := map[string]string{"clusters/staging/old.yaml": edited}
	cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}

	t.Run("without force", func(t *testing.T) {
		upstream := seedUpstream(t, seed)
		stubGit(t, upstream)
		_, _, err := pushBranch(context.Background(), cfg, "", testChange(), discard)
		if err == nil {
			t.Fatal("no error")
		}
		for _, want := range []string{"clusters/staging/old.yaml: edited since assimilate generated it", "--force"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
		for _, name := range branchNames(t, upstream) {
			if strings.HasPrefix(name, "assimilate/") {
				t.Errorf("branch %s pushed despite conflicts", name)
			}
		}
	})

	t.Run("with force", func(t *testing.T) {
		upstream := seedUpstream(t, seed)
		stubGit(t, upstream)
		ch := testChange()
		ch.Force = true
		var logs []string
		branch, noChanges, err := pushBranch(context.Background(), cfg, "", ch, func(s string) { logs = append(logs, s) })
		if err != nil || noChanges {
			t.Fatalf("noChanges=%v err=%v", noChanges, err)
		}
		if _, files := branchTip(t, upstream, branch); files["clusters/staging/old.yaml"] != "" {
			t.Error("old.yaml not pruned")
		}
		if want := "pruning clusters/staging/old.yaml (edited since assimilate generated it)"; !strings.Contains(strings.Join(logs, "\n"), want) {
			t.Errorf("logs %q missing %q", logs, want)
		}
	})
}

// A target generated by another domain is a conflict: two source repos
// rendering the same path must not silently steal it from each other.
func TestPushBranchForeignDomainConflict(t *testing.T) {
	seed := map[string]string{"clusters/staging/api.yaml": markedAs("other", "kind: Theirs\n")}
	cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}

	t.Run("without force", func(t *testing.T) {
		upstream := seedUpstream(t, seed)
		stubGit(t, upstream)
		_, _, err := pushBranch(context.Background(), cfg, "", testChange(), discard)
		if err == nil {
			t.Fatal("no error")
		}
		for _, want := range []string{`clusters/staging/api.yaml: generated by assimilate-domain "other"`, "--force"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})

	t.Run("with force", func(t *testing.T) {
		upstream := seedUpstream(t, seed)
		stubGit(t, upstream)
		ch := testChange()
		ch.Force = true
		var logs []string
		branch, _, err := pushBranch(context.Background(), cfg, "", ch, func(s string) { logs = append(logs, s) })
		if err != nil {
			t.Fatal(err)
		}
		if _, files := branchTip(t, upstream, branch); files["clusters/staging/api.yaml"] != marked(string(ch.Files["api.yaml"])) {
			t.Errorf("api.yaml = %q", files["clusters/staging/api.yaml"])
		}
		if want := `overwriting clusters/staging/api.yaml (generated by assimilate-domain "other")`; !strings.Contains(strings.Join(logs, "\n"), want) {
			t.Errorf("logs %q missing %q", logs, want)
		}
	})
}

// A target with a pre-domain marker is adopted: no conflict, and the
// re-publish stamps it with the domain even when the body is unchanged.
func TestPushBranchAdoptsLegacyMarker(t *testing.T) {
	ch := testChange()
	upstream := seedUpstream(t, map[string]string{
		"clusters/staging/api.yaml":           markedAs("", string(ch.Files["api.yaml"])),
		"clusters/staging/workers/queue.yaml": markedAs("", string(ch.Files["workers/queue.yaml"])),
	})
	stubGit(t, upstream)
	cfg := spec.GitConfig{Type: "github", Repo: "acme/gitops", Path: "clusters/staging", Branch: "main"}

	var logs []string
	branch, noChanges, err := pushBranch(context.Background(), cfg, "", ch, func(s string) { logs = append(logs, s) })
	if err != nil || noChanges {
		t.Fatalf("noChanges=%v err=%v", noChanges, err)
	}
	if _, files := branchTip(t, upstream, branch); files["clusters/staging/api.yaml"] != marked(string(ch.Files["api.yaml"])) {
		t.Errorf("api.yaml = %q", files["clusters/staging/api.yaml"])
	}
	if joined := strings.Join(logs, "\n"); strings.Contains(joined, "overwriting") {
		t.Errorf("legacy adoption logged as overwrite: %q", logs)
	}
}
