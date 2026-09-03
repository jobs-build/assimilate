// Command assimilate deploys a monorepo's k8s resources: it builds every
// image referenced by the environment's templates on a jobs-iroh server,
// renders the manifests with the resulting image references, publishes them
// to the GitOps repo as a PR, and with --rollout merges the PR and triggers
// the ArgoCD refresh/sync.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/urfave/cli/v2"
	"golang.org/x/term"

	"github.com/jobs-build/assimilate/internal/argocd"
	"github.com/jobs-build/assimilate/internal/builds"
	"github.com/jobs-build/assimilate/internal/gitops"
	"github.com/jobs-build/assimilate/internal/jobs"
	"github.com/jobs-build/assimilate/internal/project"
	"github.com/jobs-build/assimilate/internal/spec"
	"github.com/jobs-build/assimilate/internal/tealogin"
	"github.com/jobs-build/assimilate/internal/tmpl"
	"github.com/jobs-build/assimilate/internal/ui"
)

func main() {
	if err := newApp().Run(reorderArgs(os.Args)); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newApp builds the cli.App; extracted so tests can construct it and stub
// a command's Action.
func newApp() *cli.App {
	return &cli.App{
		Name:  "assimilate",
		Usage: "deploy a monorepo's k8s resources: jobs-iroh builds → rendered manifests → GitOps PR → ArgoCD",
		Commands: []*cli.Command{
			{
				Name:      "deploy",
				Usage:     "build all images and publish the rendered manifests as a PR",
				ArgsUsage: "<environment>",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "rollout", Usage: "merge the PR and trigger the ArgoCD refresh/sync"},
					&cli.BoolFlag{Name: "plain", Usage: "plain line output even on a TTY"},
					&cli.BoolFlag{Name: "force", Usage: "overwrite GitOps files that assimilate did not generate, that were edited since, or that another assimilate-domain generated; prune edited stale files too"},
					&cli.StringFlag{Name: "adopt-legacy", Usage: "one-time migration: also prune stale files with pre-domain markers (assimilate ≤ 0.5) under `DIR` of the configured path (. for all of it) as this repo's"},
				},
				Action: deploy,
			},
			{
				Name:      "render",
				Usage:     "render the environment's manifests with resolved image refs to stdout (no server, no build)",
				ArgsUsage: "<environment>",
				Action:    render,
			},
		},
	}
}

// valueFlags are the flags that take a value, which may follow as the next
// token (`--adopt-legacy DIR`) rather than attached (`--adopt-legacy=DIR`).
// reorderArgs must carry that token along with the flag.
var valueFlags = map[string]bool{"--adopt-legacy": true}

// reorderArgs stable-partitions the tokens after a known subcommand so
// dash-prefixed flags precede positionals: urfave/cli v2 stops flag parsing
// at the first positional, which would reject the documented
// `assimilate deploy staging --rollout`. Every flag is boolean except the
// ones in valueFlags, whose separate value token moves with them. A literal
// "--" and everything after it are left untouched; a lone "-" is a
// positional.
func reorderArgs(args []string) []string {
	if len(args) < 3 {
		return args
	}
	switch args[1] {
	case "deploy", "render":
	default:
		return args
	}
	out := append(make([]string, 0, len(args)), args[0], args[1])
	rest := args[2:]
	var flags, pos []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "--" {
			return append(append(append(out, flags...), pos...), rest[i:]...)
		}
		switch {
		case valueFlags[a] && i+1 < len(rest):
			flags = append(flags, a, rest[i+1])
			i++
		case len(a) > 1 && a[0] == '-':
			flags = append(flags, a)
		default:
			pos = append(pos, a)
		}
	}
	return append(append(out, flags...), pos...)
}

// parseAdoptDir validates the --adopt-legacy value into a subtree of the
// configured GitOps path: "" for the whole path (given as "." or "/"),
// otherwise the cleaned relative directory. Empty and escaping values are
// errors.
func parseAdoptDir(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("--adopt-legacy requires a directory under the configured GitOps path (. for all of it)")
	}
	dir, err := project.CleanRepoPath(raw)
	if err != nil {
		return "", fmt.Errorf("--adopt-legacy: %w", err)
	}
	return dir, nil
}

