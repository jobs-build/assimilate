package tmpl

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jobs-build/assimilate/internal/spec"
)

// writeTree materializes files (slash-relative path -> content) in a temp
// dir standing in for one environment directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func mustScan(t *testing.T, dir string) *Extraction {
	t.Helper()
	x, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return x
}

func mustRender(t *testing.T, x *Extraction, images map[string]string) map[string][]byte {
	t.Helper()
	out, err := x.Render(images)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out
}

// allRefs maps every build key of x to a distinct fake image ref.
func allRefs(x *Extraction) map[string]string {
	images := map[string]string{}
	for i, s := range x.Builds {
		images[s.Key()] = fmt.Sprintf("localhost:5000/jobs:k%d", i)
	}
	return images
}

func TestScanDefaultsAndFields(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"api.yaml": `containers:
  - name: minimal
    image:
      type: jobs-build
      platform: linux/amd64
  - name: full
    image:
      type: jobs-build
      name: backend
      path: services/backend
      build-file: BUILD.prod
      args:
        variant: slim
        debug: "false"
      platform: linux/arm64
`,
	})
	x := mustScan(t, dir)
	want := []spec.BuildSpec{
		{Path: "/", Platform: "linux/amd64"},
		{
			Name:      "backend",
			Path:      "/services/backend",
			BuildFile: "BUILD.prod",
			Args:      map[string]string{"variant": "slim", "debug": "false"},
			Platform:  "linux/arm64",
		},
	}
	if !reflect.DeepEqual(x.Builds, want) {
		t.Fatalf("Builds = %+v, want %+v", x.Builds, want)
	}
}

func TestScanPathNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/", "/"},
		{"", "/"},
		{".", "/"},
		{"services/backend", "/services/backend"},
		{"/services/backend", "/services/backend"},
		{"./svc/", "/svc"},
		{"a//b/./c", "/a/b/c"},
		{`a\b`, "/a/b"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			dir := writeTree(t, map[string]string{
				"a.yaml": fmt.Sprintf("image:\n  type: jobs-build\n  platform: linux/amd64\n  path: %q\n", c.in),
			})
			x := mustScan(t, dir)
			if len(x.Builds) != 1 || x.Builds[0].Path != c.want {
				t.Fatalf("path %q normalized to %q, want %q", c.in, x.Builds[0].Path, c.want)
			}
		})
	}
}

func TestScanOrderingAndDedupe(t *testing.T) {
	// Spec identity is path+build-file+args+platform; name is cosmetic.
	// a.yaml: A (twice, second time under a different name), B (doc 2).
	// b/nested.yaml: A again, C. z.yaml: D.
	dir := writeTree(t, map[string]string{
		"a.yaml": `one:
  image:
    type: jobs-build
    name: a-first
    path: svc/a
    platform: linux/amd64
two:
  image:
    type: jobs-build
    name: a-renamed
    path: svc/a
    platform: linux/amd64
---
three:
  image:
    type: jobs-build
    path: svc/b
    platform: linux/amd64
`,
		"b/nested.yaml": `four:
  image:
    type: jobs-build
    path: svc/a
    platform: linux/amd64
five:
  image:
    type: jobs-build
    path: svc/c
    platform: linux/amd64
`,
		"z.yaml": `six:
  image:
    type: jobs-build
    path: svc/d
    platform: linux/amd64
`,
	})
	x := mustScan(t, dir)
	var got []string
	for _, s := range x.Builds {
		got = append(got, s.Path)
	}
	want := []string{"/svc/a", "/svc/b", "/svc/c", "/svc/d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("build order = %v, want %v", got, want)
	}
	if x.Builds[0].Name != "a-first" {
		t.Fatalf("dedupe kept name %q, want first appearance %q", x.Builds[0].Name, "a-first")
	}

	// Every site is substituted, including all duplicates of A.
	out := mustRender(t, x, allRefs(x))
	for f, wantN := range map[string]int{"a.yaml": 3, "b/nested.yaml": 2, "z.yaml": 1} {
		if n := strings.Count(string(out[f]), "localhost:5000/jobs:k"); n != wantN {
			t.Errorf("%s: %d substitutions, want %d\n%s", f, n, wantN, out[f])
		}
	}
	aRef := "localhost:5000/jobs:k0"
	if n := strings.Count(string(out["a.yaml"]), aRef) + strings.Count(string(out["b/nested.yaml"]), aRef); n != 3 {
		t.Errorf("spec A substituted %d times, want 3", n)
	}
}

