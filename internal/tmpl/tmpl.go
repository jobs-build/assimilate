// Package tmpl scans an environment's YAML templates for jobs-build image
// objects and renders the templates with resolved image references.
//
// A template is any *.yaml/*.yml/*.json file under the environment directory
// except assimilate.yaml, walked in lexical path order. Multi-document YAML
// files are supported. Anywhere in a YAML document, a mapping value of an
// `image` key that contains `type: jobs-build` is a build object; every other
// `image` value is left untouched. Build objects must be inlined: aliases and
// `<<` merge keys that hide or share one are Scan errors. Comments and
// document structure survive rendering (yaml.v3 node-level round trip).
// JSON templates are copied verbatim — no jobs-build substitution.
package tmpl

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jobs-build/assimilate/internal/spec"
)

// Extraction is the scanned template set of one environment.
type Extraction struct {
	// Builds are the unique build specs in order of first appearance
	// (files in lexical path order, objects in document order).
	Builds []spec.BuildSpec

	files []*tmplFile
}

// tmplFile is one parsed template. Files without substitution sites keep no
// role for docs at render time — raw is emitted verbatim, byte-identical.
type tmplFile struct {
	path string       // as walked; used in error messages
	rel  string       // envDir-relative slash path; the Render map key
	raw  []byte       // original bytes
	docs []*yaml.Node // document roots in stream order
	subs []site
}

// site is one substitution point: the mapping value node of an `image` key,
// rewritten into a scalar image ref by Render.
type site struct {
	node *yaml.Node
	key  string // spec.Key() of the build
	name string // spec.DisplayName(), for Render errors
	line int
}

// Scan parses every template under envDir and extracts the build specs.
// Templates are parsed in lexical order of their envDir-relative slash paths.
//
// Validation errors (missing platform, unknown keys in a jobs-build object,
// broken YAML, image aliases or `<<` merge keys hiding a build object,
// aliased build anchors, ".." path segments) are reported with file and
// line. A `jobs-build` object's fields: name (optional), path (optional,
// default "/"), sources (optional list of other project paths the build may
// reference), build-file (optional), args (optional string map), platform
// (required).
func Scan(envDir string) (*Extraction, error) {
	x := &Extraction{}
	index := map[string]int{} // spec.Key() -> Builds slot
	configPath := filepath.Join(envDir, "assimilate.yaml")
	var rels []string
	err := filepath.WalkDir(envDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != envDir && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || p == configPath {
			return nil
		}
		if ext := filepath.Ext(name); ext != ".yaml" && ext != ".yml" && ext != ".json" {
			return nil
		}
		rel, err := filepath.Rel(envDir, p)
		if err != nil {
			return err
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	// WalkDir's per-directory order diverges from full-path lexical order
	// when a directory name shares a prefix with sibling files (foo/x.yaml
	// would walk before foo-bar.yaml); sort the collected paths instead.
	sort.Strings(rels)
	for _, rel := range rels {
		if err := x.scanFile(index, filepath.Join(envDir, filepath.FromSlash(rel)), rel); err != nil {
			return nil, err
		}
	}
	return x, nil
}

// scanFile decodes one template into document nodes and collects its
// jobs-build objects.
func (x *Extraction) scanFile(index map[string]int, fpath, rel string) error {
	raw, err := os.ReadFile(fpath)
	if err != nil {
		return err
	}
	f := &tmplFile{path: fpath, rel: rel, raw: raw}
	if filepath.Ext(fpath) == ".json" {
		// JSON cannot hold a jobs-build object (no substitution support);
		// the file is emitted verbatim by Render.
		x.files = append(x.files, f)
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("%s: %w", fpath, err)
		}
		f.docs = append(f.docs, &doc)
	}
	for _, doc := range f.docs {
		if err := x.walk(index, f, doc); err != nil {
			return err
		}
	}
	if err := f.checkAliasedSites(); err != nil {
		return err
	}
	x.files = append(x.files, f)
	return nil
}

// checkAliasedSites fails when a substitution site is the target of an alias
// anywhere in the file: Render rewrites the anchored mapping into a scalar in
// place, which would corrupt every alias or `<<` merge of that anchor.
func (f *tmplFile) checkAliasedSites() error {
	if len(f.subs) == 0 {
		return nil
	}
	sites := make(map[*yaml.Node]bool, len(f.subs))
	for _, s := range f.subs {
		sites[s.node] = true
	}
	for _, doc := range f.docs {
		if err := findAliasToSite(f.path, doc, sites); err != nil {
			return err
		}
	}
	return nil
}

// findAliasToSite walks n for an AliasNode whose target is a substitution
// site.
func findAliasToSite(file string, n *yaml.Node, sites map[*yaml.Node]bool) error {
	if n.Kind == yaml.AliasNode && sites[n.Alias] {
		return nodeErrf(file, n.Line, "anchor %q on a jobs-build image object is aliased here; inline the object", n.Value)
	}
	for _, c := range n.Content {
		if err := findAliasToSite(file, c, sites); err != nil {
			return err
		}
	}
	return nil
}

// walk descends n depth-first in node order, capturing every mapping value of
// an `image` key that is itself a mapping with `type: jobs-build`. An image
// value that is an alias is inspected through its target: aliases to a
// jobs-build mapping and image mappings carrying a `<<` merge key (direct or
// via alias) are errors — Render substitutes only inlined objects; every
// other alias stays untouched where it appears.
func (x *Extraction) walk(index map[string]int, f *tmplFile, n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind == yaml.ScalarNode && k.Value == "image" {
				target := v
				if v.Kind == yaml.AliasNode && v.Alias != nil {
					target = v.Alias
				}
				if target.Kind == yaml.MappingNode {
					if mk := mergeKey(target); mk != nil {
						line := mk.Line
						if target != v {
							line = v.Line
						}
						return nodeErrf(f.path, line, "merge keys are not supported in image objects; inline the fields")
					}
					if isJobsBuild(target) {
						if target != v {
							return nodeErrf(f.path, v.Line, "image aliases a jobs-build object (anchor %q); inline the object", v.Value)
						}
						s, err := decodeBuild(f.path, v)
						if err != nil {
							return err
						}
						key := s.Key()
						if _, ok := index[key]; !ok {
							index[key] = len(x.Builds)
							x.Builds = append(x.Builds, s)
						}
						f.subs = append(f.subs, site{node: v, key: key, name: s.DisplayName(), line: v.Line})
						continue
					}
				}
			}
			if err := x.walk(index, f, k); err != nil {
				return err
			}
			if err := x.walk(index, f, v); err != nil {
				return err
			}
		}
		return nil
	}
	for _, c := range n.Content {
		if err := x.walk(index, f, c); err != nil {
			return err
		}
	}
	return nil
}

