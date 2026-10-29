package main

import (
	"math"
	"os"
	"path/filepath"
	"relayer/services"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A1 settles that one source→destination pair runs one process. relayStateDir
// turns that into a path, and these are the properties an operator depends on.
func TestRelayStateDir(t *testing.T) {
	t.Run("the working directory does not change the answer", func(t *testing.T) {
		// The property the old cursor file did NOT have: its root was the working
		// directory, so the same config started from two places wrote to two
		// different files and each run saw half the state.
		config := filepath.Join(t.TempDir(), "config.json")
		from := relayStateDir(config, "cosmos-to-eth", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"})

		here, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(here) })
		if err := os.Chdir(t.TempDir()); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		if got := relayStateDir(config, "cosmos-to-eth", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"}); got != from {
			t.Fatalf("the same config resolved to two directories:\n  %s\n  %s", from, got)
		}
	})

	t.Run("it sits beside the config", func(t *testing.T) {
		dir := t.TempDir()
		got := relayStateDir(filepath.Join(dir, "config.json"), "cosmos-to-eth", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"})
		if filepath.Dir(filepath.Dir(got)) != dir {
			t.Fatalf("state dir %s is not beside the config in %s", got, dir)
		}
	})

	t.Run("different identities never collide", func(t *testing.T) {
		config := filepath.Join(t.TempDir(), "config.json")
		other := filepath.Join(t.TempDir(), "config.json")
		seen := map[string]string{}
		for label, dir := range map[string]string{
			"baseline":          relayStateDir(config, "cosmos-to-eth", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"}),
			"other client":      relayStateDir(config, "cosmos-to-eth", relayClientIDs{router: "osmosis-1", wasm: "08-wasm-0"}),
			"other direction":   relayStateDir(config, "cosmos-to-l2", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"}),
			"other config file": relayStateDir(other, "cosmos-to-eth", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"}),
		} {
			if first, dup := seen[dir]; dup {
				t.Errorf("%s and %s share a state directory: %s", first, label, dir)
			}
			seen[dir] = label
		}
	})

	t.Run("a client id is never used as a path component", func(t *testing.T) {
		// Client ids are chain-supplied strings; treating one as a path is how a
		// state directory ends up somewhere nobody expected.
		dir := relayStateDir(filepath.Join(t.TempDir(), "config.json"), "cosmos-to-eth", relayClientIDs{router: "../../etc/evil", wasm: "../../etc/evil"})
		if strings.Contains(dir, "evil") || strings.Contains(dir, "..") {
			t.Fatalf("client id leaked into the path: %s", dir)
		}
	})

	// Reported by @DongLieu on #439. The state directory is the lock's identity, so
	// two names for one config must not produce two of them -- two directories mean
	// two lock files, and two locks that never contend mean two relayers owning the
	// same path and signer, which is the whole thing this layout prevents.
	//
	// The old code derived the HASH from filepath.Abs and the ROOT from the raw
	// filepath.Dir(configPath), so it diverged two different ways. The relative case
	// below is not exotic: it is `./relayer start --config config.json` run from the
	// config's own directory, and on macOS it already disagreed with the absolute
	// form because /var is a symlink to /private/var.
	t.Run("is the same for every name of one config", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		if err := os.WriteFile(configPath, []byte("{}"), configFilePerm); err != nil {
			t.Fatalf("write config: %v", err)
		}
		linkPath := filepath.Join(t.TempDir(), "link.json")
		if err := os.Symlink(configPath, linkPath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		want := relayStateDir(configPath, "cosmos-to-eth", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"})

		if got := relayStateDir(linkPath, "cosmos-to-eth", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"}); got != want {
			t.Fatalf("state dir via symlink = %q, want %q; the two names take different locks "+
				"and both relayers start", got, want)
		}

		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(wd) })
		if err := os.Chdir(dir); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		if got := relayStateDir("config.json", "cosmos-to-eth", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"}); got != want {
			t.Fatalf("state dir via a relative --config = %q, want %q; running the ordinary way "+
				"takes a different lock from the absolute path", got, want)
		}
	})

	// A different config, direction or client id must still get its own directory --
	// canonicalising must not collapse identities that are genuinely distinct.
	t.Run("still separates distinct identities", func(t *testing.T) {
		dir := t.TempDir()
		first := filepath.Join(dir, "a.json")
		second := filepath.Join(dir, "b.json")
		for _, p := range []string{first, second} {
			if err := os.WriteFile(p, []byte("{}"), configFilePerm); err != nil {
				t.Fatalf("write config: %v", err)
			}
		}

		base := relayStateDir(first, "cosmos-to-eth", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"})
		for name, got := range map[string]string{
			"another config":    relayStateDir(second, "cosmos-to-eth", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"}),
			"another direction": relayStateDir(first, "eth-to-cosmos", relayClientIDs{router: "cosmoshub-1", wasm: "08-wasm-0"}),
			"another client":    relayStateDir(first, "cosmos-to-eth", relayClientIDs{router: "osmosis-1", wasm: "08-wasm-0"}),
		} {
			if got == base {
				t.Fatalf("%s shares a state directory with the original (%q); their locks would "+
					"contend and one legitimate process would refuse to start", name, got)
			}
		}
	})

	// The first consequence: changing an override must move the state directory,
	// because the process is now relaying a different pair and must not inherit the
	// previous pair's cursors and pending work.
	t.Run("follows the override", func(t *testing.T) {
		cfg := cosmosToEthConfig{ICS26ClientID: "from-json"}
		configPath := filepath.Join(t.TempDir(), "config.json")

		dirFor := func(override string) string {
			t.Setenv("ICS26_CLIENT_ID", override)
			return relayStateDir(configPath, "cosmos-to-eth", effectiveRelayIDs(cfg))
		}

		first := dirFor("client-a")
		second := dirFor("client-b")
		if first == second {
			t.Fatalf("both overrides resolved to %s: a restart under a new ICS26_CLIENT_ID would "+
				"replay the previous pair's pending packets under the wrong client", first)
		}

		// And the raw config value must not be what names it, or the two paths above
		// would have collided on the config's own id.
		raw := relayStateDir(configPath, "cosmos-to-eth", relayClientIDs{router: cfg.ICS26ClientID, wasm: cfg.CosmosWasmClientID})
		if first == raw || second == raw {
			t.Fatal("an overridden run reused the directory named for the raw config id")
		}
	})

	// The second consequence, and the one that breaks the guard rather than the
	// state: two configs whose RAW ids differ but whose EFFECTIVE ids are the same
	// serve one relay path, so they must contend for one lock. Naming the directory
	// from the raw id gave them two, and both started.
	t.Run("two configs on one effective pair share one state dir", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.json")
		t.Setenv("ICS26_CLIENT_ID", "the-real-client")

		one := effectiveRelayIDs(cosmosToEthConfig{ICS26ClientID: "stale-a"})
		two := effectiveRelayIDs(cosmosToEthConfig{ICS26ClientID: "stale-b"})

		dirOne := relayStateDir(configPath, "cosmos-to-eth", one)
		dirTwo := relayStateDir(configPath, "cosmos-to-eth", two)
		if dirOne != dirTwo {
			t.Fatalf("%s != %s: both processes relay %q, so they must contend for one lock; "+
				"two directories means both start and advance the same cursors",
				dirOne, dirTwo, "the-real-client")
		}
	})

	// The state directory must move when EITHER half of the pair moves.
	//
	// Reported by @neitdung: it was named from the router id alone while the builder
	// received {router, wasm}. Holding the config and the router fixed and changing
	// only COSMOS_WASM_CLIENT_ID therefore relayed a different path out of the same
	// directory -- the same lock, the same cursors, the same pending trackers. On
	// restart the new wasm client inherits a cursor that skips its own older events,
	// and pending packets from the previous pair replay under the new destination.
	//
	// Each half is changed on its own, because a test that changes both at once
	// passes against a digest that reads only one of them.
	t.Run("tracks both halves of the effective pair", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.json")
		cfg := cosmosToEthConfig{ICS26ClientID: "cosmoshub-1", CosmosWasmClientID: "08-wasm-0"}

		dirFor := func(t *testing.T, router, wasm string) string {
			t.Helper()
			t.Setenv("ICS26_CLIENT_ID", router)
			t.Setenv("COSMOS_WASM_CLIENT_ID", wasm)
			return relayStateDir(configPath, "cosmos-to-eth", effectiveRelayIDs(cfg))
		}

		t.Run("the wasm half alone", func(t *testing.T) {
			a := dirFor(t, "cosmoshub-1", "08-wasm-0")
			b := dirFor(t, "cosmoshub-1", "08-wasm-7")
			if a == b {
				t.Fatalf("08-wasm-0 and 08-wasm-7 share state directory %q: one cursor, one lock, two relay paths", a)
			}
		})

		t.Run("the router half alone", func(t *testing.T) {
			a := dirFor(t, "cosmoshub-1", "08-wasm-0")
			b := dirFor(t, "osmosis-1", "08-wasm-0")
			if a == b {
				t.Fatalf("cosmoshub-1 and osmosis-1 share state directory %q", a)
			}
		})

		t.Run("the same pair, twice", func(t *testing.T) {
			a := dirFor(t, "cosmoshub-1", "08-wasm-0")
			b := dirFor(t, "cosmoshub-1", "08-wasm-0")
			if a != b {
				t.Fatalf("%s != %s: one identity must always reach one directory, or two processes never contend", a, b)
			}
		})
	})

	// Fields are framed by length, not separated by a byte. A separator is only
	// unambiguous while no field can contain it, and these come from a JSON config.
	// Two distinct identities colliding into one directory is the duplicate-owner
	// case the lock exists to prevent, and it would show up as two processes quietly
	// sharing one cursor rather than as an error.
	t.Run("framing cannot be forged by a separator in a client ID", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.json")

		// Under "\x00" separators both of these flatten to <path>\x00<kind>\x00a\x00b\x00c,
		// so they are the pair that actually collides. A shift with an empty field on
		// one side does not: the separator for that field still lands in the string.
		left := relayStateDir(configPath, "cosmos-to-eth", relayClientIDs{router: "a", wasm: "b\x00c"})
		right := relayStateDir(configPath, "cosmos-to-eth", relayClientIDs{router: "a\x00b", wasm: "c"})
		if left == right {
			t.Fatalf("{a, b\\x00c} and {a\\x00b, c} share state directory %q; the framing is forgeable", left)
		}
	})
}

func TestCheckLegacyStateLayout(t *testing.T) {
	// The other half of @DongLieu's finding: this build reads neither the old tracker
	// directory nor the old cursor file, so an upgrade would start with an empty
	// tracker and say nothing. Packets outside the startup lookback would stop being
	// tracked for timeout refunds. There is no migration -- nothing is deployed
	// outside devnets -- but the change has to be visible rather than silent.
	t.Run("refuses each old location", func(t *testing.T) {
		newConfig := func(t *testing.T) string {
			t.Helper()
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			if err := os.WriteFile(configPath, []byte("{}"), configFilePerm); err != nil {
				t.Fatalf("write config: %v", err)
			}
			return configPath
		}
		// The cwd-relative legacy cursor directory is looked for relative to the
		// process, so every case runs from a scratch directory it owns.
		chdirScratch := func(t *testing.T) string {
			t.Helper()
			wd, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			scratch := t.TempDir()
			t.Cleanup(func() { _ = os.Chdir(wd) })
			if err := os.Chdir(scratch); err != nil {
				t.Fatalf("chdir: %v", err)
			}
			return scratch
		}

		t.Run("a clean layout starts", func(t *testing.T) {
			chdirScratch(t)
			if err := checkLegacyStateLayout(newConfig(t)); err != nil {
				t.Fatalf("refused a clean layout: %v", err)
			}
		})

		t.Run("legacy tracker directory beside the config", func(t *testing.T) {
			chdirScratch(t)
			configPath := newConfig(t)
			if err := os.Mkdir(filepath.Join(filepath.Dir(configPath), legacyTrackerDirName), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			err := checkLegacyStateLayout(configPath)
			if err == nil {
				t.Fatal("started on top of the old tracker directory; the pending tracker would be " +
					"silently empty and expired packets would never be refunded")
			}
			if !strings.Contains(err.Error(), legacyTrackerDirName) {
				t.Fatalf("error does not name what was found: %v", err)
			}
		})

		t.Run("legacy cursor directory in the working directory", func(t *testing.T) {
			chdirScratch(t)
			if err := os.Mkdir(legacyCursorDirName, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			err := checkLegacyStateLayout(newConfig(t))
			if err == nil {
				t.Fatal("started on top of the old cursor directory; recovery would silently restart " +
					"from the lookback window")
			}
			if !strings.Contains(err.Error(), legacyCursorDirName) {
				t.Fatalf("error does not name what was found: %v", err)
			}
		})

		t.Run("the retired environment variable", func(t *testing.T) {
			chdirScratch(t)
			t.Setenv(legacyCursorFileEnvar, "/somewhere/cursors.json")
			err := checkLegacyStateLayout(newConfig(t))
			if err == nil {
				t.Fatalf("%s was accepted and ignored; an operator who set it would believe their "+
					"cursors were being read", legacyCursorFileEnvar)
			}
			if !strings.Contains(err.Error(), legacyCursorFileEnvar) {
				t.Fatalf("error does not name the variable: %v", err)
			}
		})
	})

	// The helper being right is not the same as the helper being called. Removing
	// the call from Start leaves every test above green while the guard does
	// nothing -- exactly the failure @DongLieu found on #435, where the log-stamp
	// helper was correct and unreachable. So this drives the real command.
	//
	// Start is expected to FAIL either way here; what is asserted is WHICH failure.
	// The legacy check runs before the prover load, so a config sitting next to old
	// state must be refused by name rather than reaching the prover.
	// The upgrade case the canonicalisation fix broke: the OLD build joined its
	// tracker directory onto filepath.Dir(configPath) with no symlink resolution
	// (47bbc901), so state sits beside the config path as SPELLED. Looking only
	// beside the canonical path misses it, and the relayer starts with an empty
	// pending tracker while telling the operator nothing -- the exact silent loss
	// this guard exists to prevent. Found in review.
	t.Run("finds state beside the symlink", func(t *testing.T) {
		// The real config, and its own directory deliberately CLEAN.
		realDir := t.TempDir()
		configPath := filepath.Join(realDir, "config.json")
		if err := os.WriteFile(configPath, []byte("{}"), configFilePerm); err != nil {
			t.Fatalf("write config: %v", err)
		}

		// A symlink to it, with the legacy state beside the LINK, which is where a
		// previous release started through that link would have put it.
		linkDir := t.TempDir()
		linkPath := filepath.Join(linkDir, "config.json")
		if err := os.Symlink(configPath, linkPath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := os.Mkdir(filepath.Join(linkDir, legacyTrackerDirName), 0o700); err != nil {
			t.Fatalf("mkdir legacy: %v", err)
		}

		// Sanity: the two parents really do differ, or this test proves nothing.
		if filepath.Dir(canonicalConfigPath(linkPath)) == filepath.Dir(linkPath) {
			t.Skip("temp dirs resolve to the same parent; nothing to discriminate")
		}

		err := checkLegacyStateLayout(linkPath)
		if err == nil {
			t.Fatal("started on top of legacy state beside the config symlink: the pending " +
				"tracker and cursor set would be silently empty, and packets older than the " +
				"startup lookback would stop being tracked for timeout refunds")
		}
		if !strings.Contains(err.Error(), legacyTrackerDirName) {
			t.Fatalf("error does not name what was found: %v", err)
		}
	})

	// The other half of checking two parents: when they are the SAME directory --
	// the ordinary case, no symlink anywhere -- it must be reported once. An
	// operator counting problems should not be told to clear two paths that are one.
	t.Run("reports one directory once", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		if err := os.WriteFile(configPath, []byte("{}"), configFilePerm); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if err := os.Mkdir(filepath.Join(dir, legacyTrackerDirName), 0o700); err != nil {
			t.Fatalf("mkdir legacy: %v", err)
		}

		err := checkLegacyStateLayout(configPath)
		if err == nil {
			t.Fatal("did not refuse legacy state beside the config")
		}
		if n := strings.Count(err.Error(), legacyTrackerDirName); n != 1 {
			t.Fatalf("%s named %d times in %q; the raw and canonical parents are the same "+
				"directory here and must be deduplicated", legacyTrackerDirName, n, err)
		}
	})
}

// FETCH_TIMEOUT bounds the Cosmos RPC calls. It used to be read as
// `if d, err := strconv.Atoi(envVal); err == nil && d > 0`, which treated BOTH a
// parse failure and a non-positive value as "not set" -- so an operator who wrote
// FETCH_TIMEOUT=30s, the way a Go duration is normally written and the natural
// guess for a field that IS a time.Duration, silently got the default.
//
// An RPC running without the deadline the operator asked for is the failure that
// wedged a whole relay direction before, so the wrong value must be refused
// rather than quietly replaced.
func TestEnvSecondsDuration(t *testing.T) {
	const name = "TEST_RELAYER_TIMEOUT_KNOB"

	t.Run("unset reports not-set without an error", func(t *testing.T) {
		t.Setenv(name, "")
		got, set, err := envSecondsDuration(name)
		if err != nil {
			t.Fatalf("unset must not be an error, got %v", err)
		}
		if set {
			t.Fatalf("reported set with value %s", got)
		}
	})

	t.Run("parses whole seconds", func(t *testing.T) {
		t.Setenv(name, "30")
		got, set, err := envSecondsDuration(name)
		if err != nil || !set || got != 30*time.Second {
			t.Fatalf("envSecondsDuration = %s, %v, %v; want 30s, true, nil", got, set, err)
		}
	})

	t.Run("refuses a value it cannot honour", func(t *testing.T) {
		for label, raw := range map[string]string{
			"go duration syntax": "30s",
			"milliseconds":       "500ms",
			"digits then unit":   "30sec",
			"zero":               "0",
			"negative":           "-1",
			"words":              "none",
			"decimal":            "1.5",
		} {
			t.Run(label, func(t *testing.T) {
				t.Setenv(name, raw)
				got, set, err := envSecondsDuration(name)
				if err == nil {
					t.Fatalf("envSecondsDuration(%q) = %s, set=%v with no error", raw, got, set)
				}
				// Must not look like an unset variable either: the caller applies
				// the default on `set == false`, so reporting set=false here would
				// reintroduce the silent fallback this replaced.
				if set {
					t.Fatalf("envSecondsDuration(%q) reported set alongside an error", raw)
				}
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name the variable the operator set", err)
				}
			})
		}
	})

	t.Run("surrounding whitespace is tolerated", func(t *testing.T) {
		// Trimmed on purpose: a trailing newline from `export FETCH_TIMEOUT=$(...)`
		// is an artefact of the shell, not something the operator typed.
		t.Setenv(name, "  30\n")
		got, set, err := envSecondsDuration(name)
		if err != nil || !set || got != 30*time.Second {
			t.Fatalf("envSecondsDuration = %s, %v, %v; want 30s, true, nil", got, set, err)
		}
	})

	// The env path is what an operator actually types, so the guard has to hold
	// through it and not only in the helper underneath.
	t.Run("refuses an overflowing value", func(t *testing.T) {
		t.Setenv("FETCH_TIMEOUT", "9223372036854775807")
		got, set, err := envSecondsDuration("FETCH_TIMEOUT")
		if err == nil {
			t.Fatalf("an overflowing FETCH_TIMEOUT was accepted: got=%v set=%v", got, set)
		}
		if set {
			t.Error("a rejected value was still reported as set")
		}

		t.Setenv("FETCH_TIMEOUT", "9223372036")
		if _, set, err := envSecondsDuration("FETCH_TIMEOUT"); err != nil || !set {
			t.Fatalf("the largest usable value was rejected: set=%v err=%v", set, err)
		}
	})
}

// buildCosmosConfig must propagate the refusal rather than swallow it -- the
// whole point of the change is that a bad override stops the process instead of
// being replaced by a default nobody chose.
func TestBuildCosmosConfig_RejectsAMalformedFetchTimeout(t *testing.T) {
	t.Setenv("FETCH_TIMEOUT", "30s")
	if _, err := buildCosmosConfig(cosmosToEthConfig{}, services.BatchConfig{}); err == nil {
		t.Fatal("a malformed FETCH_TIMEOUT was accepted")
	}

	t.Setenv("FETCH_TIMEOUT", "30")
	cfg, err := buildCosmosConfig(cosmosToEthConfig{}, services.BatchConfig{})
	if err != nil {
		t.Fatalf("a valid FETCH_TIMEOUT was rejected: %v", err)
	}
	if cfg.FetchTimeout != 30*time.Second {
		t.Fatalf("FetchTimeout = %s, want 30s", cfg.FetchTimeout)
	}
}

// router_deploy_block is optional and must reach the ETH source's flush intact:
// 0 keeps the bounded walk from the head, anything else starts lookups there.
func TestBuildCosmosConfig_CarriesRouterDeployBlock(t *testing.T) {
	t.Setenv("FETCH_TIMEOUT", "")
	cfg, err := buildCosmosConfig(cosmosToEthConfig{RouterDeployBlock: 11_000_000}, services.BatchConfig{})
	if err != nil {
		t.Fatalf("buildCosmosConfig: %v", err)
	}
	if cfg.RouterDeployBlock != 11_000_000 {
		t.Fatalf("RouterDeployBlock = %d, want 11000000", cfg.RouterDeployBlock)
	}
	cfg, err = buildCosmosConfig(cosmosToEthConfig{}, services.BatchConfig{})
	if err != nil || cfg.RouterDeployBlock != 0 {
		t.Fatalf("unset router_deploy_block = (%d, %v), want 0", cfg.RouterDeployBlock, err)
	}
}

func TestBuildCosmosToEthSource(t *testing.T) {
	// Config that can be judged without a network must be judged BEFORE any client
	// is dialled or started, because the early return has no way to release one:
	// the cleanup closure is not built until the end of the builder.
	//
	// Making buildCosmosConfig fallible introduced exactly such a return between
	// cosmosClient.Start() and that closure, leaking a live WebSocket subscription
	// on an operator typo. Review caught it.
	//
	// The assertion is on WHICH error comes back, which is what pins the order.
	// Every endpoint below is unreachable, so a builder that dialled first would
	// report a connection failure; reporting the FETCH_TIMEOUT failure instead
	// proves nothing was opened.
	t.Run("validate config before opening clients", func(t *testing.T) {
		t.Setenv("FETCH_TIMEOUT", "30s") // seconds are expected; "30s" is malformed

		const unreachable = "http://127.0.0.1:1"
		c2e := cosmosToEthConfig{
			EthRpcUrl:          unreachable,
			TmRpcUrl:           unreachable,
			ICS26ClientID:      "cosmos-0",
			CosmosWasmClientID: "08-wasm-0",
			SpectreClient:      "0x0000000000000000000000000000000000000001",
		}

		t.Run("cosmos_to_eth", func(t *testing.T) {
			_, _, cleanup, err := buildCosmosToEthSource(
				zap.NewNop(), c2e, ethToCosmosConfig{}, services.BatchConfig{}, nil, nil,
				buildCosmosToEthSourceOptions{
					startSubscriptions: true,
					// The builder no longer reads these from the config: the caller
					// resolves them once so the state directory and the relay identity
					// cannot disagree. A fixture without them now fails on the id check
					// before it reaches the one this test is about.
					ids: relayClientIDs{router: c2e.ICS26ClientID, wasm: c2e.CosmosWasmClientID},
				},
			)
			requireFetchTimeoutRefusal(t, err, cleanup)
		})

		t.Run("cosmos_to_l2", func(t *testing.T) {
			_, _, cleanup, err := buildCosmosToL2Dest(
				zap.NewNop(), c2e, services.BatchConfig{}, nil, nil, t.TempDir(),
			)
			requireFetchTimeoutRefusal(t, err, cleanup)
		})
	})

	// cosmos_wasm_client_id is the id of the Ethereum light client ON Cosmos, and
	// only the eth->cosmos direction reads it. Requiring it unconditionally made
	// submit-misbehaviour unusable for a cosmos_to_l2 source: selectSource offers L2
	// sources, cosmos_to_l2 deliberately carries no such id, and the builder refused
	// the evidence before looking at it.
	//
	// So the requirement belongs to the caller, and both directions of that are
	// worth pinning -- a fix that simply dropped the check would satisfy the first
	// case and silently remove the guard from `start`.
	t.Run("requires the wasm client ID only when it is used", func(t *testing.T) {
		// An L2-shaped source: no cosmos_wasm_client_id, unreachable endpoints. The
		// assertion is on WHICH error comes back, so an endpoint failure proves the
		// wasm-id check was passed rather than skipped along with everything else.
		l2Shaped := cosmosToEthConfig{
			ICS26ClientID: "cosmos-l2-0",
			SpectreClient: "0x0000000000000000000000000000000000000001",
			EthRpcUrl:     "http://127.0.0.1:1",
			TmRpcUrl:      "http://127.0.0.1:1",
		}

		t.Run("a one-shot caller may omit it", func(t *testing.T) {
			_, _, cleanup, err := buildCosmosToEthSource(
				zap.NewNop(), l2Shaped, ethToCosmosConfig{}, services.BatchConfig{}, nil, nil,
				buildCosmosToEthSourceOptions{},
			)
			if cleanup != nil {
				cleanup()
			}
			if err != nil && strings.Contains(err.Error(), "cosmos_wasm_client_id") {
				t.Fatalf("an L2 source was rejected for a field it does not have: %v", err)
			}
		})

		t.Run("a caller that relays eth->cosmos still needs it", func(t *testing.T) {
			_, _, cleanup, err := buildCosmosToEthSource(
				zap.NewNop(), l2Shaped, ethToCosmosConfig{}, services.BatchConfig{}, nil, nil,
				buildCosmosToEthSourceOptions{relaysEVMToCosmos: true},
			)
			if cleanup != nil {
				cleanup()
			}
			if err == nil || !strings.Contains(err.Error(), "cosmos_wasm_client_id") {
				t.Fatalf("error = %v; a relayer driving eth->cosmos must be stopped at startup, "+
					"not at the first packet", err)
			}
		})
	})

	// The structural half. Both regressions on this PR were the same shape: a value
	// the environment can override, resolved inside the builder rather than in the
	// caller beside the others. The fix moved every one of them to the caller, and
	// this is what keeps them there -- a new override added inside the builder makes
	// this red instead of silently escaping the rule.
	t.Run("builder reads no environment itself", func(t *testing.T) {
		src, err := os.ReadFile("build_source.go")
		if err != nil {
			t.Fatalf("read build_source.go: %v", err)
		}
		body := string(src)
		start := strings.Index(body, "func buildCosmosToEthSource(")
		if start < 0 {
			t.Fatal("buildCosmosToEthSource not found; this guard is pinned to its name")
		}
		// To the next top-level declaration, which is where the function ends.
		rest := body[start+len("func buildCosmosToEthSource("):]
		end := strings.Index(rest, "\nfunc ")
		if end < 0 {
			end = len(rest)
		}
		if strings.Contains(rest[:end], "envOrDefault(") {
			t.Fatal("buildCosmosToEthSource reads the environment directly; resolve it in the caller " +
				"beside effectiveRelayIDs/effectiveRoleManager, where every override is decided")
		}
	})
}

func requireFetchTimeoutRefusal(t *testing.T, err error, cleanup func()) {
	t.Helper()
	if cleanup != nil {
		t.Error("a builder that failed on config returned a cleanup func, so it opened something")
		cleanup()
	}
	if err == nil {
		t.Fatal("a malformed FETCH_TIMEOUT was accepted")
	}
	if !strings.Contains(err.Error(), "FETCH_TIMEOUT") {
		t.Fatalf("error = %v; want the FETCH_TIMEOUT refusal. Any connection error here means a "+
			"client was opened before the config was judged, and the early return cannot close it", err)
	}
}

// strconv accepting a number is not the same as time.Duration holding it.
// time.Duration is an int64 of NANOseconds, so 9223372037 seconds -- a perfectly
// ordinary-looking integer -- wraps to a NEGATIVE duration.
//
// A negative timeout is worse than a wrong one because it means two different
// things depending on who reads it: context.WithTimeout cancels immediately,
// while services.fetchCtx substitutes its 15s default. One accepted setting,
// two behaviours, neither of them what the operator asked for.
func TestSecondsToDuration_RefusesWhatDoesNotFit(t *testing.T) {
	const maxSeconds = uint64(math.MaxInt64 / int64(time.Second)) // 9223372036

	t.Run("the largest representable value is accepted", func(t *testing.T) {
		got, err := secondsToDuration("fetch_timeout", maxSeconds)
		if err != nil {
			t.Fatalf("the boundary value was rejected: %v", err)
		}
		if got <= 0 {
			t.Fatalf("duration = %v, want positive", got)
		}
	})

	t.Run("one past the boundary is refused", func(t *testing.T) {
		if _, err := secondsToDuration("fetch_timeout", maxSeconds+1); err == nil {
			t.Fatal("a value one second past the maximum was accepted")
		}
	})

	t.Run("MaxUint64 is refused rather than wrapping", func(t *testing.T) {
		got, err := secondsToDuration("fetch_timeout", math.MaxUint64)
		if err == nil {
			t.Fatalf("MaxUint64 was accepted as %v", got)
		}
		if !strings.Contains(err.Error(), "fetch_timeout") {
			t.Errorf("error %q does not name the setting the operator wrote", err)
		}
	})

	// The property, not just the three points above: nothing this function
	// returns without an error may be non-positive.
	t.Run("no accepted value is ever non-positive", func(t *testing.T) {
		for _, seconds := range []uint64{1, 30, 3600, maxSeconds - 1, maxSeconds, maxSeconds + 1, 1 << 62, math.MaxInt64, math.MaxUint64} {
			got, err := secondsToDuration("fetch_timeout", seconds)
			if err != nil {
				continue
			}
			if got <= 0 {
				t.Errorf("%d seconds was accepted as %v", seconds, got)
			}
		}
	})
}

// Found in review: Start named the state directory from the RAW config id while
// buildCosmosToEthSource relayed the EFFECTIVE one. The directory is the lock,
// the recovery cursors and the pending tracker, so the two must be the same
// function of the same inputs or the state describes a path nobody is serving.
func TestEffectiveRelayIDs(t *testing.T) {
	cfg := cosmosToEthConfig{ICS26ClientID: "from-json", CosmosWasmClientID: "wasm-from-json"}

	t.Run("the environment wins, for both ids", func(t *testing.T) {
		t.Setenv("ICS26_CLIENT_ID", "from-env")
		t.Setenv("COSMOS_WASM_CLIENT_ID", "wasm-from-env")
		ids := effectiveRelayIDs(cfg)
		router, wasm := ids.router, ids.wasm
		if router != "from-env" || wasm != "wasm-from-env" {
			t.Fatalf("got (%q, %q), want the environment values", router, wasm)
		}
	})

	t.Run("an unset override leaves the config value", func(t *testing.T) {
		t.Setenv("ICS26_CLIENT_ID", "")
		t.Setenv("COSMOS_WASM_CLIENT_ID", "")
		ids := effectiveRelayIDs(cfg)
		router, wasm := ids.router, ids.wasm
		if router != "from-json" || wasm != "wasm-from-json" {
			t.Fatalf("got (%q, %q); an empty override must not erase the configured id", router, wasm)
		}
	})
}

// Every value the environment may override for a source is decided in one
// place, beside effectiveRelayIDs -- which the role-manager address was not,
// after the identity fix moved the client ids to the caller and left this one
// reading the environment in the builder.
//
// It is not cosmetic: RouterManagesProofSubmission compares this address with
// the router to choose ICS26Router versus direct SpectreClient submission, so a
// stale value routes incident evidence down the wrong contract path.
func TestEffectiveRoleManager(t *testing.T) {
	cfg := cosmosToEthConfig{ICS26Address: "0xfromconfig"}

	t.Run("the environment wins", func(t *testing.T) {
		t.Setenv("ROLE_MANAGER", "0xfromenv")
		if got := effectiveRoleManager(cfg); got != "0xfromenv" {
			t.Fatalf("role manager = %q, want the override", got)
		}
	})

	t.Run("an unset variable falls back to the config", func(t *testing.T) {
		t.Setenv("ROLE_MANAGER", "")
		if got := effectiveRoleManager(cfg); got != "0xfromconfig" {
			t.Fatalf("role manager = %q, want the config's", got)
		}
	})
}