func TestScanSkips(t *testing.T) {
	valid := "image:\n  type: jobs-build\n  path: %s\n  platform: linux/amd64\n"
	// Skipped files hold invalid YAML or invalid build objects so any
	// accidental parse fails the test loudly.
	dir := writeTree(t, map[string]string{
		"assimilate.yaml":     "image:\n  type: jobs-build\n  bogus: [unclosed\n",
		".hidden.yaml":        "also: [broken\n",
		".hiddendir/x.yaml":   "still: [broken\n",
		".hidden.json":        "{",
		"notes.md":            "# not yaml\n",
		"sub/assimilate.yaml": fmt.Sprintf(valid, "svc/a"), // only envDir/assimilate.yaml is special
		"app.yml":             fmt.Sprintf(valid, "svc/b"), // .yml counts
	})
	x := mustScan(t, dir)
	if len(x.Builds) != 2 {
		t.Fatalf("Builds = %+v, want 2 entries", x.Builds)
	}
	out := mustRender(t, x, allRefs(x))
	var got []string
	for p := range out {
		got = append(got, p)
	}
	if len(out) != 2 {
		t.Fatalf("rendered files = %v, want [app.yml sub/assimilate.yaml]", got)
	}
	for _, p := range []string{"app.yml", "sub/assimilate.yaml"} {
		if _, ok := out[p]; !ok {
			t.Errorf("missing rendered file %q (have %v)", p, got)
		}
	}
}

// JSON templates are never parsed or substituted: byte-identical passthrough,
// even for content that looks like a build object or is not valid JSON.
func TestScanJSONVerbatim(t *testing.T) {
	jsonBody := "{\n  \"image\": {\"type\": \"jobs-build\", \"platform\": \"linux/amd64\"}\n}\n"
	dir := writeTree(t, map[string]string{
		"config.json":     jsonBody,
		"sub/broken.json": "{not json",
		"api.yaml":        "image:\n  type: jobs-build\n  platform: linux/amd64\n",
	})
	x := mustScan(t, dir)
	if len(x.Builds) != 1 {
		t.Fatalf("Builds = %+v, want only the YAML build", x.Builds)
	}
	out := mustRender(t, x, allRefs(x))
	if len(out) != 3 {
		t.Fatalf("rendered %d files, want 3", len(out))
	}
	if got := string(out["config.json"]); got != jsonBody {
		t.Errorf("config.json = %q, want %q", got, jsonBody)
	}
	if got := string(out["sub/broken.json"]); got != "{not json" {
		t.Errorf("sub/broken.json = %q", got)
	}
}

