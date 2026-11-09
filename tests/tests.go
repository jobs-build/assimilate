// Package tests discovers the monorepo's TESTS.jobs tests as the union of two
// sources: every in-scope template image whose path holds a TESTS.jobs (the
// image's source tree, platform and args, so the test joins the image's
// build), and every other TESTS.jobs of the project tree, run against the
// whole tree once per platform its assimilate-tests.yaml declares. Every
// TESTS.jobs ends up in the set or in an error; nothing is inferred.
package tests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jobs-build/assimilate/spec"
)

// SidecarFile declares the platforms of a TESTS.jobs that no in-scope image
// covers; it sits next to the TESTS.jobs.
const SidecarFile = "assimilate-tests.yaml"

// Env is one environment's scanned image specs, in appearance order.
type Env struct {
	Name   string
	Builds []spec.BuildSpec
}

// Tree lists the TESTS.jobs directories of the ingested project tree as
// "/"-rooted paths ("/" for the root); *jobs.Local implements it.
type Tree interface {
	TestDirs(ctx context.Context, root string) ([]string, error)
}

// Test is one discovered test: a template test (Image set) or a sidecar test.
type Test struct {
	Spec           spec.BuildSpec // Test true; Name is the unique row label
	Image          string         // template test: the image's display name; "" for a sidecar test
	Envs           []string       // template test: environments the image appears in, scope order
	SidecarIgnored bool           // template test: a sidecar exists in the directory but the template won
}

// Set is the discovered tests of a scope, in run and list order.
type Set struct {
	Tests    []Test           // template tests in scope order, then sidecar tests by path, platforms in declared order
	Untested []spec.BuildSpec // in-scope images whose path has no TESTS.jobs; unique by Path, first appearance
}

// sidecar mirrors assimilate-tests.yaml exactly; KnownFields(true) turns any
// key outside this schema into a parse error.
type sidecar struct {
	Platforms []string `yaml:"platforms"`
}

// ReadSidecar returns the platforms declared in file (strict schema): one
// key, platforms, a non-empty list of non-empty strings. Duplicates are
// returned as written. A file that cannot be read yields the os.ReadFile
// error (fs.ErrNotExist when missing); decode and schema errors start with
// the file path.
func ReadSidecar(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var sc sidecar
	if err := dec.Decode(&sc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if len(sc.Platforms) == 0 {
		return nil, fmt.Errorf("%s: platforms is required and must be a non-empty list", file)
	}
	for i, p := range sc.Platforms {
		if p == "" {
			return nil, fmt.Errorf("%s: platforms[%d] must be a non-empty string", file, i)
		}
	}
	return sc.Platforms, nil
}

// Discover builds the Set for envs over the project at root. A TESTS.jobs
// directory that is an image path in scope is that image's test (the
// template wins over a sidecar there); any other needs a valid sidecar, and
// a missing or invalid one is an error naming the file to write.
func Discover(ctx context.Context, root string, envs []Env, tree Tree) (*Set, error) {
	dirs, err := tree.TestDirs(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("finding TESTS.jobs in the project tree: %w", err)
	}
	hasTest := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		hasTest[d] = true
	}

	set := &Set{}
	byKey := map[string]int{}     // test Key → index in set.Tests
	images := map[string]bool{}   // image paths in scope
	untested := map[string]bool{} // paths already in set.Untested
	for _, env := range envs {
		for _, b := range env.Builds {
			images[b.Path] = true
			if !hasTest[b.Path] {
				if !untested[b.Path] {
					untested[b.Path] = true
					set.Untested = append(set.Untested, b)
				}
				continue
			}
			t := b.TestOf()
			i, ok := byKey[t.Key()]
			if !ok {
				i = len(set.Tests)
				byKey[t.Key()] = i
				set.Tests = append(set.Tests, Test{
					Spec:           t,
					Image:          b.DisplayName(),
					SidecarIgnored: exists(sidecarPath(root, b.Path)),
				})
			}
			if !slices.Contains(set.Tests[i].Envs, env.Name) {
				set.Tests[i].Envs = append(set.Tests[i].Envs, env.Name)
			}
		}
	}

	for _, dir := range slices.Sorted(slices.Values(dirs)) {
		if images[dir] {
			continue
		}
		platforms, err := ReadSidecar(sidecarPath(root, dir))
		if errors.Is(err, fs.ErrNotExist) {
			d := strings.TrimSuffix(dir, "/") // the root reads /TESTS.jobs
			return nil, fmt.Errorf("%s/TESTS.jobs: no platforms declared; add %s/%s with:\n  platforms:\n    - linux/amd64", d, d, SidecarFile)
		}
		if err != nil {
			return nil, err
		}
		for _, p := range platforms {
			t := spec.WholeTreeTest(dir, p)
			if _, ok := byKey[t.Key()]; ok {
				continue
			}
			byKey[t.Key()] = len(set.Tests)
			set.Tests = append(set.Tests, Test{Spec: t})
		}
	}

	label(set.Tests)
	return set, nil
}

// label sets every test's Spec.Name to its row label: the image's display
// name for a template test, "<path> <platform>" for a sidecar test. A label
// two or more tests would share is replaced, for each of them, by the long
// form, which adds the sorted args and the sources.
func label(tests []Test) {
	short := make([]string, len(tests))
	count := map[string]int{}
	for i, t := range tests {
		short[i] = t.Image
		if t.Image == "" {
			short[i] = t.Spec.Path + " " + t.Spec.Platform
		}
		count[short[i]]++
	}
	for i := range tests {
		if count[short[i]] > 1 {
			tests[i].Spec.Name = longLabel(tests[i].Spec)
		} else {
			tests[i].Spec.Name = short[i]
		}
	}
}

// longLabel renders "<path> <platform>", then " k=v" per arg in key order,
// then " sources=/a,/b" when the spec has sources.
func longLabel(s spec.BuildSpec) string {
	var b strings.Builder
	b.WriteString(s.Path + " " + s.Platform)
	for _, k := range slices.Sorted(maps.Keys(s.Args)) {
		fmt.Fprintf(&b, " %s=%s", k, s.Args[k])
	}
	if len(s.Sources) > 0 {
		b.WriteString(" sources=" + strings.Join(s.Sources, ","))
	}
	return b.String()
}

// Origin describes where a test's platform came from, for --list: the image
// and its environments, or the sidecar file.
func (t Test) Origin() string {
	if t.Image == "" {
		return SidecarFile
	}
	o := "image " + t.Image + " (" + strings.Join(t.Envs, ", ") + ")"
	if t.SidecarIgnored {
		o += "; " + SidecarFile + " ignored"
	}
	return o
}

// sidecarPath locates the sidecar of the "/"-rooted dir on disk.
func sidecarPath(root, dir string) string {
	return filepath.Join(spec.SourceDir(root, dir), SidecarFile)
}

// exists reports whether file can be stat'ed.
func exists(file string) bool {
	_, err := os.Stat(file)
	return err == nil
}
