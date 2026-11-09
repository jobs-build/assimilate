package ui

import (
	"io"
	"log"
	"log/slog"
	"strings"
	"testing"
)

// captureLogs must route every Info+ record of the default slog logger (and
// of the std log package, which slog.SetDefault re-wires) into the callback
// and put both loggers back exactly on restore.
func TestCaptureLogsRoutesDefaultLoggers(t *testing.T) {
	prevWriter, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(io.Discard) // keep the post-restore probe out of the test output
	t.Cleanup(func() { log.SetOutput(prevWriter); log.SetFlags(prevFlags) })
	prevSlog := slog.Default()
	discardWriter, discardFlags := log.Writer(), log.Flags()

	var got []string
	restore := captureLogs(func(line string) { got = append(got, line) })

	slog.Info("amberclient: sharded transfers disabled", "reason", "relayed")
	slog.Warn("amberclient: shard attach failed", "err", "timeout")
	slog.Debug("amberclient connected") // below Info: dropped
	slog.Default().With("conn", 1).WithGroup("peer").Info("punch landed", "addr", "1.2.3.4")
	log.Print("quic: something")

	want := []string{
		"amberclient: sharded transfers disabled reason=relayed",
		"WARN amberclient: shard attach failed err=timeout",
		"punch landed conn=1 peer.addr=1.2.3.4",
		"quic: something",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("captured:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	restore()
	if slog.Default() != prevSlog {
		t.Error("restore did not put the previous slog default back")
	}
	if log.Writer() != discardWriter || log.Flags() != discardFlags {
		t.Error("restore did not put the std log writer/flags back")
	}
	n := len(got)
	slog.Info("after restore")
	log.Print("after restore")
	if len(got) != n {
		t.Errorf("records after restore still reach the callback: %q", got[n:])
	}
}