func TestScanErrors(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantLine int    // 0 = don't check the :line: part
		wantSub  string // substring of the message
	}{
		{
			name: "missing platform",
			content: `image:
  type: jobs-build
  path: svc/a
`,
			wantLine: 2,
			wantSub:  "platform is required",
		},
		{
			name: "empty platform",
			content: `image:
  type: jobs-build
  platform: ""
`,
			wantLine: 3,
			wantSub:  "platform is required",
		},
		{
			name: "null platform",
			content: `image:
  type: jobs-build
  platform:
`,
			wantLine: 3,
			wantSub:  "platform is required",
		},
		{
			name: "unknown key",
			content: `image:
  type: jobs-build
  platform: linux/amd64
  platfrom: typo
`,
			wantLine: 4,
			wantSub:  `unknown key "platfrom"`,
		},
		{
			name: "duplicate key",
			content: `image:
  type: jobs-build
  platform: linux/amd64
  platform: linux/arm64
`,
			wantLine: 4,
			wantSub:  `duplicate key "platform"`,
		},
		{
			name: "args scalar",
			content: `image:
  type: jobs-build
  platform: linux/amd64
  args: nope
`,
			wantLine: 4,
			wantSub:  "args must be a mapping",
		},
		{
			name: "args sequence",
			content: `image:
  type: jobs-build
  platform: linux/amd64
  args:
    - a=b
`,
			wantLine: 5,
			wantSub:  "args must be a mapping",
		},
		{
			name: "args value not scalar",
			content: `image:
  type: jobs-build
  platform: linux/amd64
  args:
    k:
      - v
`,
			wantLine: 6,
			wantSub:  "args values must be strings",
		},
		{
			name: "args key not scalar",
			content: `image:
  type: jobs-build
  platform: linux/amd64
  args: { [a, b]: v }
`,
			wantLine: 4,
			wantSub:  "args keys must be strings",
		},
		{
			name: "platform not scalar",
			content: `image:
  type: jobs-build
  platform: [linux/amd64]
`,
			wantLine: 3,
			wantSub:  "platform must be a string",
		},
		{
			name: "name not scalar",
			content: `image:
  type: jobs-build
  platform: linux/amd64
  name:
    a: b
`,
			wantLine: 5,
			wantSub:  "name must be a string",
		},
		{
			name:    "broken yaml",
			content: "a: [unclosed\n",
			wantSub: "yaml",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"a.yaml": c.content})
			_, err := Scan(dir)
			if err == nil {
				t.Fatal("Scan succeeded, want error")
			}
			file := filepath.Join(dir, "a.yaml")
			if !strings.Contains(err.Error(), file) {
				t.Errorf("error %q does not name file %q", err, file)
			}
			if c.wantLine != 0 {
				loc := fmt.Sprintf("%s:%d:", file, c.wantLine)
				if !strings.Contains(err.Error(), loc) {
					t.Errorf("error %q does not carry %q", err, loc)
				}
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not contain %q", err, c.wantSub)
			}
		})
	}
}

// Untouched image values: scalars, sequences, mappings without type, and
// mappings with a different type — including strict-decode violations that
// would error in a real jobs-build object.
func TestScanNonJobsBuildUntouched(t *testing.T) {
	content := `a:
  image: nginx:latest # plain ref
b:
  image:
    - not
    - a-build
c:
  image:
    repository: nginx
    tag: latest
d:
  image:
    type: docker-build
    platform: linux/amd64
    unknown-key-no-error: true
e:
  image:
    type: 42
`
	dir := writeTree(t, map[string]string{"plain.yaml": content})
	x := mustScan(t, dir)
	if len(x.Builds) != 0 {
		t.Fatalf("Builds = %+v, want none", x.Builds)
	}
	out := mustRender(t, x, nil)
	if string(out["plain.yaml"]) != content {
		t.Fatalf("passthrough not byte-identical:\n%q\nwant\n%q", out["plain.yaml"], content)
	}
}

// A file with zero substitutions must survive Render byte-for-byte, however
// gnarly its formatting — no yaml round trip may touch it.
func TestRenderPassthroughByteIdentical(t *testing.T) {
	cases := map[string]string{
		"weird.yaml":     "# leading comment\n\n---\nkey:   spaced value\t\nlist: [1,2,  3]\n...\n---\nsecond: doc # trailing\n",
		"nonewline.yaml": "a: 1",
		"empty.yaml":     "",
		"comments.yaml":  "# only\n# comments\n",
		"crlf.yaml":      "a: 1\r\nb: 2\r\n",
		"anchors.yaml":   "base: &b\n  x: 1\nuse: *b\n",
	}
	dir := writeTree(t, cases)
	x := mustScan(t, dir)
	out := mustRender(t, x, nil)
	if len(out) != len(cases) {
		t.Fatalf("rendered %d files, want %d", len(out), len(cases))
	}
	for name, want := range cases {
		if got := string(out[name]); got != want {
			t.Errorf("%s not byte-identical:\n%q\nwant\n%q", name, got, want)
		}
	}
}