// signalContext returns a ctx cancelled on the first SIGINT/SIGTERM. The
// registration is released once ctx is done: keeping it would swallow every
// further signal for the rest of the drain, making the process unkillable —
// releasing it restores the default disposition so a second signal
// terminates the process.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// loadEnv resolves the environment argument into root, config and scanned
// templates — the shared front half of every command.
func loadEnv(c *cli.Context) (root string, cfg spec.Config, x *tmpl.Extraction, err error) {
	if c.NArg() != 1 {
		return "", spec.Config{}, nil, fmt.Errorf("usage: assimilate %s <environment>", c.Command.Name)
	}
	env := c.Args().First()
	cwd, err := os.Getwd()
	if err != nil {
		return "", spec.Config{}, nil, err
	}
	root, err = project.FindRoot(cwd)
	if err != nil {
		return "", spec.Config{}, nil, err
	}
	envDir, err := project.EnvDir(root, env)
	if err != nil {
		return "", spec.Config{}, nil, err
	}
	cfg, err = project.LoadConfig(envDir)
	if err != nil {
		return "", spec.Config{}, nil, err
	}
	x, err = tmpl.Scan(envDir)
	if err != nil {
		return "", spec.Config{}, nil, err
	}
	return root, cfg, x, nil
}

func deploy(c *cli.Context) error {
	root, cfg, x, err := loadEnv(c)
	if err != nil {
		return err
	}
	env := c.Args().First()
	rollout := c.Bool("rollout")

	// Preflight every credential and flag before any build starts.
	if cfg.Git.Type == "" {
		return errors.New("no git repo configured in assimilate.yaml")
	}
	var adoptLegacy bool
	var adoptDir string
	if c.IsSet("adopt-legacy") {
		if adoptDir, err = parseAdoptDir(c.String("adopt-legacy")); err != nil {
			return err
		}
		adoptLegacy = true
	}
	gitTok, err := gitToken(c.Context, cfg.Git)
	if err != nil {
		return err
	}
	var argoToken string
	if rollout && len(cfg.ArgoCD) > 0 {
		if argoToken = firstEnv("ARGOCD_AUTH_TOKEN", "ARGOCD_TOKEN"); argoToken == "" {
			return errors.New("ARGOCD_AUTH_TOKEN (or ARGOCD_TOKEN) must be set for --rollout with argocd configured")
		}
	}
	server := os.Getenv("JOBS_SERVER")
	if server == "" && len(x.Builds) > 0 {
		return errors.New("JOBS_SERVER must be set (jobs-iroh server endpoint ID)")
	}

	ctx, stop := signalContext(c.Context)
	defer stop()

	images := map[string]string{}
	var results []builds.Result
	if len(x.Builds) > 0 {
		local, err := jobs.Open(jobs.DefaultDataDir())
		if err != nil {
			return err
		}
		defer local.Close()
		client, err := jobs.Dial(ctx, local, jobs.Options{
			Server: server,
			Addrs:  splitAddrs(os.Getenv("JOBS_SERVER_ADDR")),
		})
		if err != nil {
			return err
		}
		defer client.Close()

		bctx, cancel := context.WithCancel(ctx)
		defer cancel()
		events := make(chan spec.Event, 256)
		names := make([]string, len(x.Builds))
		for i, s := range x.Builds {
			names[i] = s.DisplayName()
		}

		var runErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer close(events)
			results, runErr = builds.Run(bctx, root, cfg.Registry, x.Builds, client, events)
		}()
		if !c.Bool("plain") && term.IsTerminal(int(os.Stdout.Fd())) {
			if err := ui.RunTUI(bctx, cancel, names, events); err != nil {
				<-done
				return err
			}
		} else {
			ui.RunPlain(os.Stderr, names, events)
		}
		<-done
		if runErr != nil {
			for _, r := range results {
				if r.State != spec.StateDone {
					fmt.Fprintf(os.Stderr, "  %s: %s %s\n", r.Spec.DisplayName(), r.State, r.Err)
				}
			}
			return runErr
		}
		for _, r := range results {
			images[r.Spec.Key()] = r.ImageRef
		}
		local.MaybeGC(ctx) // opportunistic local-store sweep, at most daily
	}

	files, err := x.Render(images)
	if err != nil {
		return err
	}

	// Builds can outlast a short-lived OAuth access token; resolve again
	// right before the publish (a refresh only happens when needed).
	if gitTok, err = gitToken(ctx, cfg.Git); err != nil {
		return err
	}

	logf := func(line string) { fmt.Fprintln(os.Stderr, line) }
	res, err := gitops.Publish(ctx, cfg.Git, gitTok, gitops.Change{
		Env:         env,
		Domain:      cfg.Domain,
		Message:     commitMessage(env, results),
		Files:       files,
		Force:       c.Bool("force"),
		AdoptLegacy: adoptLegacy,
		AdoptDir:    adoptDir,
	}, rollout, logf)
	if err != nil {
		return err
	}
	switch {
	case res.NoChanges:
		fmt.Println("no manifest changes")
	case res.Merged:
		fmt.Println("merged:", res.PRURL)
	default:
		fmt.Println("pull request:", res.PRURL)
	}

	if rollout && len(cfg.ArgoCD) > 0 {
		if err := argocd.Rollout(ctx, cfg.ArgoCD, argoToken,
			os.Getenv("ARGOCD_INSECURE") == "true", logf); err != nil {
			return err
		}
	}
	return nil
}

