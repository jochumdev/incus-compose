package shared

import (
	"log/slog"
	"strings"
)

// LevelTrace is one step below slog.LevelDebug.
const LevelTrace = slog.LevelDebug - 4

// StringToSlogLevel parses a string level to an slog.Level.
func StringToSlogLevel(l string) slog.Level {
	switch strings.ToUpper(l) {
	case "TRACE":
		return LevelTrace
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