func TestRenderSubstitutionAndComments(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"api.yaml": `# deployment of the backend
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: backend
          # build the backend from source
          image:
            type: jobs-build
            name: backend
            path: services/backend
            platform: linux/amd64
          ports:
            - containerPort: 8080 # http
`,
	})
	x := mustScan(t, dir)
	ref := "localhost:5000/jobs:abc123"
	out := mustRender(t, x, map[string]string{x.Builds[0].Key(): ref})
	got := string(out["api.yaml"])

	for _, want := range []string{
		"# deployment of the backend",
		"# build the backend from source",
		"# http",
		"image: " + ref,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "jobs-build") {
		t.Errorf("rendered output still contains the build object:\n%s", got)
	}

	// The result must round-trip as YAML with the ref in place.
	var doc struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `yaml:"name"`
						Image string `yaml:"image"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(out["api.yaml"], &doc); err != nil {
		t.Fatalf("rendered output is not valid yaml: %v", err)
	}
	cs := doc.Spec.Template.Spec.Containers
	if len(cs) != 1 || cs[0].Image != ref {
		t.Fatalf("containers = %+v, want image %q", cs, ref)
	}
}

func TestRenderMultiDocument(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"multi.yaml": `kind: Deployment
image:
  type: jobs-build
  platform: linux/amd64
---
kind: Service
port: 80
`,
	})
	x := mustScan(t, dir)
	ref := "localhost:5000/jobs:k0"
	out := mustRender(t, x, map[string]string{x.Builds[0].Key(): ref})
	got := string(out["multi.yaml"])
	if !strings.Contains(got, "image: "+ref) {
		t.Fatalf("missing substitution:\n%s", got)
	}
	if !strings.Contains(got, "---\n") {
		t.Fatalf("document separator lost:\n%s", got)
	}
	if !strings.Contains(got, "kind: Service") || !strings.Contains(got, "port: 80") {
		t.Fatalf("second document lost:\n%s", got)
	}
}

func TestRenderMissingImageKey(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a.yaml": `image:
  type: jobs-build
  name: backend
  platform: linux/amd64
`,
	})
	x := mustScan(t, dir)
	_, err := x.Render(map[string]string{})
	if err == nil {
		t.Fatal("Render succeeded, want error")
	}
	file := filepath.Join(dir, "a.yaml")
	if !strings.Contains(err.Error(), file+":2:") {
		t.Errorf("error %q does not carry %q", err, file+":2:")
	}
	if !strings.Contains(err.Error(), "no image for build backend") {
		t.Errorf("error %q does not name the build", err)
	}
}

// Deeply nested build objects inside sequences of mappings (the k8s pod
// shape) and multiple containers per file.
func TestScanDeepNesting(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"deep.yaml": `spec:
  jobTemplate:
    spec:
      template:
        spec:
          initContainers:
            - name: migrate
              image:
                type: jobs-build
                path: services/migrate
                platform: linux/amd64
          containers:
            - name: worker
              image:
                type: jobs-build
                path: services/worker
                platform: linux/amd64
            - name: sidecar
              image: envoy:v1.30 # untouched
`,
	})
	x := mustScan(t, dir)
	var got []string
	for _, s := range x.Builds {
		got = append(got, s.Path)
	}
	want := []string{"/services/migrate", "/services/worker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("builds = %v, want %v", got, want)
	}
	out := mustRender(t, x, allRefs(x))
	s := string(out["deep.yaml"])
	if !strings.Contains(s, "image: localhost:5000/jobs:k0") ||
		!strings.Contains(s, "image: localhost:5000/jobs:k1") {
		t.Fatalf("substitutions missing:\n%s", s)
	}
	if !strings.Contains(s, "image: envoy:v1.30 # untouched") {
		t.Fatalf("plain image ref (or its comment) damaged:\n%s", s)
	}
}

// Args order must not affect identity, and differing args must prevent
// dedupe.
func TestScanArgsIdentity(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a.yaml": `one:
  image:
    type: jobs-build
    platform: linux/amd64
    args:
      x: "1"
      y: "2"
two:
  image:
    type: jobs-build
    platform: linux/amd64
    args:
      y: "2"
      x: "1"
three:
  image:
    type: jobs-build
    platform: linux/amd64
    args:
      x: "1"
