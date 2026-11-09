package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/urfave/cli/v2"

	"github.com/jobs-build/assimilate/internal/builds"
	"github.com/jobs-build/assimilate/internal/jobs"
	"github.com/jobs-build/assimilate/internal/spec"
)

func TestReorderArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "trailing flag after positional",
			in:   []string{"assimilate", "deploy", "staging", "--rollout"},
			want: []string{"assimilate", "deploy", "--rollout", "staging"},
		},
		{
			name: "mixed flags stay in order",
			in:   []string{"assimilate", "deploy", "--plain", "staging", "--rollout"},
			want: []string{"assimilate", "deploy", "--plain", "--rollout", "staging"},
		},
		{
			name: "no flags untouched",
			in:   []string{"assimilate", "render", "staging"},
			want: []string{"assimilate", "render", "staging"},
		},
		{
			name: "global flag is not a subcommand",
			in:   []string{"assimilate", "--help"},
			want: []string{"assimilate", "--help"},
		},
		{
			name: "terminator freezes everything after it",
			in:   []string{"assimilate", "deploy", "--", "staging", "--rollout"},
			want: []string{"assimilate", "deploy", "--", "staging", "--rollout"},
		},
		{
			name: "bare invocation",
			in:   []string{"assimilate"},
			want: []string{"assimilate"},
		},
		{
			name: "value flag keeps its separate value",
			in:   []string{"assimilate", "deploy", "staging", "--adopt-legacy", "manifests/hoodi"},
			want: []string{"assimilate", "deploy", "--adopt-legacy", "manifests/hoodi", "staging"},
		},
		{
			name: "value flag in equals form",
			in:   []string{"assimilate", "deploy", "staging", "--adopt-legacy=.", "--rollout"},
			want: []string{"assimilate", "deploy", "--adopt-legacy=.", "--rollout", "staging"},
		},
		{
			name: "value flag missing its value at the end",
			in:   []string{"assimilate", "deploy", "staging", "--adopt-legacy"},
			want: []string{"assimilate", "deploy", "--adopt-legacy", "staging"},
		},
		{
			name: "test with trailing flag",
			in:   []string{"assimilate", "test", "staging", "--plain"},
			want: []string{"assimilate", "test", "--plain", "staging"},
		},
		{
			name: "test without environment",
			in:   []string{"assimilate", "test", "--list"},
			want: []string{"assimilate", "test", "--list"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reorderArgs(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("reorderArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAppParsesTrailingRolloutFlag drives the real cli.App with reordered
// args: the documented `assimilate deploy staging --rollout` must parse
// --rollout as a flag and leave exactly one positional.
func TestAppParsesTrailingRolloutFlag(t *testing.T) {
	app := newApp()
	var called bool
	for _, cmd := range app.Commands {
		if cmd.Name == "deploy" {
			cmd.Action = func(c *cli.Context) error {
				called = true
				if !c.Bool("rollout") {
					t.Error("rollout flag not parsed")
				}
				if c.NArg() != 1 {
					t.Errorf("NArg = %d, want 1 (args %q)", c.NArg(), c.Args().Slice())
				}
				if c.Args().First() != "staging" {
					t.Errorf("first arg = %q, want %q", c.Args().First(), "staging")
				}
				return nil
			}
		}
	}
	args := reorderArgs([]string{"assimilate", "deploy", "staging", "--rollout"})
	if err := app.Run(args); err != nil {
		t.Fatalf("app.Run(%q): %v", args, err)
	}
	if !called {
		t.Fatal("deploy action never ran")
	}
}

// TestAppParsesTestCommand drives the real cli.App: `assimilate test
// staging --list --plain` parses both flags and one positional, and the
// environment is optional.
func TestAppParsesTestCommand(t *testing.T) {
	app := newApp()
	var got *cli.Context
	for _, cmd := range app.Commands {
		if cmd.Name == "test" {
			cmd.Action = func(c *cli.Context) error {
				got = c
				return nil
			}
		}
	}

	args := reorderArgs([]string{"assimilate", "test", "staging", "--list", "--plain"})
	if err := app.Run(args); err != nil {
		t.Fatalf("app.Run(%q): %v", args, err)
	}
	if got == nil {
		t.Fatal("test action never ran")
	}
	if !got.Bool("list") {
		t.Error("list flag not parsed")
	}
	if !got.Bool("plain") {
		t.Error("plain flag not parsed")
	}
	if got.NArg() != 1 {
		t.Errorf("NArg = %d, want 1 (args %q)", got.NArg(), got.Args().Slice())
	}
	if got.Args().First() != "staging" {
		t.Errorf("first arg = %q, want %q", got.Args().First(), "staging")
	}

	got = nil
	if err := app.Run([]string{"assimilate", "test"}); err != nil {
		t.Fatalf("app.Run(assimilate test): %v", err)
	}
	if got == nil {
		t.Fatal("test action never ran without an environment")
	}
	if got.NArg() != 0 {
		t.Errorf("NArg = %d, want 0 (args %q)", got.NArg(), got.Args().Slice())
	}
}

// TestTestCommandWithoutServer drives `assimilate test` over a synthetic
// project with no JOBS_SERVER: a TESTS.jobs with neither an image nor a
// sidecar is reported before the missing server, --list needs no server, and
// a run without one is refused after discovery.
func TestTestCommandWithoutServer(t *testing.T) {
	root := t.TempDir()
	image := func(name, path string, args string) string {
		return "containers:\n  - name: " + name + "\n    image:\n      type: jobs-build\n      name: " + name +
			"\n      path: " + path + "\n      platform: linux/amd64\n" + args
	}
	writeTree(t, root, map[string]string{
		"assimilate-templates/staging/api.yaml": image("api", "/services/api", "      args:\n        v: \"1\"\n        a: x\n"),
		"assimilate-templates/staging/web.yaml": image("web", "/services/web", ""),
		"services/api/BUILD.jobs":               "# recipe\n",
		"services/api/TESTS.jobs":               "# tests\n",
		"services/web/BUILD.jobs":               "# recipe\n",
		"lib/proto/TESTS.jobs":                  "# tests\n",
	})
	t.Chdir(filepath.Join(root, "services"))
	t.Setenv("ASSIMILATE_DATA_DIR", t.TempDir())
	t.Setenv("JOBS_SERVER", "")
	run := func(args ...string) (string, error) {
		app := newApp()
		var out strings.Builder
		app.Writer = &out
		err := app.Run(reorderArgs(append([]string{"assimilate", "test"}, args...)))
		return out.String(), err
	}

	if _, err := run(); err == nil || !strings.Contains(err.Error(), "/lib/proto/TESTS.jobs: no platforms declared") {
		t.Fatalf("missing sidecar: err = %v", err)
	}

	writeTree(t, root, map[string]string{"lib/proto/assimilate-tests.yaml": "platforms:\n  - linux/amd64\n  - linux/arm64\n"})
	out, err := run("staging", "--list")
	if err != nil {
		t.Fatalf("--list: %v", err)
	}
	want := "" +
		"api                     /services/api  linux/amd64  a=x v=1  image api (staging)\n" +
		"/lib/proto linux/amd64  /lib/proto     linux/amd64  -        assimilate-tests.yaml\n" +
		"/lib/proto linux/arm64  /lib/proto     linux/arm64  -        assimilate-tests.yaml\n" +
		"images without TESTS.jobs: web\n"
	if out != want {
		t.Fatalf("--list output:\n%s\nwant:\n%s", out, want)
	}

	if _, err := run("staging"); err == nil || err.Error() != "JOBS_SERVER must be set (jobs-iroh server endpoint ID)" {
		t.Fatalf("no server: err = %v", err)
	}

	// Nothing found needs no server either.
	empty := t.TempDir()
	writeTree(t, empty, map[string]string{
		"assimilate-templates/staging/other.yaml": "kind: x\n",
		"README.md": "hi\n",
	})
	t.Chdir(empty)
	out, err = run("staging")
	if err != nil || out != "no tests found\n" {
		t.Fatalf("empty set: out = %q, err = %v", out, err)
	}
}

func TestTestSummary(t *testing.T) {
	done := builds.Result{State: spec.StateDone}
	hit := builds.Result{State: spec.StateDone, Cached: true}
	failed := builds.Result{State: spec.StateFailed}
	cancelled := builds.Result{State: spec.StateCancelled}
	for _, tc := range []struct {
		in   []builds.Result
		want string
	}{
		{nil, "no tests found"},
		{[]builds.Result{done, hit, hit}, "3 passed (2 already passed), 0 failed"},
		{[]builds.Result{done, failed}, "1 passed (0 already passed), 1 failed"},
		{[]builds.Result{hit, cancelled, cancelled}, "1 passed (1 already passed), 0 failed, 2 cancelled"},
	} {
		if got := testSummary(tc.in); got != tc.want {
			t.Errorf("testSummary = %q, want %q", got, tc.want)
		}
	}
}

func TestSignalContextFirstSignalCancels(t *testing.T) {
	ctx, stop := signalContext(context.Background())
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("ctx not cancelled after SIGINT")
	}
}

// TestSignalContextSecondSignalKills re-executes the test binary as a child
// that holds a signalContext: the first SIGINT must cancel ctx (child prints
// "draining"), after which the released registration must let a further
// SIGINT terminate the child instead of being swallowed for the whole drain.
func TestSignalContextSecondSignalKills(t *testing.T) {
	if os.Getenv("ASSIMILATE_SIGNAL_CHILD") == "1" {
		ctx, stop := signalContext(context.Background())
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("draining")
		time.Sleep(30 * time.Second) // simulated drain; a signal must cut it short
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestSignalContextSecondSignalKills$")
	cmd.Env = append(os.Environ(), "ASSIMILATE_SIGNAL_CHILD=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	sc := bufio.NewScanner(stdout)
	waitLine := func(want string) {
		for sc.Scan() {
			if sc.Text() == want {
				return
			}
		}
		t.Fatalf("child exited before printing %q", want)
	}
	waitLine("ready")
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitLine("draining")

	// The release goroutine runs just after ctx.Done(), so retry the second
	// signal until one lands on the restored default disposition.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(10 * time.Second)
	for {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("child exited without signal status: %v", err)
			}
			ws, ok := ee.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
				t.Fatalf("child exit status = %v, want death by SIGINT", ee)
			}
			return
		case <-deadline:
			t.Fatal("second SIGINT swallowed; child kept draining")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestGithubToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	stub := func(t *testing.T, token string, err error) {
		t.Helper()
		orig := ghAuthToken
		ghAuthToken = func(context.Context) (string, error) { return token, err }
		t.Cleanup(func() { ghAuthToken = orig })
	}

	t.Run("env wins over gh", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "from-env")
		stub(t, "from-gh", nil)
		got, err := githubToken(context.Background())
		if err != nil || got != "from-env" {
			t.Fatalf("got %q, %v; want from-env, nil", got, err)
		}
	})

	t.Run("falls back to gh cli", func(t *testing.T) {
		stub(t, "from-gh", nil)
		got, err := githubToken(context.Background())
		if err != nil || got != "from-gh" {
			t.Fatalf("got %q, %v; want from-gh, nil", got, err)
		}
	})

	t.Run("not logged in", func(t *testing.T) {
		stub(t, "", errors.New("exit status 1"))
		if _, err := githubToken(context.Background()); err == nil {
			t.Fatal("want error when neither env nor gh provides a token")
		}
	})

	t.Run("gh absent returns empty", func(t *testing.T) {
		stub(t, "", nil)
		if _, err := githubToken(context.Background()); err == nil {
			t.Fatal("want error when gh returns an empty token")
		}
	})
}

// isolateXDG points the CLI-config search at a fresh temp dir so tests
// never read (or refresh!) the developer's real tea logins.
func isolateXDG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_CONFIG_DIRS", filepath.Join(dir, "sys"))
	xdg.Reload()
	t.Cleanup(xdg.Reload)
	return dir
}

func TestGitToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("FORGEJO_TOKEN", "")
	t.Setenv("GITEA_TOKEN", "")
	isolateXDG(t)
	fjCfg := spec.GitConfig{Type: "forgejo", URL: "https://git.example.com"}

	t.Run("forgejo from env", func(t *testing.T) {
		t.Setenv("FORGEJO_TOKEN", "fj-tok")
		t.Setenv("GITEA_TOKEN", "gt-tok")
		got, err := gitToken(context.Background(), fjCfg)
		if err != nil || got != "fj-tok" {
			t.Fatalf("got %q, %v; want fj-tok, nil", got, err)
		}
	})

	t.Run("forgejo GITEA_TOKEN fallback", func(t *testing.T) {
		t.Setenv("GITEA_TOKEN", "gt-tok")
		got, err := gitToken(context.Background(), fjCfg)
		if err != nil || got != "gt-tok" {
			t.Fatalf("got %q, %v; want gt-tok, nil", got, err)
		}
	})

	t.Run("forgejo falls back to CLI config", func(t *testing.T) {
		home := isolateXDG(t)
		path := filepath.Join(home, "tea", "config.yml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := "logins:\n    - name: x\n      url: https://git.example.com\n      token: cli-tok\n"
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := gitToken(context.Background(), fjCfg)
		if err != nil || got != "cli-tok" {
			t.Fatalf("got %q, %v; want cli-tok, nil", got, err)
		}
	})

	t.Run("forgejo no source", func(t *testing.T) {
		_, err := gitToken(context.Background(), fjCfg)
		if err == nil || !strings.Contains(err.Error(), "FORGEJO_TOKEN") {
			t.Fatalf("err = %v, want mention of FORGEJO_TOKEN", err)
		}
	})

	t.Run("github ignores forgejo tokens", func(t *testing.T) {
		t.Setenv("FORGEJO_TOKEN", "fj-tok")
		t.Setenv("GITEA_TOKEN", "gt-tok")
		t.Setenv("GITHUB_TOKEN", "gh-tok")
		got, err := gitToken(context.Background(), spec.GitConfig{Type: "github"})
		if err != nil || got != "gh-tok" {
			t.Fatalf("got %q, %v; want gh-tok, nil", got, err)
		}
	})
}

// parseAdoptDir turns the --adopt-legacy value into a cfg.Path-relative
// subtree: "." (or a slash) is the whole path (""), other values are
// cleaned; empty and escaping values are errors.
func TestParseAdoptDir(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: ".", want: ""},
		{in: "/", want: ""},
		{in: "manifests/hoodi", want: "manifests/hoodi"},
		{in: "/manifests/hoodi/", want: "manifests/hoodi"},
		{in: "./manifests//hoodi", want: "manifests/hoodi"},
		{in: "", wantErr: "requires a directory"},
		{in: "../other", wantErr: "escapes"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseAdoptDir(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseAdoptDir(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The same path with and without sources is two source trees: render must
// key them apart, or the with-sources image gets a tag deploy would never
// produce.
func TestResolveSourcesKeysBySource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"services/a/BUILD.jobs": "# recipe\n",
		"services/a/main.go":    "package main\n",
		"lib/x.go":              "package lib\n",
	})
	local, err := jobs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	plain := spec.BuildSpec{Path: "/services/a", Platform: "linux/amd64"}
	withLib := spec.BuildSpec{Path: "/services/a", Sources: []string{"/lib"}, Platform: "linux/amd64"}
	srcs, err := resolveSources(ctx, local, root, []spec.BuildSpec{plain, withLib, plain}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 {
		t.Fatalf("%d sources resolved, want 2", len(srcs))
	}
	for _, s := range []spec.BuildSpec{plain, withLib} {
		want, err := local.SourceFor(ctx, root, s)
		if err != nil {
			t.Fatal(err)
		}
		if got := srcs[s.SourceKey()]; got.String() != want.String() {
			t.Errorf("%v: resolved %s, want %s", s.Sources, got, want)
		}
	}
	if srcs[plain.SourceKey()].String() == srcs[withLib.SourceKey()].String() {
		t.Error("the two source trees are the same")
	}
}

// deploy resolves every image with sources before it builds anything: one
// bad source must not surface only after all other images were built.
func TestPreflightSources(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"services/a/BUILD.jobs": "# recipe\n",
		"lib/x.go":              "package lib\n",
	})
	local, err := jobs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	good := spec.BuildSpec{Path: "/services/a", Sources: []string{"/lib"}, Platform: "linux/amd64"}
	bad := spec.BuildSpec{Name: "broken", Path: "/services/a", Sources: []string{"/nope"}, Platform: "linux/amd64"}
	// Images without sources are left to the build run, as before.
	plainMissing := spec.BuildSpec{Path: "/services/gone", Platform: "linux/amd64"}

	if err := preflightSources(ctx, local, root, []spec.BuildSpec{plainMissing, good}); err != nil {
		t.Fatalf("preflight = %v", err)
	}
	err = preflightSources(ctx, local, root, []spec.BuildSpec{good, plainMissing, bad})
	if err == nil || !strings.Contains(err.Error(), "/nope is not in the project tree") || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("preflight = %v", err)
	}
}
