// Package jobs is assimilate's client to a jobs-iroh server: local source
// ingest, source push, build submission, and watch/log streaming.
//
// It is built on jobs-iroh's exported packages (amber, amberclient, builddef,
// importdef, api, wire) and replicates the connection patterns of
// jobs-iroh/tui/client.go and jobs-iroh/clientcli/{apiclient,logtracker}.go:
// one QUIC connection on the jobs-admin/1.0 ALPN serves submit, watch, logs
// and cancel (one request per stream, concurrent streams are safe); one
// amberclient connection on jobs-amber-admin/1.0 pushes source trees. Every
// stream is armed with context.AfterFunc(ctx, …SetDeadline(now)) and fully
// terminated with amberclient.CloseStream, or QUIC MAX_STREAMS credit leaks.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jobs-build/amber-store-core/key"
	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/amberclient"
	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/builddef"
	"github.com/jobs-build/jobs-iroh/gcsweep"
	"github.com/jobs-build/jobs-iroh/importdef"
	"golang.org/x/sys/unix"

	"github.com/jobs-build/assimilate/internal/spec"
)

// The client-facing server ALPNs (frozen contract — jobs-iroh/serve mounts
// these). Local constants, like clientcli's and tui's, so assimilate does not
// import the server composition for the names. The admin ALPN serves the
// whole build surface (submit/watch/logs/cancel), so one API connection
// suffices next to the amber push connection.
const (
	alpnAdmin      = "jobs-admin/1.0"
	alpnAmberAdmin = "jobs-amber-admin/1.0"
)

// scratchPrefix is the MANDATORY prefix of client push refs: the server
// refuses post-submit cleanup of refs outside it.
const scratchPrefix = "client-push/"

// ProgressFunc reports transfer progress (done/total objects).
type ProgressFunc func(done, total int)

// Sink receives one build's progress. Implementations must be safe for
// concurrent calls from multiple follower goroutines.
type Sink interface {
	// State reports a coalesced snapshot: the request phase and a short
	// human counts summary (e.g. "3/7 built · 1 running").
	State(phase string, counts string)
	// Snapshot delivers the raw coalesced watch snapshot (incl. the graph
	// edges NodeSnap.Deps and Cached) alongside State — the TUI folds it
	// into the build's expandable subtree. Plain consumers may ignore it.
	Snapshot(snap api.Snapshot)
	// Log delivers one reassembled output line (chunks split lines at
	// arbitrary byte boundaries; partial lines are buffered until newline).
	// node is the graph node the line belongs to ("" = build-level note);
	// lines carry NO node prefix — consumers add one where views mix nodes.
	Log(node, line string)
}

// Source identifies an ingested source tree. The zero value is usable by
// test fakes; real values come from Ingest.
type Source struct {
	key     key.Key // amber tree root key
	keyStr  string
	valid   bool
	scratch string // client-push/<hex> ref once pushed
}

// String returns the tree key in hex ("" for the zero value).
func (s Source) String() string { return s.keyStr }

// Handle identifies a submitted build.
type Handle struct {
	RequestID string
	K         string // 64-hex build key — the registry image tag
}

// Local is the offline half: the embedded amber store and definition math.
// It is enough to compute image references without any server.
type Local struct {
	store   *amber.Store
	dataDir string
	// gc is the local-store sweeper (jobs-iroh's gcsweep, the same engine
	// jobs-client embeds): its read observer records ref touches while the
	// store is open, and MaybeGC runs the stamp-gated sweep. nil = disabled
	// (JOBS_GC_RETENTION=0 or construction failure).
	gc *gcsweep.Sweeper
}

// Open opens (creating if needed) assimilate's own amber store under dataDir.
// The store's flock is single-process: concurrent assimilate invocations on
// one data dir fail fast with a descriptive error.
func Open(dataDir string) (*Local, error) {
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	st, err := amber.Open(filepath.Join(dataDir, "store"))
	if err != nil {
		// The packstore takes a non-blocking exclusive flock on its
		// directory; a holder elsewhere surfaces as EWOULDBLOCK.
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another assimilate is using %s: %w", dataDir, err)
		}
		return nil, fmt.Errorf("open store under %s: %w", dataDir, err)
	}
	l := &Local{store: st, dataDir: dataDir}
	if ret := gcRetention(); ret > 0 {
		sw, err := gcsweep.New(slog.Default(), st, gcsweep.Options{
			StoreDir:     filepath.Join(dataDir, "store"),
			SnapshotPath: filepath.Join(dataDir, "refaccess.cbor"),
			Retention:    ret,
		})
		if err != nil {
			// GC must never block a deploy: warn and run without it.
			fmt.Fprintf(os.Stderr, "gc disabled for this run: %v\n", err)
		} else {
			l.gc = sw
		}
	}
	return l, nil
}

// Close flushes the GC tracker (sweeper first — it must not touch the
// store after it closes) and releases the store.
func (l *Local) Close() error {
	if l.gc != nil {
		l.gc.Close()
	}
	return l.store.Close()
}