`,
	})
	x := mustScan(t, dir)
	if len(x.Builds) != 2 {
		t.Fatalf("Builds = %+v, want 2 (reordered args dedupe, different args do not)", x.Builds)
	}
}

// wantScanErr scans a tree of {"a.yaml": content} and requires the error to
// carry a.yaml:line: and the given substring.
func wantScanErr(t *testing.T, content string, line int, sub string) {
	t.Helper()
	dir := writeTree(t, map[string]string{"a.yaml": content})
	_, err := Scan(dir)
	if err == nil {
		t.Fatal("Scan succeeded, want error")
	}
	loc := fmt.Sprintf("%s:%d:", filepath.Join(dir, "a.yaml"), line)
	if !strings.Contains(err.Error(), loc) {
		t.Errorf("error %q does not carry %q", err, loc)
	}
	if !strings.Contains(err.Error(), sub) {
		t.Errorf("error %q does not contain %q", err, sub)
	}
}

// An image value aliasing an anchored jobs-build mapping must not ship the
// raw build object; it is a Scan error telling the user to inline it.
func TestScanImageAliasToJobsBuildErrors(t *testing.T) {
	wantScanErr(t, `base: &common
  type: jobs-build
  platform: linux/amd64
app:
  image: *common
`, 5, `image aliases a jobs-build object (anchor "common"); inline the object`)
}

// Aliases to non-jobs-build values keep working untouched.
func TestScanImageAliasNonJobsBuildUntouched(t *testing.T) {
	content := `base: &b
  repository: nginx
  tag: latest
ref: &r nginx:latest
app:
  image: *b
other:
  image: *r
`
	dir := writeTree(t, map[string]string{"plain.yaml": content})
	x := mustScan(t, dir)
	if len(x.Builds) != 0 {
		t.Fatalf("Builds = %+v, want none", x.Builds)
	}
	out := mustRender(t, x, nil)
	if string(out["plain.yaml"]) != content {
		t.Fatalf("passthrough not byte-identical:\n%q\nwant\n%q", out["plain.yaml"], content)
	}
}

// A `<<` merge key in an image mapping is a Scan error — direct, whether or
// not the merge would deliver type: jobs-build, and through an alias.
func TestScanImageMergeKeyErrors(t *testing.T) {
	const sub = "merge keys are not supported in image objects; inline the fields"
	cases := []struct {
		name     string
		content  string
		wantLine int
	}{
		{
			name: "jobs-build via merge",
			content: `common: &common
  type: jobs-build
  platform: linux/amd64
app:
  image:
    <<: *common
    path: services/backend
`,
			wantLine: 6,
		},
		{
			name: "merge without jobs-build",
			content: `common: &c
  tag: latest
app:
  image:
    <<: *c
    repository: nginx
`,
			wantLine: 5,
		},
		{
			name: "alias to mapping with merge key",
			content: `base: &base
  type: jobs-build
mix: &common
  <<: *base
  platform: linux/amd64
app:
  image: *common
`,
			wantLine: 7,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantScanErr(t, c.content, c.wantLine, sub)
		})
	}
}

// An anchored jobs-build image node that is aliased (or merged) elsewhere
// cannot be substituted in place without corrupting the other sites; Scan
// fails naming the anchor.
func TestScanAnchoredSiteAliasedErrors(t *testing.T) {
	const sub = `anchor "build" on a jobs-build image object is aliased here; inline the object`
	cases := []struct {
		name     string
		content  string
		wantLine int
	}{
		{
			name: "plain alias",
			content: `one:
  image: &build
    type: jobs-build
    platform: linux/amd64
other:
  config: *build
`,
			wantLine: 6,
		},
		{
			name: "merge of the anchor",
			content: `one:
  image: &build
    type: jobs-build
    platform: linux/amd64
defaults:
  <<: *build
`,
			wantLine: 6,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantScanErr(t, c.content, c.wantLine, sub)
		})
	}
}

// An anchored jobs-build image with no aliases still scans and renders.
func TestScanAnchoredSiteUnaliasedOK(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a.yaml": `one:
  image: &solo
    type: jobs-build
    platform: linux/amd64
