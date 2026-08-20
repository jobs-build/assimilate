package project

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jobs-build/assimilate/internal/spec"
)

// mkTree creates dirs (relative, "/"-separated) and files (path → content)
// under a fresh temp dir and returns its path.
func mkTree(t *testing.T, dirs []string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFindRoot(t *testing.T) {
	root := mkTree(t, []string{TemplatesDir + "/staging", "a/b/c"}, nil)

	t.Run("nested", func(t *testing.T) {
		got, err := FindRoot(filepath.Join(root, "a", "b", "c"))
		if err != nil {
			t.Fatal(err)
		}
		if got != root {
			t.Fatalf("got %q, want %q", got, root)
		}
	})

	t.Run("at root", func(t *testing.T) {
		got, err := FindRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		if got != root {
			t.Fatalf("got %q, want %q", got, root)
		}
	})

	t.Run("relative from", func(t *testing.T) {
		t.Chdir(filepath.Join(root, "a", "b"))
		got, err := FindRoot(".")
		if err != nil {
			t.Fatal(err)
		}
		if got != root {
			t.Fatalf("got %q, want %q", got, root)
		}
	})

	t.Run("file named like templates dir is skipped", func(t *testing.T) {
		r := mkTree(t, []string{TemplatesDir, "inner"}, map[string]string{"inner/" + TemplatesDir: ""})
		got, err := FindRoot(filepath.Join(r, "inner"))
		if err != nil {
			t.Fatal(err)
		}
		if got != r {
			t.Fatalf("got %q, want %q", got, r)
		}
	})

	t.Run("not found", func(t *testing.T) {
		start := mkTree(t, []string{"x/y"}, nil)
		from := filepath.Join(start, "x", "y")
		_, err := FindRoot(from)
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), from) {
			t.Fatalf("error %q does not name the start dir %q", err, from)
		}
		if !strings.Contains(err.Error(), TemplatesDir) {
			t.Fatalf("error %q does not name %q", err, TemplatesDir)
		}
	})
}

