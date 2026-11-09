package ui

import (
	"context"
	"log"
	"log/slog"
	"strings"
)

// captureLogs re-points the process's default loggers — slog.Default and,
// through slog.SetDefault, the std log package — at send for as long as the
// TUI owns the terminal. Both write to stderr by default, and on a terminal
// stderr is the very screen the alt-screen TUI draws on: a library log line
// (amberclient's relay/sharding notes during a push, go-iroh's router
// warnings) would land mid-frame and stay there until the next full
// repaint. Records below Info are dropped, as the default handler drops
// them. restore puts the previous loggers back exactly — slog.SetDefault
// does not undo its rewiring of the log package on its own.
func captureLogs(send func(line string)) (restore func()) {
	prev := slog.Default()
	prevWriter, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(&noteHandler{send: send}))
	return func() {
		slog.SetDefault(prev)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	}
}

// noteHandler renders each record as one footer line — "LEVEL msg k=v …",
// the level omitted at Info — and hands it to send. Safe for concurrent
// use as long as send is.
type noteHandler struct {
	send  func(string)
	attrs string // pre-rendered " k=v" of With(…) attrs
	group string // "a.b." key prefix from WithGroup
}

func (h *noteHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

func (h *noteHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	if r.Level >= slog.LevelWarn {
		b.WriteString(r.Level.String())
		b.WriteByte(' ')
	}
	b.WriteString(r.Message)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		h.writeAttr(&b, a)
		return true
	})
	h.send(b.String())
	return nil
}

func (h *noteHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range attrs {
		h.writeAttr(&b, a)
	}
	return &noteHandler{send: h.send, attrs: b.String(), group: h.group}
}

func (h *noteHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &noteHandler{send: h.send, attrs: h.attrs, group: h.group + name + "."}
}

// writeAttr appends " key=value" (group-prefixed), flattening group values
// and skipping empty attrs, as slog's handler contract asks.
func (h *noteHandler) writeAttr(b *strings.Builder, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		sub := &noteHandler{group: h.group}
		if a.Key != "" {
			sub.group += a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			sub.writeAttr(b, ga)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(h.group)
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(a.Value.String())
}
