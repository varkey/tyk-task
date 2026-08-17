// Package logging provides a minimal leveled wrapper around the standard
// library "log" package, shared by main and every internal package that logs
// (internal/api, internal/authn). A single process-wide level - set once in
// main from the --log-level flag - decides what actually reaches the output
// stream:
//
//   - Debug: high-volume/verbose detail, off by default. Access logging (see
//     internal/api's accessLog) lives here so `kubectl logs` isn't dominated
//     by one line per request unless someone opts in to debug it.
//   - Info: normal operational milestones (startup, shutdown, state changes).
//   - Warn: an expected-but-notable failure, e.g. a rejected/denied caller.
//   - Error: something this service itself couldn't do that it expected to.
//
// Warn and Error are always visible at the default level (Info), so
// authentication/authorization failures are never silently dropped by a log
// level choice - only the verbose access log is level-gated.
package logging

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
)

// Level orders log severity; higher values are more severe.
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// ParseLevel parses the case-insensitive level names accepted by the
// --log-level flag.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q: must be one of debug, info, warn, error", s)
	}
}

// current defaults to LevelInfo before main ever calls SetLevel, so packages
// that log during init (none currently do, but might) fail safe to the
// normal default rather than the noisiest one.
var current atomic.Int32

func init() {
	current.Store(int32(LevelInfo))
}

// SetLevel changes the process-wide log level. Typically called once, early
// in main, from the parsed --log-level flag.
func SetLevel(l Level) {
	current.Store(int32(l))
}

// Enabled reports whether a message at level l would currently be logged.
func Enabled(l Level) bool {
	return int32(l) >= current.Load()
}

func logf(l Level, format string, args ...any) {
	if !Enabled(l) {
		return
	}
	log.Printf("%-5s "+format, append([]any{l.String()}, args...)...)
}

// Debugf logs verbose detail, visible only when --log-level=debug.
func Debugf(format string, args ...any) { logf(LevelDebug, format, args...) }

// Infof logs a normal operational milestone.
func Infof(format string, args ...any) { logf(LevelInfo, format, args...) }

// Warnf logs an expected-but-notable failure, e.g. a rejected/denied caller.
// Always visible at the default level.
func Warnf(format string, args ...any) { logf(LevelWarn, format, args...) }

// Errorf logs something this service itself couldn't do that it expected to.
// Always visible at the default level.
func Errorf(format string, args ...any) { logf(LevelError, format, args...) }