// mergeKey returns m's `<<` merge key node, or nil. yaml.v3 tags a plain
// `<<` key !!merge; a quoted "<<" is matched by value as well — either shape
// in an image object cannot be substituted correctly.
func mergeKey(m *yaml.Node) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && (k.Tag == "!!merge" || k.Value == "<<") {
			return k
		}
	}
	return nil
}

// isJobsBuild reports whether v is a mapping whose `type` entry is the
// scalar "jobs-build". Any other shape is not a build object and is left
// untouched by Render.
func isJobsBuild(v *yaml.Node) bool {
	if v.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, val := v.Content[i], v.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Value == "type" {
			return val.Kind == yaml.ScalarNode && val.Value == "jobs-build"
		}
	}
	return false
}

// decodeBuild strictly decodes one jobs-build mapping: allowed keys are
// exactly {type, name, path, sources, build-file, args, platform}; unknown
// or duplicate keys are errors; platform is required non-empty; path must
// not contain ".." and is normalized to a cleaned, "/"-prefixed slash path;
// sources are the other project paths the build may reference, normalized
// against the path (spec.NormalizeSources) and not allowed on a root build.
func decodeBuild(file string, m *yaml.Node) (spec.BuildSpec, error) {
	var s spec.BuildSpec
	seen := map[string]bool{}
	platformLine := 0
	var sources []string
	sourcesLine := 0
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			return s, nodeErrf(file, k.Line, "jobs-build image object: non-scalar key")
		}
		if seen[k.Value] {
			return s, nodeErrf(file, k.Line, "jobs-build image object: duplicate key %q", k.Value)
		}
		seen[k.Value] = true
		var err error
		switch k.Value {
		case "type":
			_, err = scalarValue(file, k, v)
		case "name":
			s.Name, err = scalarValue(file, k, v)
		case "path":
			s.Path, err = scalarValue(file, k, v)
			if err == nil && hasDotDot(s.Path) {
				err = nodeErrf(file, v.Line, "jobs-build image object: path must not contain %q", "..")
			}
		case "build-file":
			s.BuildFile, err = scalarValue(file, k, v)
		case "platform":
			s.Platform, err = scalarValue(file, k, v)
			platformLine = v.Line
		case "args":
			s.Args, err = decodeArgs(file, v)
		case "sources":
			sources, err = decodeSources(file, v)
			sourcesLine = k.Line
		default:
			err = nodeErrf(file, k.Line, "jobs-build image object: unknown key %q", k.Value)
		}
		if err != nil {
			return s, err
		}
	}
	if s.Platform == "" {
		line := m.Line
		if platformLine != 0 {
			line = platformLine
		}
		return s, nodeErrf(file, line, "jobs-build image object: platform is required")
	}
	s.Path = normalizePath(s.Path)
	if len(sources) > 0 && s.Path == "/" {
		return s, nodeErrf(file, sourcesLine, "jobs-build image object: sources is not allowed on a root build (path %q already covers the project)", "/")
	}
	s.Sources = spec.NormalizeSources(s.Path, sources)
	return s, nil
}

