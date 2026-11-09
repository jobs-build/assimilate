package ownership

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStripMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		in   string
		want string
	}{
		{"yaml without marker", "x.yaml", "apiVersion: v1\n", "apiVersion: v1\n"},
		{"yaml with marker", "x.yaml", MarkerPrefix + "abc\napiVersion: v1\n", "apiVersion: v1\n"},
		{"yml with marker", "x.yml", MarkerPrefix + "abc\nkind: X\n", "kind: X\n"},
		{"yaml with hash and domain", "x.yaml", MarkerPrefix + "abc\n" + DomainPrefix + "mono\nkind: X\n", "kind: X\n"},
		{"json unchanged", "x.json", `{"a":1}` + "\n", `{"a":1}` + "\n"},
		{"yaml only marker no body", "x.yaml", MarkerPrefix + "abc\n", ""},
		{"yaml marker without newline", "x.yaml", MarkerPrefix + "abc", ""},
		{"yaml hash and domain no body", "x.yaml", MarkerPrefix + "abc\n" + DomainPrefix + "mono", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripMarker(tc.path, []byte(tc.in)); string(got) != tc.want {
				t.Fatalf("StripMarker(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestComputeBodyHash(t *testing.T) {
	body := []byte("apiVersion: v1\nkind: ConfigMap\n")
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	// Marked and unmarked YAML hash identically: the header never hashes
	// itself. JSON hashes the whole body (the marker lives in the sidecar).
	for name, got := range map[string]string{
		"yaml unmarked":      ComputeBodyHash("x.yaml", body),
		"yaml marked":        ComputeBodyHash("x.yaml", append([]byte(MarkerPrefix+"deadbeef\n"), body...)),
		"yaml marked+domain": ComputeBodyHash("x.yaml", append([]byte(MarkerPrefix+"deadbeef\n"+DomainPrefix+"mono\n"), body...)),
		"json":               ComputeBodyHash("x.json", body),
	} {
		if got != want {
			t.Errorf("%s hash = %s, want %s", name, got, want)
		}
	}
}

func TestWriteMarkedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deploy.yaml")
	body := []byte("apiVersion: apps/v1\nkind: Deployment\n")

	if err := WriteMarked(path, body, "mono"); err != nil {
		t.Fatalf("WriteMarked: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := MarkerPrefix + ComputeBodyHash(path, body) + "\n" + DomainPrefix + "mono\n" + string(body)
	if string(written) != want {
		t.Fatalf("file = %q, want %q", written, want)
	}
}

// An empty domain writes the pre-domain header: just the hash line.
func TestWriteMarkedYAMLNoDomain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.yaml")
	body := []byte("kind: Deployment\n")

	if err := WriteMarked(path, body, ""); err != nil {
		t.Fatalf("WriteMarked: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := MarkerPrefix + ComputeBodyHash(path, body) + "\n" + string(body)
	if string(written) != want {
		t.Fatalf("file = %q, want %q", written, want)
	}
}

func TestWriteMarkedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := []byte(`{"a":1}` + "\n")

	if err := WriteMarked(path, body, "mono"); err != nil {
		t.Fatalf("WriteMarked: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(body) {
		t.Fatalf("JSON body modified: got %q, want %q", written, body)
	}
	sidecar, err := os.ReadFile(path + SidecarExt)
	if err != nil {
		t.Fatal(err)
	}
	if want := MarkerPrefix + ComputeBodyHash(path, body) + "\n" + DomainPrefix + "mono\n"; string(sidecar) != want {
		t.Fatalf("sidecar = %q, want %q", sidecar, want)
	}
}

func TestWriteMarkedUnsupportedExtension(t *testing.T) {
	if err := WriteMarked(filepath.Join(t.TempDir(), "x.txt"), []byte("hi"), "mono"); err == nil {
		t.Fatal("no error for unsupported extension")
	}
}

func TestStatus(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mark := func(name, body, domain string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := WriteMarked(p, []byte(body), domain); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ownedYAML := mark("owned.yaml", "kind: X\n", "mono")
	legacyYAML := mark("legacy.yaml", "kind: X\n", "")
	ownedJSON := mark("owned.json", `{"a":1}`+"\n", "mono")
	editedJSON := mark("edited.json", `{"a":1}`+"\n", "mono")
	if err := os.WriteFile(editedJSON, []byte(`{"a":2}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sidecar written before domains existed: the bare hex hash.
	legacyJSON := write("legacy.json", `{"a":1}`+"\n")
	write("legacy.json"+SidecarExt, ComputeBodyHash("x.json", []byte(`{"a":1}`+"\n"))+"\n")

	for _, tc := range []struct {
		name string
		path string
		want FileStatus
	}{
		{"missing", filepath.Join(dir, "gone.yaml"), FileStatus{}},
		{"owned yaml", ownedYAML, FileStatus{Exists: true, Owned: true, Matches: true, Domain: "mono"}},
		{"legacy yaml", legacyYAML, FileStatus{Exists: true, Owned: true, Matches: true}},
		{"owned json", ownedJSON, FileStatus{Exists: true, Owned: true, Matches: true, Domain: "mono"}},
		{"legacy json", legacyJSON, FileStatus{Exists: true, Owned: true, Matches: true}},
		{"unmarked yaml", write("plain.yaml", "kind: X\n"), FileStatus{Exists: true}},
		{"unmarked json", write("plain.json", `{"a":1}`), FileStatus{Exists: true}},
		{"edited yaml", write("edited.yaml", MarkerPrefix+ComputeBodyHash("x.yaml", []byte("a: 1\n"))+"\n"+DomainPrefix+"mono\na: 2\n"), FileStatus{Exists: true, Owned: true, Domain: "mono"}},
		{"edited json", editedJSON, FileStatus{Exists: true, Owned: true, Domain: "mono"}},
		{"malformed marker", write("bad.yaml", MarkerPrefix+"not-hex\nkind: X\n"), FileStatus{Exists: true, Owned: true}},
		{"domain line only", write("domainonly.yaml", DomainPrefix+"mono\nkind: X\n"), FileStatus{Exists: true, Owned: true, Domain: "mono"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Status(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if st != tc.want {
				t.Fatalf("Status = %+v, want %+v", st, tc.want)
			}
		})
	}
}

// Scan lists every YAML/JSON file under dir (recursively, slash-relative)
// with its status, ignoring other extensions and the .git directory.
func TestScan(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, domain := range map[string]string{
		"api.yaml":            "mono",
		"workers/queue.yml":   "other",
		"workers/config.json": "mono",
	} {
		if err := WriteMarked(filepath.Join(dir, filepath.FromSlash(name)), []byte("body\n"), domain); err != nil {
			t.Fatal(err)
		}
	}
	write("plain.yaml", "kind: X\n")
	write("notes.txt", "ignored\n")
	write("README.md", "ignored\n")
	write(".git/config.yaml", "kind: X\n")
	write(".git/objects/x.json", "{}\n")

	got, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]FileStatus{
		"api.yaml":            {Exists: true, Owned: true, Matches: true, Domain: "mono"},
		"workers/queue.yml":   {Exists: true, Owned: true, Matches: true, Domain: "other"},
		"workers/config.json": {Exists: true, Owned: true, Matches: true, Domain: "mono"},
		"plain.yaml":          {Exists: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Scan = %+v, want %+v", got, want)
	}
}

func TestScanMissingDir(t *testing.T) {
	got, err := Scan(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan of missing dir = %+v, want empty", got)
	}
}

// Remove deletes a marked file; a JSON file goes together with its sidecar.
func TestRemove(t *testing.T) {
	dir := t.TempDir()
	yaml := filepath.Join(dir, "api.yaml")
	json := filepath.Join(dir, "config.json")
	for _, p := range []string{yaml, json} {
		if err := WriteMarked(p, []byte("body\n"), "mono"); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{yaml, json} {
		if err := Remove(p); err != nil {
			t.Fatalf("Remove(%s): %v", p, err)
		}
	}
	for _, p := range []string{yaml, json, json + SidecarExt} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present (err=%v)", p, err)
		}
	}
	// A JSON file without a sidecar is removed without complaint.
	bare := filepath.Join(dir, "bare.json")
	if err := os.WriteFile(bare, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Remove(bare); err != nil {
		t.Fatalf("Remove(bare json): %v", err)
	}
}
