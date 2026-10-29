// Package logbridge gives the standard library logger a severity.
//
// The relay engine logs through log.Printf, which has no levels, so a log viewer
// can only label every line "unknown" and an operator cannot filter, alert on, or
// even skim for the lines that matter. Replacing every call site with a typed
// logger would touch every package; this keeps the call sites and lets each one
// say how serious it is by how its message starts:
//
//	log.Printf("WARN [SendEthTx] nonce too low (attempt %d/%d)", ...)  // warn
//	log.Printf("[SendEthTx] tx %s confirmed in block %d", ...)         // info
//
// Install routes the standard logger into zap, which writes the level as a field.
// The token is removed from the message first, so the logged text is the text that
// was always there: docs/E2E.md and scripts/ match on it, and that must not move.
//
// Choosing a level is a statement about the operator, not about verbosity:
//
//	ERROR   a person has to act or look: a packet dropped that nothing else
//	        refunds (an ack or a timeout), state that could not be made durable and
//	        that no later pass re-derives from the chain (an owed acknowledgement, a
//	        settlement tombstone, retry state), an operation abandoned, a recovered
//	        panic.
//	WARN    something went wrong or is degraded and the relayer is handling it
//	        itself: a retry, a fallback, a skipped check, a resubscribe, a dropped
//	        send the timeout scanner refunds, a failed write that the next pass or a
//	        restart scan repeats (a recovery cursor, a packet tracked before it is
//	        relayed).
//	(none)  lifecycle, progress, and every state change a packet goes through.
//	DEBUG   repeats with nothing new to say. Never for a line docs/ or scripts/
//	        tell someone to look for: the default level hides it.
//
// There is deliberately no INFO token. Info is what an unmarked line is, and a
// second way to spell it is a second way to get it wrong.
package logbridge

import (
	"log"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// The level tokens. One must be the first thing in the message, followed by a
// single space; anything else is part of the message.
const (
	Debug = "DEBUG "
	Warn  = "WARN "
	Error = "ERROR "
)

// callerSkip is how many frames sit between zap and the code that called
// log.Printf: this file's Write, the standard logger's output, and Printf itself.
// With it the "caller" field names the log statement instead of this file.
// TestInstall_CallerIsTheLogStatement fails if a Go release changes the depth.
const callerSkip = 3

// split separates a leading level token from the message. A message without one
// is info, and is returned unchanged.
func split(msg string) (zapcore.Level, string) {
	for _, t := range [...]struct {
		token string
		level zapcore.Level
	}{
		{Debug, zapcore.DebugLevel},
		{Warn, zapcore.WarnLevel},
		{Error, zapcore.ErrorLevel},
	} {
		if rest, ok := strings.CutPrefix(msg, t.token); ok {
			return t.level, rest
		}
	}
	return zapcore.InfoLevel, msg
}

// writer is the standard logger's output. Every goroutine that logs reaches Write
// through the standard logger's own mutex, and zap is safe for concurrent use, so
// it holds no state of its own.
type writer struct{ logger *zap.Logger }

func (w writer) Write(p []byte) (int, error) {
	level, msg := split(strings.TrimSuffix(string(p), "\n"))
	switch level {
	case zapcore.DebugLevel:
		w.logger.Debug(msg)
	case zapcore.WarnLevel:
		w.logger.Warn(msg)
	case zapcore.ErrorLevel:
		w.logger.Error(msg)
	default:
		w.logger.Info(msg)
	}
	return len(p), nil
}

// Install makes logger the destination of the standard library's default logger,
// and so of every *log.Logger the relayer injects, which are all log.Default().
//
// It clears the standard logger's flags and prefix. zap stamps the time itself and
// a relay path travels as a field on the logger, so leaving either set would print
// it a second time inside the message.
//
// The returned function puts the previous output, flags and prefix back. Tests need
// that; the relayer runs on one logger for the life of the process and never calls it.
func Install(logger *zap.Logger) (restore func()) {
	flags, prefix, out := log.Flags(), log.Prefix(), log.Writer()
	log.SetFlags(0)
	log.SetPrefix("")
	log.SetOutput(writer{logger: logger.WithOptions(zap.AddCallerSkip(callerSkip))})
	return func() {
		log.SetFlags(flags)
		log.SetPrefix(prefix)
		log.SetOutput(out)
	}
}
