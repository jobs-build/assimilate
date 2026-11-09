package jobs

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/jobs-build/jobs-iroh/amberclient"
	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"
	"github.com/tmc/go-iroh/iroh"
)

// apiConn is the one framed-API connection (admin ALPN) on its own ephemeral
// client endpoint — clientcli's apiConn. Streams are one-request-each (the
// api contract); concurrent streams on one connection are safe.
type apiConn struct {
	conn *iroh.Conn
	ep   *iroh.Endpoint
}

// close tears down the connection and its endpoint.
func (a *apiConn) close() {
	_ = a.conn.Close()
	_ = a.ep.Shutdown(context.Background())
}

// openRequest opens one stream and sends its single request frame. The
// returned stop arms ctx cancellation (deadline-expiry, like amberclient's
// streams) and must be deferred alongside the stream close.
func (a *apiConn) openRequest(ctx context.Context, t string, body any) (net.Conn, func(), error) {
	stream, err := a.conn.OpenStreamConn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s stream: %w", t, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.SetDeadline(time.Now()) })
	if err := api.WriteFrame(stream, t, body); err != nil {
		stop()
		amberclient.CloseStream(stream)
		return nil, nil, fmt.Errorf("send %s: %w", t, err)
	}
	return stream, func() { stop() }, nil
}

// call is the one-shot request/reply exchange: send t, read exactly one
// frame, decode it into reply (nil for bodyless replies like ok).
func (a *apiConn) call(ctx context.Context, t string, body any, wantType string, reply any) error {
	stream, stop, err := a.openRequest(ctx, t, body)
	if err != nil {
		return err
	}
	defer stop()
	// Full termination (send FIN + receive cancel): one-shot request streams
	// are never read to EOF by either side, and only fully closed streams
	// hand their MAX_STREAMS credit back — a deploy's many submits, cancels
	// and log fetches would otherwise stall on the 101st call.
	defer amberclient.CloseStream(stream)
	rt, rb, err := api.ReadFrame(stream)
	if err != nil {
		return fmt.Errorf("%s: read reply: %w", t, err)
	}
	return decodeReply(rt, rb, wantType, reply)
}

// decodeReply maps a reply frame onto the expected type, surfacing server
// api.Error frames as Go errors.
func decodeReply(rt string, body cbor.RawMessage, wantType string, reply any) error {
	if rt == api.TError {
		var e api.Error
		if err := api.DecodeBody(body, &e); err != nil {
			return fmt.Errorf("undecodable server error frame: %w", err)
		}
		return fmt.Errorf("server: %s: %s", e.Code, e.Text)
	}
	if rt != wantType {
		return fmt.Errorf("unexpected reply frame %q (want %q)", rt, wantType)
	}
	if reply == nil {
		return nil
	}
	return api.DecodeBody(body, reply)
}

// logOpener opens one node's log stream (stored view first, then live chunks
// while follow); *apiConn implements it, tests fake it.
type logOpener interface {
	openLogs(ctx context.Context, node string, follow bool) (api.LogView, func() (wire.LogChunk, error), func(), error)
}

// openLogs performs the logs frame exchange on its own stream: the stored
// view arrives synchronously, subsequent chunks through the returned reader.
// The returned done disarms ctx and fully terminates the stream.
func (a *apiConn) openLogs(ctx context.Context, node string, follow bool) (api.LogView, func() (wire.LogChunk, error), func(), error) {
	stream, stop, err := a.openRequest(ctx, api.TLogs, api.LogsRequest{Node: node, Follow: follow})
	if err != nil {
		return api.LogView{}, nil, nil, err
	}
	done := func() {
		stop()
		amberclient.CloseStream(stream)
	}
	rt, rb, err := api.ReadFrame(stream)
	if err != nil {
		done()
		return api.LogView{}, nil, nil, fmt.Errorf("logs: read view: %w", err)
	}
	var view api.LogView
	if err := decodeReply(rt, rb, api.TLogView, &view); err != nil {
		done()
		return api.LogView{}, nil, nil, err
	}
	next := func() (wire.LogChunk, error) {
		rt, rb, err := api.ReadFrame(stream)
		if err != nil {
			return wire.LogChunk{}, err
		}
		var chunk wire.LogChunk
		if err := decodeReply(rt, rb, api.TLogChunk, &chunk); err != nil {
			return wire.LogChunk{}, err
		}
		return chunk, nil
	}
	return view, next, done, nil
}

// logFetcher fetches one node's stored log view; *apiConn implements it,
// tests fake it.
type logFetcher interface {
	fetchLogs(ctx context.Context, node string) (api.LogView, error)
}

// fetchLogs performs one logs frame exchange (stored view, no follow).
func (a *apiConn) fetchLogs(ctx context.Context, node string) (api.LogView, error) {
	var view api.LogView
	err := a.call(ctx, api.TLogs, api.LogsRequest{Node: node}, api.TLogView, &view)
	return view, err
}