// render resolves image refs offline (K is the hash of the canonical build
// definition — a local ingest suffices) and prints the rendered manifests.
func render(c *cli.Context) error {
	root, cfg, x, err := loadEnv(c)
	if err != nil {
		return err
	}
	images := map[string]string{}
	if len(x.Builds) > 0 {
		local, err := jobs.Open(jobs.DefaultDataDir())
		if err != nil {
			return err
		}
		defer local.Close()
		srcs := map[string]jobs.Source{}
		for _, s := range x.Builds {
			src, ok := srcs[s.Path]
			if !ok {
				if src, err = local.Ingest(c.Context, spec.SourceDir(root, s.Path)); err != nil {
					return fmt.Errorf("ingest %s: %w", s.Path, err)
				}
				srcs[s.Path] = src
			}
			k, err := local.DefinitionKey(src, s)
			if err != nil {
				return err
			}
			images[s.Key()] = spec.ImageRef(cfg.Registry, k)
		}
		local.MaybeGC(c.Context) // opportunistic local-store sweep, at most daily
	}
	files, err := x.Render(images)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Printf("# --- %s ---\n%s\n", p, files[p])
	}
	return nil
}

func commitMessage(env string, results []builds.Result) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "assimilate: deploy %s", env)
	if len(results) > 0 {
		fmt.Fprintf(b, " (%d images)\n", len(results))
		for _, r := range results {
			fmt.Fprintf(b, "\n%s → %s", r.Spec.DisplayName(), r.ImageRef)
		}
	}
	return b.String()
}

// ghAuthToken shells out to the GitHub CLI for the token of the logged-in
// user; a var so tests can stub it.
var ghAuthToken = func(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitToken resolves the credential for the configured GitOps provider.
// For forgejo: the environment first, then the tea-style CLI configs
// (forgejo's, then tea's), refreshing an expiring OAuth login in place.
func gitToken(ctx context.Context, cfg spec.GitConfig) (string, error) {
	if cfg.Type == "forgejo" {
		if t := firstEnv("FORGEJO_TOKEN", "GITEA_TOKEN"); t != "" {
			return t, nil
		}
		t, err := tealogin.Token(ctx, cfg.URL)
		if err == nil {
			return t, nil
		}
		if !errors.Is(err, tealogin.ErrNoLogin) {
			return "", err
		}
		return "", errors.New("no Forgejo credential: set FORGEJO_TOKEN (or GITEA_TOKEN), or log in with `tea login add`")
	}
	return githubToken(ctx)
}

// githubToken resolves the GitHub credential: the environment first, then the
// GitHub CLI's own token if the user is already logged in with `gh auth login`.
func githubToken(ctx context.Context) (string, error) {
	if t := firstEnv("GITHUB_TOKEN", "GH_TOKEN"); t != "" {
		return t, nil
	}
	if t, err := ghAuthToken(ctx); err == nil && t != "" {
		return t, nil
	}
	return "", errors.New("no GitHub credential: set GITHUB_TOKEN (or GH_TOKEN), or log in with `gh auth login`")
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func splitAddrs(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, a := range strings.Split(s, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}