// gcRetention is the local GC expiry window: JOBS_GC_RETENTION (Go
// duration; "0" disables — the same knob jobs-client honors, so one
// setting governs the whole toolchain), default 30 days. An unparsable
// value disables with a warning rather than failing the command.
func gcRetention() time.Duration {
	v := os.Getenv("JOBS_GC_RETENTION")
	if v == "" {
		return 720 * time.Hour
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "ignoring invalid JOBS_GC_RETENTION %q; gc disabled\n", v)
		return 0
	}
	return d
}

// gcCheckEvery is how often MaybeGC is allowed to sweep; the stamp file's
// mtime is the record.
const gcCheckEvery = 24 * time.Hour

// MaybeGC runs the opportunistic local-store sweep: at most once per
// gcCheckEvery (stamp-gated), after a command's main work, while the store
// is still open. Silent unless something was reclaimed; never fails the
// command. The store's exclusive flock means no concurrent reader exists.
func (l *Local) MaybeGC(ctx context.Context) {
	if l.gc == nil {
		return
	}
	stamp := filepath.Join(l.dataDir, "gc.stamp")
	if info, err := os.Stat(stamp); err == nil && time.Since(info.ModTime()) < gcCheckEvery {
		return
	}
	stats, err := l.gc.Sweep(ctx, -1, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gc sweep failed (will retry within %s): %v\n", gcCheckEvery, err)
		return
	}
	_ = os.WriteFile(stamp, nil, 0o644) // mtime is the record; content unused
	if stats.ExpiredLast > 0 || stats.LastCycleFreed > 0 {
		fmt.Fprintf(os.Stderr, "gc: expired %d refs, freed %d bytes\n",
			stats.ExpiredLast, stats.LastCycleFreed)
	}
}

// Ingest ingests the source directory (honoring .amberignore) and returns
// its tree source.
func (l *Local) Ingest(ctx context.Context, dir string) (Source, error) {
	k, err := l.store.IngestSourceDir(ctx, dir)
	if err != nil {
		return Source{}, fmt.Errorf("ingest %s: %w", dir, err)
	}
	return Source{key: k, keyStr: k.String(), valid: true}, nil
}

// DefinitionKey computes the canonical build definition of s over src and
// returns the build key K (64-hex) — the future image tag — without any
// server interaction.
func (l *Local) DefinitionKey(src Source, s spec.BuildSpec) (string, error) {
	_, k, err := definition(src, s)
	if err != nil {
		return "", err
	}
	return k.String(), nil
}

// definition constructs the canonical tree-source build Definition of s over
// src — clientcli's treeDefinition with assimilate's convention baked in:
// the ingested subtree IS the build root, so Dir stays empty. These fields
// are the whole identity; the server derives the same K from the same bytes
// (Submit cross-checks).
func definition(src Source, s spec.BuildSpec) (canon []byte, k key.Key, err error) {
	if !src.valid {
		return nil, key.Key{}, errors.New("jobs: source not ingested")
	}
	params, err := canonicalParams(s.Args)
	if err != nil {
		return nil, key.Key{}, err
	}
	in, err := builddef.TreeInput(src.key)
	if err != nil {
		return nil, key.Key{}, err
	}
	def := builddef.Definition{
		Source:    in,
		Platform:  s.Platform,
		Params:    params,
		BuildFile: s.BuildFile,
	}
	canon, err = def.Canonical()
	if err != nil {
		return nil, key.Key{}, fmt.Errorf("canonicalize definition: %w", err)
	}
	k, err = def.Key()
	if err != nil {
		return nil, key.Key{}, fmt.Errorf("definition key: %w", err)
	}
	return canon, k, nil
}

// canonicalParams folds build args into canonical params CBOR — the exact
// bytes clientcli's parseParams produces for --param key=value, so equal
// args yield equal K across assimilate and jobs-client (cache join).
func canonicalParams(args map[string]string) ([]byte, error) {
	m := map[string]any{}
	for k, v := range args {
		m[k] = v
	}
	p, err := importdef.CanonicalParams(m)
	if err != nil {
		return nil, fmt.Errorf("encode params: %w", err)
	}
	return []byte(p), nil
}

// DefaultDataDir is ASSIMILATE_DATA_DIR or <XDG_DATA_HOME|~/.local/share>/assimilate.
func DefaultDataDir() string {
	if d := os.Getenv("ASSIMILATE_DATA_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "assimilate")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".assimilate-data"
	}
	return filepath.Join(home, ".local", "share", "assimilate")
}

// Options configures Dial.
type Options struct {
	Server string   // server endpoint ID (JOBS_SERVER)
	Addrs  []string // optional direct host:port addresses (JOBS_SERVER_ADDR)
}

