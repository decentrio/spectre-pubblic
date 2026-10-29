package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestLogConfigTarget checks the line that tells an operator which file a
// command actually read. --config defaults to "config.json", so a run meant for
// another file and invoked without the flag reads that default instead; the
// resolved absolute path is what makes the two distinguishable in the log.
func TestLogConfigTarget(t *testing.T) {
	t.Parallel()

	capture := func(configPath string, cfg *appConfig) string {
		core, logs := observer.New(zapcore.InfoLevel)
		logConfigTarget(zap.New(core), "create-clients-eth", configPath, cfg)
		if logs.Len() != 1 {
			t.Fatalf("logged %d lines, want 1", logs.Len())
		}
		return logs.All()[0].Message
	}

	t.Run("names the resolved path and the selected source", func(t *testing.T) {
		t.Parallel()
		cfg := &appConfig{}
		cfg.CosmosToEthConfig.ICS26ClientID = "arb-client-0"

		msg := capture("config-arb.json", cfg)

		want, err := filepath.Abs("config-arb.json")
		if err != nil {
			t.Fatalf("abs: %v", err)
		}
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry the absolute path %q", msg, want)
		}
		if !strings.Contains(msg, "arb-client-0") {
			t.Errorf("message %q does not name the selected source", msg)
		}
	})

	// A relative path alone would not have separated config.json from
	// config-arb.json in the report that prompted this; the absolute form does.
	t.Run("distinguishes two configs in the same directory", func(t *testing.T) {
		t.Parallel()
		cfg := &appConfig{}
		cfg.CosmosToEthConfig.ICS26ClientID = "arb-client-0"

		if a, b := capture("config.json", cfg), capture("config-arb.json", cfg); a == b {
			t.Fatalf("both configs logged the same line: %q", a)
		}
	})

	t.Run("omits the source when none is selected", func(t *testing.T) {
		t.Parallel()
		msg := capture("config.json", &appConfig{})
		if strings.Contains(msg, "source") {
			t.Errorf("message %q mentions a source when none was selected", msg)
		}
	})

	// The helper was tested; the wiring was not, and that is how three of the five
	// commands taking --config went without it -- start, update-client and
	// submit-misbehaviour, the three an operator is most likely to run against the
	// wrong file during an incident. A test on logConfigTarget itself cannot catch
	// that: it passes whether or not anybody calls it.
	//
	// The command list is derived from the source rather than written down, so the
	// sixth command to take --config is covered the day it is added instead of the
	// day someone remembers this test exists. Structural for the same reason
	// TestStart/opts into the wasm client ID requirement is: these calls sit inside cobra
	// RunE closures behind RPC dials a unit test has no way to satisfy.
	t.Run("every config command names the file it read", func(t *testing.T) {
		t.Parallel()

		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatalf("read cmd dir: %v", err)
		}

		// --config defaults to "config.json"; that default is the whole reason the
		// line exists, so a command declaring it is a command that needs one.
		const declaresConfigFlag = `flagConfigPath, "config.json"`
		total := 0
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			body := string(src)
			commands := strings.Count(body, declaresConfigFlag)
			if commands == 0 {
				continue
			}
			total += commands
			// One file can declare several commands (create_clients.go declares
			// two), so count rather than merely look for one call.
			if calls := strings.Count(body, "logConfigTarget("); calls < commands {
				t.Errorf("%s declares %d command(s) taking --config but calls logConfigTarget %d time(s); "+
					"a run against the wrong file would say nothing about which file it read",
					name, commands, calls)
			}
		}
		if total == 0 {
			t.Fatal("found no command taking --config; this test no longer describes the package")
		}
	})
}
