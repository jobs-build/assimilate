package spec

import (
	"reflect"
	"testing"
)

// Regression: the old encoding flattened args as sorted "k=v" joined with
// \x01, so the crafted value "1\x01b=2" collided with the two-entry map
// {a: 1, b: 2} and silently deduped two different builds onto one image.
func TestKeyInjectiveCraftedArgValue(t *testing.T) {
	a := BuildSpec{Path: "/svc", Platform: "linux/amd64", Args: map[string]string{"a": "1\x01b=2"}}
	b := BuildSpec{Path: "/svc", Platform: "linux/amd64", Args: map[string]string{"a": "1", "b": "2"}}
	if a.Key() == b.Key() {
		t.Fatalf("crafted arg value collides: %q", a.Key())
	}
}

func TestKeyInjectiveBoundaries(t *testing.T) {
	pairs := []struct {
		name string
		a, b BuildSpec
	}{
		{"field boundary", BuildSpec{Path: "/a", BuildFile: "b"}, BuildSpec{Path: "/ab", BuildFile: ""}},
		{"arg key/value boundary", BuildSpec{Args: map[string]string{"ab": "c"}}, BuildSpec{Args: map[string]string{"a": "bc"}}},
		{"value/next-key boundary", BuildSpec{Args: map[string]string{"a": "xb", "": "y"}}, BuildSpec{Args: map[string]string{"a": "x", "b": "y"}}},
	}
	for _, p := range pairs {
		if p.a.Key() == p.b.Key() {
			t.Errorf("%s: distinct specs share key %q", p.name, p.a.Key())
		}
	}
}

func TestKeyArgsOrderInsensitive(t *testing.T) {
	a := BuildSpec{Path: "/svc", Platform: "linux/amd64", Args: map[string]string{}}
	b := BuildSpec{Path: "/svc", Platform: "linux/amd64", Args: map[string]string{}}
	for _, k := range []string{"x", "y", "z", "variant"} {
		a.Args[k] = k + "-v"
	}
	for _, k := range []string{"variant", "z", "y", "x"} {
		b.Args[k] = k + "-v"
	}
	if a.Key() != b.Key() {
		t.Fatalf("same args, different keys: %q vs %q", a.Key(), b.Key())
	}
	if a.Key() != a.Key() {
		t.Fatal("Key is not deterministic")
	}
}
func TestNormalizeSources(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		sources []string
		want    []string
	}{
		{"none", "/services/a", nil, nil},
		{"sorted", "/services/a", []string{"/proto", "/lib"}, []string{"/lib", "/proto"}},
		{"duplicates", "/services/a", []string{"/proto", "/proto"}, []string{"/proto"}},
		{"the path itself", "/services/a", []string{"/services/a", "/proto"}, []string{"/proto"}},
		{"inside the path", "/services/a", []string{"/services/a/gen"}, nil},
		{"inside another source", "/services/a", []string{"/lib/x", "/lib"}, []string{"/lib"}},
		{"a sibling with a common prefix stays", "/services/a", []string{"/services/a-b", "/lib", "/lib-2"}, []string{"/lib", "/lib-2", "/services/a-b"}},
		{"a source containing the path stays", "/services/a", []string{"/services"}, []string{"/services"}},
	}
	for _, tt := range tests {
		if got := NormalizeSources(tt.path, tt.sources); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: NormalizeSources(%q, %v) = %v, want %v", tt.name, tt.path, tt.sources, got, tt.want)
		}
	}
}

func TestKeyWithSources(t *testing.T) {
	plain := BuildSpec{Path: "/services/a", Platform: "linux/amd64", Args: map[string]string{"k": "v"}}
	// The key of a spec without sources is the encoding it always had.
	if got, want := plain.Key(), "11:/services/a0:11:linux/amd641:k1:v"; got != want {
		t.Errorf("Key without sources = %q, want %q", got, want)
	}
	withSources := plain
	withSources.Sources = []string{"/proto"}
	other := plain
	other.Sources = []string{"/lib"}
	if withSources.Key() == plain.Key() || withSources.Key() == other.Key() {
		t.Errorf("keys collide: %q %q %q", plain.Key(), withSources.Key(), other.Key())
	}
	// A sources list must not read as args.
	asArgs := BuildSpec{Path: "/services/a", Platform: "linux/amd64", Args: map[string]string{"k": "v", "/proto": "/lib"}}
	twoSources := plain
	twoSources.Sources = []string{"/lib", "/proto"}
	if asArgs.Key() == twoSources.Key() {
		t.Errorf("args and sources encode alike: %q", asArgs.Key())
	}
}

func TestSourceKeyAndKeep(t *testing.T) {
	a := BuildSpec{Path: "/services/a", Platform: "linux/amd64", Sources: []string{"/lib", "/proto"}}
	sameSource := BuildSpec{Path: "/services/a", Platform: "linux/arm64", BuildFile: "BUILD.prod", Sources: []string{"/lib", "/proto"}}
	if a.SourceKey() != sameSource.SourceKey() {
		t.Error("specs that differ only in platform and recipe must share a source")
	}
	for name, other := range map[string]BuildSpec{
		"no sources":    {Path: "/services/a"},
		"other sources": {Path: "/services/a", Sources: []string{"/lib"}},
		"other path":    {Path: "/services/b", Sources: []string{"/lib", "/proto"}},
	} {
		if other.SourceKey() == a.SourceKey() {
			t.Errorf("%s: SourceKey collides", name)
		}
	}
	if got, want := a.Keep(), []string{"lib", "proto", "services/a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Keep = %v, want %v", got, want)
	}
	// A source that contains the path covers it.
	wide := BuildSpec{Path: "/services/a", Sources: []string{"/proto", "/services"}}
	if got, want := wide.Keep(), []string{"proto", "services"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Keep = %v, want %v", got, want)
	}
}