`,
	})
	x := mustScan(t, dir)
	if len(x.Builds) != 1 {
		t.Fatalf("Builds = %+v, want 1", x.Builds)
	}
	out := mustRender(t, x, allRefs(x))
	var doc struct {
		One struct {
			Image string `yaml:"image"`
		} `yaml:"one"`
	}
	if err := yaml.Unmarshal(out["a.yaml"], &doc); err != nil {
		t.Fatalf("rendered output is not valid yaml: %v\n%s", err, out["a.yaml"])
	}
	if doc.One.Image != "localhost:5000/jobs:k0" {
		t.Fatalf("image = %q, want the substituted ref\n%s", doc.One.Image, out["a.yaml"])
	}
}

// Builds must appear in lexical FULL-path order. filepath.WalkDir's
// per-directory order would visit foo/x.yaml before foo-bar.yaml; the
// contract is foo-bar.yaml < foo.yaml < foo/x.yaml.
func TestScanLexicalFullPathOrder(t *testing.T) {
	build := "image:\n  type: jobs-build\n  path: %s\n  platform: linux/amd64\n"
	dir := writeTree(t, map[string]string{
		"foo/x.yaml":   fmt.Sprintf(build, "from-foo-slash-x"),
		"foo-bar.yaml": fmt.Sprintf(build, "from-foo-bar"),
		"foo.yaml":     fmt.Sprintf(build, "from-foo"),
	})
	x := mustScan(t, dir)
	var got []string
	for _, s := range x.Builds {
		got = append(got, s.Path)
	}
	want := []string{"/from-foo-bar", "/from-foo", "/from-foo-slash-x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("build order = %v, want %v", got, want)
	}
}

// A path with a ".." segment names a directory outside the one written;
// normalizePath must not silently clamp it into the root.
func TestScanPathDotDotRejected(t *testing.T) {
	for _, p := range []string{"../shared", "a/../b", "..", `..\win`} {
		t.Run(p, func(t *testing.T) {
			content := fmt.Sprintf("image:\n  type: jobs-build\n  platform: linux/amd64\n  path: %q\n", p)
			wantScanErr(t, content, 4, `path must not contain ".."`)
		})
	}
}
func TestSourcesDecoding(t *testing.T) {
	image := func(body string) string {
		return "spec:\n  containers:\n    - name: api\n      image:\n        type: jobs-build\n        platform: linux/amd64\n" + body
	}
	scanOne := func(t *testing.T, content string) (spec.BuildSpec, error) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		x, err := Scan(dir)
		if err != nil {
			return spec.BuildSpec{}, err
		}
		if len(x.Builds) != 1 {
			t.Fatalf("%d builds, want 1", len(x.Builds))
		}
		return x.Builds[0], nil
	}

	t.Run("normalized", func(t *testing.T) {
		got, err := scanOne(t, image("        path: services/api\n        sources:\n          - proto\n          - /lib/\n          - ./proto\n          - /lib/sub\n          - services/api/gen\n"))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"/lib", "/proto"}; !reflect.DeepEqual(got.Sources, want) {
			t.Errorf("Sources = %v, want %v", got.Sources, want)
		}
	})
	t.Run("absent and empty mean none", func(t *testing.T) {
		for _, body := range []string{"        path: /services/api\n", "        path: /services/api\n        sources: []\n"} {
			got, err := scanOne(t, image(body))
			if err != nil || got.Sources != nil {
				t.Errorf("Sources = %v, %v; want nil", got.Sources, err)
			}
		}
	})
	t.Run("flow style and key order", func(t *testing.T) {
		got, err := scanOne(t, image("        sources: [/proto]\n        path: /services/api\n"))
		if err != nil || !reflect.DeepEqual(got.Sources, []string{"/proto"}) {
			t.Errorf("Sources = %v, %v", got.Sources, err)
		}
	})

	for name, tt := range map[string]struct{ body, want string }{
		"not a list":       {"        path: /services/api\n        sources: /proto\n", "sources must be a list of paths"},
		"a mapping entry":  {"        path: /services/api\n        sources:\n          - a: b\n", "sources entries must be non-empty strings"},
		"an empty entry":   {"        path: /services/api\n        sources:\n          - \"\"\n", "sources entries must be non-empty strings"},
		"dot-dot":          {"        path: /services/api\n        sources:\n          - ../other\n", `sources must not contain ".."`},
		"the project root": {"        path: /services/api\n        sources:\n          - /\n", "sources must not name the project root"},
		"on a root build":  {"        sources:\n          - /proto\n", "sources is not allowed on a root build"},
		"root build path":  {"        path: /\n        sources: [proto]\n", "sources is not allowed on a root build"},
		"duplicate key":    {"        path: /services/api\n        sources: [/a]\n        sources: [/b]\n", `duplicate key "sources"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := scanOne(t, image(tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "a.yaml:") {
				t.Errorf("err = %v, want one naming the file and %q", err, tt.want)
			}
		})
	}
}
