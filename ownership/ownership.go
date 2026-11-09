// Package ownership marks rendered manifest files as assimilate-generated so
// later runs can tell their own output apart from files a human wrote or
// edited, and from files another source repository generated. YAML files
// carry the marker inline as a header of first-line comments; JSON cannot
// hold comments, so a JSON file's marker lives in a sidecar file next to it.
// The marker records the SHA-256 of the file body — a present, matching hash
// means "assimilate wrote this and nobody touched it since" — and the
// assimilate-domain, the name of the source repository the file was rendered
// from, so pruning only ever removes one domain's own stale files.
package ownership

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// MarkerPrefix is the prefix of the hash header line. The full line is
// MarkerPrefix + <hex sha256> + "\n".
const MarkerPrefix = "# assimilate-hash: "

// DomainPrefix is the prefix of the domain header line, written after the
// hash line. The full line is DomainPrefix + <domain> + "\n".
const DomainPrefix = "# assimilate-domain: "

// SidecarExt is appended to a JSON file path to form its marker sidecar path.
const SidecarExt = ".assimilate"

// isYAML reports whether path has a YAML extension.
func isYAML(path string) bool {
	ext := filepath.Ext(path)
	return ext == ".yaml" || ext == ".yml"
}

// isJSON reports whether path has a JSON extension.
func isJSON(path string) bool {
	return filepath.Ext(path) == ".json"
}

// header is the parsed marker: the hash and domain lines (either may be
// absent) and the remainder after them.
type header struct {
	hash, domain string
	found        bool   // at least one header line was present
	rest         []byte // everything after the header lines
}

// parseHeader splits the leading marker lines off b. Header lines are the
// consecutive leading lines starting with MarkerPrefix or DomainPrefix, in
// any order; a final header line without a trailing newline still counts.
func parseHeader(b []byte) header {
	h := header{rest: b}
	for {
		var prefix string
		switch {
		case bytes.HasPrefix(h.rest, []byte(MarkerPrefix)):
			prefix = MarkerPrefix
		case bytes.HasPrefix(h.rest, []byte(DomainPrefix)):
			prefix = DomainPrefix
		default:
			return h
		}
		h.found = true
		line, rest, _ := bytes.Cut(h.rest, []byte{'\n'})
		value := strings.TrimSpace(string(line[len(prefix):]))
		if prefix == MarkerPrefix {
			h.hash = value
		} else {
			h.domain = value
		}
		h.rest = rest
	}
}

// formatHeader renders the marker lines for hash and domain; an empty domain
// yields the hash line alone.
func formatHeader(hash, domain string) []byte {
	var b bytes.Buffer
	b.WriteString(MarkerPrefix)
	b.WriteString(hash)
	b.WriteByte('\n')
	if domain != "" {
		b.WriteString(DomainPrefix)
		b.WriteString(domain)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// StripMarker returns the portion of body that participates in the hash.
// For YAML files, the leading marker lines are removed. All other inputs are
// returned unchanged.
func StripMarker(path string, body []byte) []byte {
	if !isYAML(path) {
		return body
	}
	return parseHeader(body).rest
}

// ComputeBodyHash returns the lowercase-hex SHA-256 of the hash-relevant
// portion of body (see StripMarker).
func ComputeBodyHash(path string, body []byte) string {
	sum := sha256.Sum256(StripMarker(path, body))
	return hex.EncodeToString(sum[:])
}

// WriteMarked writes body to path and records assimilate's ownership marker:
// the body hash and, unless empty, the domain. For YAML files, the marker is
// prepended as comment lines. For JSON files, the marker is stored in a
// sidecar file named <path>+SidecarExt. Parent directories are created as
// needed.
func WriteMarked(path string, body []byte, domain string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	hdr := formatHeader(ComputeBodyHash(path, body), domain)

	switch {
	case isYAML(path):
		out := make([]byte, 0, len(hdr)+len(body))
		out = append(out, hdr...)
		out = append(out, body...)
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return fmt.Errorf("write yaml %s: %w", path, err)
		}
	case isJSON(path):
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return fmt.Errorf("write json %s: %w", path, err)
		}
		if err := os.WriteFile(path+SidecarExt, hdr, 0o644); err != nil {
			return fmt.Errorf("write sidecar %s: %w", path+SidecarExt, err)
		}
	default:
		return fmt.Errorf("WriteMarked: unsupported extension for %s", path)
	}
	return nil
}

// Remove deletes path; for a JSON file its marker sidecar goes with it (a
// missing sidecar is not an error).
func Remove(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	if isJSON(path) {
		if err := os.Remove(path + SidecarExt); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove sidecar %s: %w", path+SidecarExt, err)
		}
	}
	return nil
}

// FileStatus describes a target path's ownership state.
type FileStatus struct {
	// Exists reports whether the file is present on disk.
	Exists bool
	// Owned reports whether an assimilate marker is present (YAML header or
	// JSON sidecar). False when Exists is false.
	Owned bool
	// Matches reports whether the recorded marker hash equals the current
	// body's hash. False when Owned is false or when the marker is malformed.
	Matches bool
	// Domain is the assimilate-domain recorded in the marker; "" when the
	// marker predates domains or Owned is false.
	Domain string
}

// Status inspects path and returns its ownership status. A missing file
// produces FileStatus{} (all false) with a nil error. I/O errors other than
// "not exist" are returned.
func Status(path string) (FileStatus, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return FileStatus{}, nil
	}
	if err != nil {
		return FileStatus{}, fmt.Errorf("read %s: %w", path, err)
	}

	st := FileStatus{Exists: true}

	h, owned, err := readMarker(path, body)
	if err != nil {
		return FileStatus{}, err
	}
	st.Owned = owned
	if !owned {
		return st, nil
	}
	st.Domain = h.domain

	if !isHex64(h.hash) {
		return st, nil // owned but malformed → Matches stays false
	}

	st.Matches = ComputeBodyHash(path, body) == h.hash
	return st, nil
}

// readMarker returns the parsed marker and whether one was found. For YAML,
// the marker is the leading comment header. For JSON, the marker is the
// sidecar file, which holds the same header lines — or, from before domains
// existed, the bare hex hash — and body is unused.
func readMarker(path string, body []byte) (h header, owned bool, err error) {
	switch {
	case isYAML(path):
		h = parseHeader(body)
		return h, h.found, nil
	case isJSON(path):
		side, err := os.ReadFile(path + SidecarExt)
		if errors.Is(err, os.ErrNotExist) {
			return header{}, false, nil
		}
		if err != nil {
			return header{}, false, fmt.Errorf("read sidecar %s: %w", path+SidecarExt, err)
		}
		h = parseHeader(side)
		if !h.found {
			h.hash = strings.TrimSpace(string(side)) // legacy sidecar: hash only
		}
		return h, true, nil
	default:
		return header{}, false, nil
	}
}

// Scan walks dir recursively and returns the status of every YAML and JSON
// file under it, keyed by slash-separated path relative to dir. Other files
// and any .git directory are skipped; a missing dir yields an empty map.
func Scan(dir string) (map[string]FileStatus, error) {
	out := map[string]FileStatus{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, os.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !isYAML(p) && !isJSON(p) {
			return nil
		}
		st, err := Status(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = st
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}
	return out, nil
}

// isHex64 reports whether s is exactly 64 lowercase hex characters.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