func TestEnvDir(t *testing.T) {
	root := mkTree(t,
		[]string{TemplatesDir + "/staging", TemplatesDir + "/prod"},
		map[string]string{
			TemplatesDir + "/README.md": "not an env",
			TemplatesDir + "/broken":    "a file, not an env",
		})

	t.Run("ok", func(t *testing.T) {
		got, err := EnvDir(root, "staging")
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(root, TemplatesDir, "staging")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	for _, env := range []string{"missing", "broken"} {
		t.Run("not a dir: "+env, func(t *testing.T) {
			_, err := EnvDir(root, env)
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), "prod, staging") {
				t.Fatalf("error %q does not list environments sorted", err)
			}
			if strings.Contains(err.Error(), "README") {
				t.Fatalf("error %q lists a plain file as an environment", err)
			}
		})
	}

	t.Run("no templates dir", func(t *testing.T) {
		bare := t.TempDir()
		_, err := EnvDir(bare, "staging")
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "no environments") {
			t.Fatalf("error %q does not mention missing environments", err)
		}
	})

	for _, env := range []string{"", ".", "..", "a/b", "../prod"} {
		t.Run("invalid name "+env, func(t *testing.T) {
			if _, err := EnvDir(root, env); err == nil {
				t.Fatalf("EnvDir(%q): want error", env)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    spec.Config
		wantErr string // substring of the error; "" = success
	}{
		{
			name: "full",
			yaml: `git:
  github:
    repo: fables-for-robots/gitops
    path: clusters/staging
    branch: main
registry: registry.example.com:5000
argocd:
  - url: https://argocd.example.com/applications/argocd/my-app
  - url: https://argocd.example.com/applications/other
`,
			want: spec.Config{
				Git:      spec.GitConfig{Type: "github", Repo: "fables-for-robots/gitops", Path: "clusters/staging", Branch: "main"},
				Registry: "registry.example.com:5000",
				ArgoCD: []spec.ArgoApp{
					{Server: "https://argocd.example.com", Namespace: "argocd", Name: "my-app"},
					{Server: "https://argocd.example.com", Name: "other"},
				},
			},
		},
		{
			name: "empty file defaults",
			yaml: "",
			want: spec.Config{Registry: "localhost:5000"},
		},
		{
			name: "git absent is allowed",
			yaml: "registry: r:5000\n",
			want: spec.Config{Registry: "r:5000"},
		},
		{
			name: "registry defaults with git",
			yaml: "git:\n  github:\n    repo: a/b\n",
			want: spec.Config{Git: spec.GitConfig{Type: "github", Repo: "a/b"}, Registry: "localhost:5000"},
		},
		{
			name: "path cleaned leading slash",
			yaml: "git:\n  github:\n    repo: a/b\n    path: /clusters/staging/\n",
			want: spec.Config{Git: spec.GitConfig{Type: "github", Repo: "a/b", Path: "clusters/staging"}, Registry: "localhost:5000"},
		},
		{
			name: "path cleaned dot segments",
			yaml: "git:\n  github:\n    repo: a/b\n    path: ./clusters//./staging\n",
			want: spec.Config{Git: spec.GitConfig{Type: "github", Repo: "a/b", Path: "clusters/staging"}, Registry: "localhost:5000"},
		},
		{
			name: "path dot is repo root",
			yaml: "git:\n  github:\n    repo: a/b\n    path: .\n",
			want: spec.Config{Git: spec.GitConfig{Type: "github", Repo: "a/b", Path: ""}, Registry: "localhost:5000"},
		},
		{
			name:    "path escapes root",
			yaml:    "git:\n  github:\n    repo: a/b\n    path: ../other\n",
			wantErr: "escapes the repository root",
		},
		{
			name:    "repo missing",
			yaml:    "git:\n  github:\n    path: x\n",
			wantErr: "git.github.repo: required",
		},
		{
			name:    "repo no slash",
			yaml:    "git:\n  github:\n    repo: gitops\n",
			wantErr: "owner/name",
		},
		{
			name:    "repo too many slashes",
			yaml:    "git:\n  github:\n    repo: a/b/c\n",
			wantErr: "owner/name",
		},
		{
			name:    "repo empty owner",
			yaml:    "git:\n  github:\n    repo: /gitops\n",
			wantErr: "owner/name",
		},
		{
			name:    "repo empty name",
			yaml:    "git:\n  github:\n    repo: owner/\n",
			wantErr: "owner/name",
		},
		{
			name:    "git without provider",
			yaml:    "git: {}\n",
			wantErr: "exactly one provider",
		},
		{
			name:    "unknown provider",
			yaml:    "git:\n  gitlab:\n    repo: a/b\n",
			wantErr: "gitlab",
		},
		{
			name:    "two providers",
			yaml:    "git:\n  github:\n    repo: a/b\n  gitlab:\n    repo: a/b\n",
			wantErr: "gitlab",
		},
		{
			name: "forgejo full",
			yaml: `git:
  forgejo:
    url: https://git.example.com
    repo: numtide/gitops
    path: manifests/staging
    branch: main
`,
			want: spec.Config{
				Git:      spec.GitConfig{Type: "forgejo", URL: "https://git.example.com", Repo: "numtide/gitops", Path: "manifests/staging", Branch: "main"},
				Registry: "localhost:5000",
			},
		},
		{
			name: "forgejo url trailing slash trimmed",
			yaml: "git:\n  forgejo:\n    url: https://git.example.com/\n    repo: a/b\n",
			want: spec.Config{Git: spec.GitConfig{Type: "forgejo", URL: "https://git.example.com", Repo: "a/b"}, Registry: "localhost:5000"},
		},
		{
			name:    "forgejo url missing",
			yaml:    "git:\n  forgejo:\n    repo: a/b\n",
			wantErr: "git.forgejo.url: required",
		},
		{
			name:    "forgejo url not http",
			yaml:    "git:\n  forgejo:\n    url: ssh://forgejo@git.example.com\n    repo: a/b\n",
			wantErr: "git.forgejo.url",
		},
		{
			name:    "forgejo url no host",
			yaml:    "git:\n  forgejo:\n    url: https://\n    repo: a/b\n",
			wantErr: "git.forgejo.url",
		},
		{
			name:    "forgejo repo malformed",
			yaml:    "git:\n  forgejo:\n    url: https://git.example.com\n    repo: gitops\n",
			wantErr: "owner/name",
		},
		{
			name:    "forgejo path escapes root",
			yaml:    "git:\n  forgejo:\n    url: https://git.example.com\n    repo: a/b\n    path: ../x\n",
			wantErr: "escapes the repository root",
		},
		{
			name:    "github and forgejo both set",
			yaml:    "git:\n  github:\n    repo: a/b\n  forgejo:\n    url: https://git.example.com\n    repo: a/b\n",
			wantErr: "exactly one provider",
		},
		{
			name:    "unknown forgejo key",
			yaml:    "git:\n  forgejo:\n    url: https://git.example.com\n    repo: a/b\n    remote: origin\n",
			wantErr: "remote",
		},
		{
			name:    "unknown top-level key",
			yaml:    "registri: x\n",
			wantErr: "registri",
		},
		{
			name:    "unknown github key",
			yaml:    "git:\n  github:\n    repo: a/b\n    remote: origin\n",
			wantErr: "remote",
		},
		{
			name:    "argocd entry missing url",
			yaml:    "argocd:\n  - {}\n",
			wantErr: "argocd[0]: missing url",
		},
		{
			name:    "argocd entry unknown key",
			yaml:    "argocd:\n  - url: https://a.example.com/applications/x\n    name: x\n",
			wantErr: "name",
		},
		{
			name:    "argocd bad url",
			yaml:    "argocd:\n  - url: https://a.example.com/applications/x\n  - url: https://a.example.com/settings\n",
			wantErr: "argocd[1]",
		},
		{
			name:    "git scalar",
			yaml:    "git: github\n",
			wantErr: "cannot unmarshal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(envDir, ConfigFile), []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := LoadConfig(envDir)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got config %+v", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadConfig(t.TempDir()); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestParseArgoURL(t *testing.T) {
	tests := []struct {
		raw     string
		want    spec.ArgoApp
		wantErr bool
	}{
		{
			raw:  "https://argocd.example.com/applications/my-app",
			want: spec.ArgoApp{Server: "https://argocd.example.com", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com/applications/argocd/my-app",
			want: spec.ArgoApp{Server: "https://argocd.example.com", Namespace: "argocd", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com:8443/applications/argocd/my-app",
			want: spec.ArgoApp{Server: "https://argocd.example.com:8443", Namespace: "argocd", Name: "my-app"},
		},
		{
			raw:  "http://localhost:8080/applications/my-app",
			want: spec.ArgoApp{Server: "http://localhost:8080", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com/applications/my-app/",
			want: spec.ArgoApp{Server: "https://argocd.example.com", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com/applications/argocd/my-app/",
			want: spec.ArgoApp{Server: "https://argocd.example.com", Namespace: "argocd", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com/applications/argocd/my-app?view=tree&resource=",
			want: spec.ArgoApp{Server: "https://argocd.example.com", Namespace: "argocd", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com/applications/my-app#event",
			want: spec.ArgoApp{Server: "https://argocd.example.com", Name: "my-app"},
		},
		{
			// rootpath-hosted UI: the prefix belongs to the server
			raw:  "https://argocd.example.com/argocd/applications/my-app",
			want: spec.ArgoApp{Server: "https://argocd.example.com/argocd", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com/argocd/applications/ns/my-app",
			want: spec.ArgoApp{Server: "https://argocd.example.com/argocd", Namespace: "ns", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com/argocd/applications/ns/my-app/",
			want: spec.ArgoApp{Server: "https://argocd.example.com/argocd", Namespace: "ns", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com/argocd/applications/my-app?view=tree&resource=",
			want: spec.ArgoApp{Server: "https://argocd.example.com/argocd", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com/a/b/applications/my-app",
			want: spec.ArgoApp{Server: "https://argocd.example.com/a/b", Name: "my-app"},
		},
		{
			raw:  "https://argocd.example.com/x/applications/my-app",
			want: spec.ArgoApp{Server: "https://argocd.example.com/x", Name: "my-app"},
		},
		{
			// the LAST applications segment splits server from app
			raw:  "https://argocd.example.com/applications/applications/my-app",
			want: spec.ArgoApp{Server: "https://argocd.example.com/applications", Name: "my-app"},
		},
		{raw: "", wantErr: true},
		{raw: "argocd.example.com/applications/my-app", wantErr: true}, // no scheme
		{raw: "ftp://argocd.example.com/applications/my-app", wantErr: true},
		{raw: "https:///applications/my-app", wantErr: true}, // no host
		{raw: "https://argocd.example.com", wantErr: true},   // no path
		{raw: "https://argocd.example.com/applications", wantErr: true},
		{raw: "https://argocd.example.com/applications/", wantErr: true},
		{raw: "https://argocd.example.com/settings/my-app", wantErr: true},
		{raw: "https://argocd.example.com/applications/a/b/c", wantErr: true},
		{raw: "https://argocd.example.com/applications//my-app", wantErr: true},
		{raw: "https://argocd.example.com/argocd/applications", wantErr: true},       // prefix but no app
		{raw: "https://argocd.example.com/argocd/applications/a/b/c", wantErr: true}, // >2 trailing segments
		{raw: "https://argocd.example.com/x//applications/my-app", wantErr: true},    // empty prefix segment
		{raw: "https://argocd.example.com/argocd/applications//my-app", wantErr: true},
		{raw: "://bad url", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := ParseArgoURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseArgoURL(%q) = %+v, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
