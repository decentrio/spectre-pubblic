package main

import (
	"path/filepath"

	"go.uber.org/zap"
)

// logConfigTarget names the exact file and module a command is about to act on,
// as its first line of output.
//
// It exists because --config defaults to "config.json": a run intended for
// another file, invoked without the flag, silently reads that default instead.
// Nothing downstream said which file had been loaded — the config path was only
// ever printed on the success path, when an id was written back — so a command
// that decided it had nothing to do reported an address the operator could not
// find anywhere in the config they were looking at, with no way to tell that the
// two were different files.
func logConfigTarget(logger *zap.Logger, command, configPath string, cfg *appConfig) {
	resolved := configPath
	if abs, err := filepath.Abs(configPath); err == nil {
		resolved = abs
	}
	if source := cfg.CosmosToEthConfig.ICS26ClientID; source != "" {
		logger.Sugar().Infof("%s: using config %s (source %q)", command, resolved, source)
		return
	}
	logger.Sugar().Infof("%s: using config %s", command, resolved)
}
