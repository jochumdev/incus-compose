package log

import (
	"context"
	golog "log"
	"log/slog"
	"strings"

	clog "github.com/coredns/coredns/plugin/pkg/log"
)

// Hook routes CoreDNS's logger into slog's default handler, once, before
// anything logs.
//
// plugin/pkg/log writes through the standard library logger and nothing else,
// every level a golog.Print behind a "[LEVEL] " prefix, so the whole of the
// hook-up is a writer that reads the prefix back off. It takes the level because
// clog drops its own debug output before the writer sees it.
func Hook(level slog.Level) {
	if level <= slog.LevelDebug {
		clog.D.Set()
	}

	// No flags: the timestamp and the file position are slog's to add.
	golog.SetFlags(0)
	golog.SetOutput(writer{})
}

// writer turns one std-logger line into one slog record.
type writer struct{}

// levels maps CoreDNS's prefixes onto slog's. FATAL is Error because slog has
// nothing above it, and clog.Fatal calls os.Exit itself.
var levels = []struct {
	prefix string
	level  slog.Level
}{
	{"[DEBUG] ", slog.LevelDebug},
	{"[INFO] ", slog.LevelInfo},
	{"[WARNING] ", slog.LevelWarn},
	{"[ERROR] ", slog.LevelError},
	{"[FATAL] ", slog.LevelError},
}

func (writer) Write(p []byte) (int, error) {
	n := len(p)

	line := strings.TrimSuffix(string(p), "\n")

	// Unprefixed lines are somebody else on the std logger, and still belong in
	// the log.
	level := slog.LevelInfo

	for _, l := range levels {
		if !strings.HasPrefix(line, l.prefix) {
			continue
		}

		level, line = l.level, strings.TrimPrefix(line, l.prefix)

		break
	}

	// "plugin/<name>: " is what clog.NewWithPlugin puts in front of the message.
	// Lifted out, so it is a field to filter on rather than three words of every
	// message.
	attrs := []any{}

	rest, ok := strings.CutPrefix(line, "plugin/")
	if ok {
		name, msg, found := strings.Cut(rest, ": ")
		if found {
			attrs, line = append(attrs, "plugin", name), msg
		}
	}

	slog.Log(context.Background(), level, line, attrs...)

	// The std logger checks this against what it handed over.
	return n, nil
}
