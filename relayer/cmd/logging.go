// Logging setup: how loud the relayer is, and what its lines look like.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	flagLogLevel  = "log-level"
	flagLogFormat = "log-format"

	// Namespaced like RELAYER_BENCHMARK: a bare LOG_LEVEL is exported by enough
	// other tools in a devnet shell to change this process's verbosity by accident.
	envLogLevel  = "RELAYER_LOG_LEVEL"
	envLogFormat = "RELAYER_LOG_FORMAT"
)

const (
	logFormatAuto    = "auto"
	logFormatJSON    = "json"
	logFormatConsole = "console"
)

// logSettings is what the two logging flags and their environment variables
// resolve to.
type logSettings struct {
	level zapcore.Level
	// format is logFormatJSON or logFormatConsole, never auto: auto is decided from
	// the terminal before this is built.
	format string
	// ignored words each setting that was present but unusable, ready to log. They
	// cannot be logged where they are found, because the logger they would go
	// through is the thing being built.
	ignored []string
}

// registerLogFlags declares the logging flags on cmd. They are persistent so every
// subcommand accepts them and --help lists them.
//
// Their values are not read back from cobra. The logger has to exist before the
// commands are built -- each one captures it -- and cobra only parses flags inside
// Execute, so resolveLogSettings reads the arguments itself, ahead of time. The
// declaration is what makes cobra accept the flags and what puts them in --help.
func registerLogFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().String(flagLogLevel, "",
		"minimum log level: debug, info, warn or error (or set "+envLogLevel+"); default info")
	cmd.PersistentFlags().String(flagLogFormat, "",
		"log format: json, console or auto (or set "+envLogFormat+"); auto is console on a terminal and json otherwise")
}

// resolveLogSettings reads --log-level and --log-format from args, falling back to
// the environment and then to info and auto. The flag wins over the variable, as
// --prover-addr does over PROVER_ADDR.
//
// It never fails. A logging knob that stops the relayer from starting is worse
// than the verbosity it was meant to set, so an unusable value is dropped, the
// default used, and the drop reported through ignored.
//
// The environment is the process's, not relayer/.env: that file is loaded inside
// each command, after this has run and the logger has been built.
func resolveLogSettings(args []string, getenv func(string) string, stderrIsTerminal bool) logSettings {
	probe := &cobra.Command{SilenceErrors: true, SilenceUsage: true}
	// Every other flag belongs to a subcommand this pass has not met.
	probe.FParseErrWhitelist = cobra.FParseErrWhitelist{UnknownFlags: true}
	registerLogFlags(probe)
	// A malformed flag fails the real parse in Execute, with cobra's own message.
	// Whatever was parsed before it is still read.
	_ = probe.ParseFlags(args)

	pick := func(flagName, envName string) (value, source string) {
		if v, _ := probe.Flags().GetString(flagName); v != "" {
			return v, "--" + flagName
		}
		if v := getenv(envName); v != "" {
			return v, envName
		}
		return "", ""
	}

	s := logSettings{level: zapcore.InfoLevel}

	if raw, source := pick(flagLogLevel, envLogLevel); raw != "" {
		if level, ok := parseLogLevel(raw); ok {
			s.level = level
		} else {
			s.ignored = append(s.ignored, fmt.Sprintf(
				"ignoring invalid %s=%q (want debug, info, warn or error); logging at info", source, raw))
		}
	}

	format := logFormatAuto
	if raw, source := pick(flagLogFormat, envLogFormat); raw != "" {
		switch strings.ToLower(raw) {
		case logFormatAuto, logFormatJSON, logFormatConsole:
			format = strings.ToLower(raw)
		default:
			s.ignored = append(s.ignored, fmt.Sprintf(
				"ignoring invalid %s=%q (want json, console or auto); using auto", source, raw))
		}
	}
	if format == logFormatAuto {
		format = logFormatJSON
		if stderrIsTerminal {
			format = logFormatConsole
		}
	}
	s.format = format
	return s
}

func parseLogLevel(raw string) (zapcore.Level, bool) {
	switch strings.ToLower(raw) {
	case "debug":
		return zapcore.DebugLevel, true
	case "info":
		return zapcore.InfoLevel, true
	case "warn":
		return zapcore.WarnLevel, true
	case "error":
		return zapcore.ErrorLevel, true
	}
	return zapcore.InfoLevel, false
}

// stderrIsTerminal reports whether stderr is a character device, which is what a
// terminal is and what a pipe, a file and a container's log driver are not.
func stderrIsTerminal() bool {
	info, err := os.Stderr.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// newLogger builds the process's one zap logger. JSON is what a log viewer reads:
// the same keys zap.NewProduction always wrote, so lines already in a viewer keep
// their shape. Console is for a person at a terminal.
//
// There is deliberately no sampler. zap.NewProduction installs one, which passes
// the first hundred identical messages in a second and every hundredth after, so a
// repeated line would silently thin out. The standard logger never did that, and
// every line the engine writes now goes through this logger.
func newLogger(s logSettings, out zapcore.WriteSyncer) *zap.Logger {
	var encoder zapcore.Encoder
	if s.format == logFormatConsole {
		encoder = zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	} else {
		encoder = zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	}
	return zap.New(zapcore.NewCore(encoder, out, s.level),
		zap.AddCaller(), zap.AddStacktrace(zap.DPanicLevel))
}