// decodeSources requires v to be a sequence of non-empty scalar paths without
// ".." that do not name the project root, and returns them cleaned and
// "/"-prefixed (not yet normalized against the build path).
func decodeSources(file string, v *yaml.Node) ([]string, error) {
	if v.Kind != yaml.SequenceNode {
		return nil, nodeErrf(file, v.Line, "jobs-build image object: sources must be a list of paths")
	}
	out := make([]string, 0, len(v.Content))
	for _, item := range v.Content {
		if item.Kind != yaml.ScalarNode || item.Value == "" {
			return nil, nodeErrf(file, item.Line, "jobs-build image object: sources entries must be non-empty strings")
		}
		if hasDotDot(item.Value) {
			return nil, nodeErrf(file, item.Line, "jobs-build image object: sources must not contain %q", "..")
		}
		p := normalizePath(item.Value)
		if p == "/" {
			return nil, nodeErrf(file, item.Line, "jobs-build image object: sources must not name the project root")
		}
		out = append(out, p)
	}
	return out, nil
}

// scalarValue requires v to be a scalar and returns its string value; the
// error names the key k it belongs to.
func scalarValue(file string, k, v *yaml.Node) (string, error) {
	if v.Kind != yaml.ScalarNode {
		return "", nodeErrf(file, v.Line, "jobs-build image object: %s must be a string", k.Value)
	}
	return v.Value, nil
}

// decodeArgs requires v to be a mapping of scalar keys to scalar values.
func decodeArgs(file string, v *yaml.Node) (map[string]string, error) {
	if v.Kind != yaml.MappingNode {
		return nil, nodeErrf(file, v.Line, "jobs-build image object: args must be a mapping of string to string")
	}
	args := make(map[string]string, len(v.Content)/2)
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, val := v.Content[i], v.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			return nil, nodeErrf(file, k.Line, "jobs-build image object: args keys must be strings")
		}
		if val.Kind != yaml.ScalarNode {
			return nil, nodeErrf(file, val.Line, "jobs-build image object: args values must be strings")
		}
		args[k.Value] = val.Value
	}
	return args, nil
}

// hasDotDot reports whether the slash- or backslash-separated path p contains
// a ".." segment. Such a path names a directory outside the one written —
// normalizePath would silently clamp it into the root.
func hasDotDot(p string) bool {
	for _, seg := range strings.Split(strings.ReplaceAll(p, "\\", "/"), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// normalizePath roots and cleans a template source path: "" and "/" mean the
// project root; the result is always a cleaned, "/"-prefixed slash path.
// ".." segments are rejected by decodeBuild before this runs.
func normalizePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return path.Clean("/" + p)
}

// nodeErrf formats a validation error carrying file:line.
func nodeErrf(file string, line int, format string, args ...any) error {
	return fmt.Errorf("%s:%d: %s", file, line, fmt.Sprintf(format, args...))
}

// Render substitutes every jobs-build object with images[spec.Key()] and
// returns the rendered file contents keyed by path relative to envDir.
// A spec key missing from images is an error.
//
// Files without substitutions are returned byte-identical to the source; a
// file with substitutions is re-encoded (indent 2, comments preserved).
func (x *Extraction) Render(images map[string]string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(x.files))
	for _, f := range x.files {
		if len(f.subs) == 0 {
			out[f.rel] = f.raw
			continue
		}
		for _, s := range f.subs {
			ref, ok := images[s.key]
			if !ok {
				return nil, nodeErrf(f.path, s.line, "no image for build %s", s.name)
			}
			setImage(s.node, ref)
		}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		for _, doc := range f.docs {
			if err := enc.Encode(doc); err != nil {
				return nil, fmt.Errorf("%s: %w", f.path, err)
			}
		}
		if err := enc.Close(); err != nil {
			return nil, fmt.Errorf("%s: %w", f.path, err)
		}
		out[f.rel] = buf.Bytes()
	}
	return out, nil
}

// setImage rewrites a jobs-build mapping node in place into a plain string
// scalar holding the image ref, keeping the node's comments and anchor.
func setImage(n *yaml.Node, ref string) {
	*n = yaml.Node{
		Kind:        yaml.ScalarNode,
		Tag:         "!!str",
		Value:       ref,
		HeadComment: n.HeadComment,
		LineComment: n.LineComment,
		FootComment: n.FootComment,
		Anchor:      n.Anchor,
	}
}