// Client is Local plus live connections to one jobs-iroh server. All methods
// are safe for concurrent use: both connections carry one request per stream
// and concurrent streams are safe.
type Client struct {
	*Local
	api     *apiConn            // jobs-admin/1.0: submit, watch, logs, cancel
	amber   *amberclient.Client // jobs-amber-admin/1.0: source pushes
	follows chan struct{}       // client-wide live follow-stream budget
}

// Dial connects to the server: one amberclient on jobs-amber-admin/1.0 for
// pushes, one framed-API connection on jobs-admin/1.0 for everything else.
func Dial(ctx context.Context, l *Local, o Options) (*Client, error) {
	ac, err := amberclient.Dial(ctx, amberclient.Options{
		EndpointID: o.Server, Addrs: o.Addrs, ALPN: alpnAmberAdmin,
	})
	if err != nil {
		return nil, fmt.Errorf("dial %s (amber): %w", o.Server, err)
	}
	conn, ep, err := amberclient.DialConn(ctx, amberclient.Options{
		EndpointID: o.Server, Addrs: o.Addrs, ALPN: alpnAdmin,
	})
	if err != nil {
		_ = ac.Close()
		return nil, fmt.Errorf("dial %s (api): %w", o.Server, err)
	}
	return &Client{
		Local:   l,
		api:     &apiConn{conn: conn, ep: ep},
		amber:   ac,
		follows: make(chan struct{}, maxClientFollows),
	}, nil
}

// Close tears down both connections (the Local store stays open).
func (c *Client) Close() error {
	c.api.close()
	return c.amber.Close()
}

// PushSource pushes src's tree to the server under a fresh client-push/<hex>
// scratch ref (delta transfer — only missing objects cross the wire) and
// records the scratch ref in src for subsequent Submits.
func (c *Client) PushSource(ctx context.Context, src *Source, prog ProgressFunc) error {
	if src == nil || !src.valid {
		return errors.New("jobs: push of an un-ingested source")
	}
	scratch, err := scratchRef()
	if err != nil {
		return err
	}
	var cb amberclient.ProgressFunc
	if prog != nil {
		cb = amberclient.ProgressFunc(prog)
	}
	if _, err := c.amber.PushWithProgress(ctx, c.store, scratch, src.key, cb); err != nil {
		return fmt.Errorf("push source %s: %w", src.keyStr, err)
	}
	src.scratch = scratch
	return nil
}

// scratchRef mints one scratch ref name; the server deletes it after the
// submit commits.
func scratchRef() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return scratchPrefix + hex.EncodeToString(b[:]), nil
}

// Submit submits one build of s over the pushed src. The server echoes K;
// Submit cross-checks it against the locally computed key and fails on
// mismatch.
func (c *Client) Submit(ctx context.Context, src Source, s spec.BuildSpec) (Handle, error) {
	canon, k, err := definition(src, s)
	if err != nil {
		return Handle{}, err
	}
	var sub api.Submitted
	err = c.api.call(ctx, api.TSubmit, api.SubmitRequest{
		Def:        canon,
		ScratchRef: src.scratch,
	}, api.TSubmitted, &sub)
	if err != nil {
		return Handle{}, fmt.Errorf("submit %s: %w", s.DisplayName(), err)
	}
	serverK, err := key.Parse(sub.K)
	if err != nil || serverK != k {
		return Handle{}, fmt.Errorf("submit %s: server key %x does not match local %s", s.DisplayName(), sub.K, k)
	}
	return Handle{RequestID: sub.RequestID, K: k.String()}, nil
}

// Follow watches h to its terminal snapshot, streaming coalesced state and
// log lines into sink, and returns the terminal state (done/failed/
// cancelled). Log follows attach per active node (running first), capped
// per build and globally so N concurrent builds stay under the QUIC
// per-connection live-stream budget (~100). On failure, the stored output
// of failing nodes not already streamed is fetched and delivered to sink,
// and the returned state is StateFailed with a nil error — a non-nil error
// means the outcome is unknown (transport trouble), not that the build
// failed.
func (c *Client) Follow(ctx context.Context, h Handle, sink Sink) (spec.BuildState, error) {
	stream, stop, err := c.api.openRequest(ctx, api.TWatch, api.WatchRequest{RequestID: h.RequestID})
	if err != nil {
		return "", err
	}
	defer stop()
	defer amberclient.CloseStream(stream) // full termination — see apiConn.call
	next := func() (api.Snapshot, error) {
		rt, body, err := api.ReadFrame(stream)
		if err != nil {
			return api.Snapshot{}, err
		}
		var snap api.Snapshot
		if err := decodeReply(rt, body, api.TSnapshot, &snap); err != nil {
			return api.Snapshot{}, err
		}
		return snap, nil
	}
	return followLoop(ctx, next, c.api, c.api, c.follows, sink)
}

// Cancel sends a best-effort cancel for h (used on SIGINT / TUI quit).
func (c *Client) Cancel(ctx context.Context, h Handle) error {
	return c.api.call(ctx, api.TCancel, api.CancelRequest{RequestID: h.RequestID}, api.TOK, nil)
}
