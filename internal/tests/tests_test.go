package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jobs-build/assimilate/internal/spec"
)

// fakeTree stands in for *jobs.Local: it lists dirs, or fails with err.
type fakeTree struct {
	dirs []string
	err  error
}

func (f fakeTree) TestDirs(context.Context, string) ([]string, error) { return f.dirs, f.err }

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mustDiscover runs Discover over a tree listing dirs.
func mustDiscover(t *testing.T, root string, envs []Env, dirs ...string) *Set {
	t.Helper()
	set, err := Discover(context.Background(), root, envs, fakeTree{dirs: dirs})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return set
}

// named returns s with the row label Discover gives it.
func named(s spec.BuildSpec, name string) spec.BuildSpec {
	s.Name = name
	return s
}

func checkTests(t *testing.T, got, want []Test) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tests:\n got %+v\nwant %+v", got, want)
	}
}

func checkOrigins(t *testing.T, tests []Test, want ...string) {
	t.Helper()
	var got []string
	for _, tt := range tests {
		got = append(got, tt.Origin())
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Origins = %q, want %q", got, want)
	}
}

func TestReadSidecar(t *testing.T) {
	cases := []struct {
		name, yaml string
		want       []string
		wantErr    string
	}{
		{"two platforms", "platforms:\n  - linux/amd64\n  - linux/arm64\n", []string{"linux/amd64", "linux/arm64"}, ""},
		{"duplicates kept for the caller", "platforms: [linux/amd64, linux/amd64]\n", []string{"linux/amd64", "linux/amd64"}, ""},
		{"empty list", "platforms: []\n", nil, "platforms is required"},
		{"missing key", "{}\n", nil, "platforms is required"},
		{"empty file", "", nil, "platforms is required"},
		{"empty entry", "platforms: ['']\n", nil, "platforms[0] must be a non-empty string"},
		{"unknown key", "platforms: [linux/amd64]\nargs: {a: b}\n", nil, "field args not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, SidecarFile, c.yaml)
			file := filepath.Join(dir, SidecarFile)
			got, err := ReadSidecar(file)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ReadSidecar = %q, want an error containing %q", got, c.wantErr)
				}
				if msg := err.Error(); !strings.HasPrefix(msg, file+": ") || !strings.Contains(msg, c.wantErr) {
					t.Fatalf("error %q, want it to start with %q and contain %q", msg, file+": ", c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadSidecar: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ReadSidecar = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDiscoverUnion(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/TESTS.jobs", "def test(): pass\n")
	writeFile(t, root, "lib/"+SidecarFile, "platforms:\n  - linux/amd64\n  - linux/arm64\n")
	writeFile(t, root, "services/a/TESTS.jobs", "def test(): pass\n")
	writeFile(t, root, "services/b/BUILD.jobs", "# recipe\n")
	a := spec.BuildSpec{Name: "a", Path: "/services/a", Platform: "linux/amd64", Args: map[string]string{"v": "1"}}
	b := spec.BuildSpec{Name: "b", Path: "/services/b", Platform: "linux/amd64"}
	aProd := a
	aProd.BuildFile = "BUILD.prod"
	envs := []Env{
		{Name: "staging", Builds: []spec.BuildSpec{a, b, a}}, // a twice: staging is listed once
		{Name: "prod", Builds: []spec.BuildSpec{aProd}},
	}

	set := mustDiscover(t, root, envs, "/lib", "/services/a")
	checkTests(t, set.Tests, []Test{
		{Spec: named(a.TestOf(), "a"), Image: "a", Envs: []string{"staging", "prod"}},
		{Spec: spec.BuildSpec{Name: "/lib linux/amd64", Path: "/lib", Platform: "linux/amd64", Test: true, WholeTree: true}},
		{Spec: spec.BuildSpec{Name: "/lib linux/arm64", Path: "/lib", Platform: "linux/arm64", Test: true, WholeTree: true}},
	})
	if len(set.Tests) > 0 && set.Tests[0].Spec.Key() != a.TestOf().Key() {
		t.Errorf("template test key = %q, want %q", set.Tests[0].Spec.Key(), a.TestOf().Key())
	}
	if want := []spec.BuildSpec{b}; !reflect.DeepEqual(set.Untested, want) {
		t.Errorf("Untested = %+v, want %+v", set.Untested, want)
	}
	checkOrigins(t, set.Tests, "image a (staging, prod)", "assimilate-tests.yaml", "assimilate-tests.yaml")
}

func TestDiscoverSidecarDuplicates(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/TESTS.jobs", "def test(): pass\n")
	writeFile(t, root, "lib/"+SidecarFile, "platforms: [linux/amd64, linux/amd64]\n")

	set := mustDiscover(t, root, nil, "/lib")
	checkTests(t, set.Tests, []Test{
		{Spec: spec.BuildSpec{Name: "/lib linux/amd64", Path: "/lib", Platform: "linux/amd64", Test: true, WholeTree: true}},
	})
}

func TestDiscoverMissingSidecar(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/TESTS.jobs", "def test(): pass\n")

	set, err := Discover(context.Background(), root, nil, fakeTree{dirs: []string{"/lib"}})
	want := "/lib/TESTS.jobs: no platforms declared; add /lib/assimilate-tests.yaml with:\n  platforms:\n    - linux/amd64"
	if err == nil || err.Error() != want {
		t.Fatalf("Discover = %+v, %v; want error %q", set, err, want)
	}
}

func TestDiscoverInvalidSidecar(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/TESTS.jobs", "def test(): pass\n")
	writeFile(t, root, "lib/"+SidecarFile, "platforms: []\n")

	set, err := Discover(context.Background(), root, nil, fakeTree{dirs: []string{"/lib"}})
	file := filepath.Join(root, "lib", SidecarFile)
	if err == nil || !strings.HasPrefix(err.Error(), file+": platforms is required") {
		t.Fatalf("Discover = %+v, %v; want the error of %s", set, err, file)
	}
}

func TestDiscoverRootTest(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "TESTS.jobs", "def test(): pass\n")

	_, err := Discover(context.Background(), root, nil, fakeTree{dirs: []string{"/"}})
	want := "/TESTS.jobs: no platforms declared; add /assimilate-tests.yaml with:\n  platforms:\n    - linux/amd64"
	if err == nil || err.Error() != want {
		t.Fatalf("Discover without a sidecar: %v; want error %q", err, want)
	}

	writeFile(t, root, SidecarFile, "platforms: [linux/amd64]\n")
	set := mustDiscover(t, root, nil, "/")
	checkTests(t, set.Tests, []Test{
		{Spec: spec.BuildSpec{Name: "/ linux/amd64", Path: "/", Platform: "linux/amd64", Test: true}},
	})
	checkOrigins(t, set.Tests, "assimilate-tests.yaml")
}

func TestDiscoverTemplateWinsPerScope(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "services/a/TESTS.jobs", "def test(): pass\n")
	writeFile(t, root, "services/a/"+SidecarFile, "platforms: [linux/arm64]\n")
	a := spec.BuildSpec{Name: "a", Path: "/services/a", Platform: "linux/amd64"}
	staging := Env{Name: "staging", Builds: []spec.BuildSpec{a}}
	prod := Env{Name: "prod"}

	t.Run("image out of scope", func(t *testing.T) {
		set := mustDiscover(t, root, []Env{prod}, "/services/a")
		checkTests(t, set.Tests, []Test{
			{Spec: spec.BuildSpec{Name: "/services/a linux/arm64", Path: "/services/a", Platform: "linux/arm64", Test: true, WholeTree: true}},
		})
	})
	t.Run("image in scope", func(t *testing.T) {
		set := mustDiscover(t, root, []Env{staging, prod}, "/services/a")
		checkTests(t, set.Tests, []Test{
			{Spec: named(a.TestOf(), "a"), Image: "a", Envs: []string{"staging"}, SidecarIgnored: true},
		})
		checkOrigins(t, set.Tests, "image a (staging); assimilate-tests.yaml ignored")
	})
}

func TestDiscoverUntestedImages(t *testing.T) {
	x := spec.BuildSpec{Name: "x", Path: "/svc/x", Platform: "linux/amd64"}
	xProd := x
	xProd.Platform = "linux/arm64"
	envs := []Env{
		{Name: "staging", Builds: []spec.BuildSpec{x}},
		{Name: "prod", Builds: []spec.BuildSpec{xProd}},
	}

	set := mustDiscover(t, t.TempDir(), envs)
	checkTests(t, set.Tests, nil)
	if want := []spec.BuildSpec{x}; !reflect.DeepEqual(set.Untested, want) {
		t.Errorf("Untested = %+v, want %+v", set.Untested, want)
	}
}

func TestDiscoverLabelCollision(t *testing.T) {
	apiStaging := spec.BuildSpec{Name: "api", Path: "/svc", Platform: "linux/amd64", Sources: []string{"/proto"}}
	apiProd := spec.BuildSpec{Name: "api", Path: "/svc", Platform: "linux/amd64"}
	webStaging := spec.BuildSpec{Name: "web", Path: "/web", Platform: "linux/amd64", Args: map[string]string{"c": "3", "a": "1", "b": "2"}}
	webProd := spec.BuildSpec{Name: "web", Path: "/web", Platform: "linux/amd64", Args: map[string]string{"a": "1"}, Sources: []string{"/proto", "/shared"}}
	worker := spec.BuildSpec{Name: "worker", Path: "/worker", Platform: "linux/amd64"}
	envs := []Env{
		{Name: "staging", Builds: []spec.BuildSpec{apiStaging, webStaging, worker}},
		{Name: "prod", Builds: []spec.BuildSpec{apiProd, webProd, worker}},
	}

	set := mustDiscover(t, t.TempDir(), envs, "/svc", "/web", "/worker")
	var got []string
	for _, tt := range set.Tests {
		got = append(got, tt.Spec.Name)
	}
	want := []string{
		"/svc linux/amd64 sources=/proto",
		"/web linux/amd64 a=1 b=2 c=3",
		"worker",
		"/svc linux/amd64",
		"/web linux/amd64 a=1 sources=/proto,/shared",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %q, want %q", got, want)
	}
	checkOrigins(t, set.Tests, "image api (staging)", "image web (staging)", "image worker (staging, prod)", "image api (prod)", "image web (prod)")
}

func TestDiscoverTreeError(t *testing.T) {
	boom := errors.New("boom")
	set, err := Discover(context.Background(), t.TempDir(), nil, fakeTree{err: boom})
	if !errors.Is(err, boom) || set != nil {
		t.Fatalf("Discover = %+v, %v; want nil and an error wrapping %v", set, err, boom)
	}
}
