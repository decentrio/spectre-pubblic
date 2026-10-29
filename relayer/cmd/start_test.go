package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestStart(t *testing.T) {
	// Reported by @DongLieu on #435. stampRelayPathOnLogs and relayPathID were both
	// reachable only from their unit tests: the production call was written in
	// 74cc0147, survived the first merge, and was lost in 3b75da08 when the base
	// branch moved Start out of main.go into start.go and the resolution took the
	// base's file. Every relay log kept the default empty prefix, so the entire
	// point of the PR was absent while its helper tests stayed green.
	//
	// This drives the real cobra command instead of the helper, which is the only
	// shape that can catch it. Start is expected to FAIL here -- nothing listens on the
	// prover address -- and that is fine: the stamp happens after config validation
	// and before the prover dial, so the prefix must already be set by the time the
	// error comes back.
	t.Run("stamps the relay path before it can fail", func(t *testing.T) {
		origFlags, origPrefix, origOut := log.Flags(), log.Prefix(), log.Writer()
		t.Cleanup(func() {
			log.SetFlags(origFlags)
			log.SetPrefix(origPrefix)
			log.SetOutput(origOut)
		})
		log.SetOutput(&bytes.Buffer{})
		log.SetPrefix("")

		// Run from a scratch directory. checkLegacyStateLayout looks for the previous
		// layout beside the CONFIG and in the working directory, so a stray
		// .relayer-state left in the checkout by an older build makes this test refuse
		// to start -- a result that depends on debris in someone's tree rather than on
		// the code. #439's own tests do the same for the same reason.
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(wd) })
		if err := os.Chdir(t.TempDir()); err != nil {
			t.Fatalf("chdir: %v", err)
		}

		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		config := `{"modules":[{"name":"cosmos_to_eth","src_chain":"cosmoshub-1","config":{` +
			`"tm_rpc_url":"http://127.0.0.1:26657","eth_rpc_url":"http://127.0.0.1:8545",` +
			`"ics26_address":"0x80741a37e3644612f0465145c9709a90b6d77ee3","ics26_client_id":"cosmoshub-1"}}]}`
		if err := os.WriteFile(configPath, []byte(config), configFilePerm); err != nil {
			t.Fatalf("write config: %v", err)
		}
		// Valid-shaped keys, because startup rejects missing ones BEFORE it loads the
		// config, and this test is about what happens after that.
		const testKey = "a81f9eb900c02f35d28ec80d1da79dde52cc4cc37d6fbaef5768ab59f32cbfdd"
		t.Setenv("ETH_PRIVATE_KEY", testKey)
		t.Setenv("COSMOS_PRIVATE_KEY", testKey)
		// #467 added a startup gate that refuses a Cosmos-relaying config with no
		// chain id, and it runs between the stamp and the prover load. Without this
		// the command stops there and the window this test observes never opens --
		// the fixture went stale, not the ordering. The endpoint probes behind the
		// gate tolerate an address that does not answer, so nothing here has to run.
		t.Setenv("COSMOS_CHAIN_ID", "cosmoshub-1")
		// Point the prover at an address nothing listens on, so the command stops
		// right after the stamp rather than trying to reach a chain. The budget is
		// shortened so the test does not wait out the production 30 seconds.
		t.Setenv("PROVER_ADDR", "127.0.0.1:1")
		previousBudget := proverDialBudget
		proverDialBudget = 300 * time.Millisecond
		t.Cleanup(func() { proverDialBudget = previousBudget })

		core, logs := observer.New(zapcore.InfoLevel)
		command := Start(zap.New(core))
		// Dialling the prover makes Start log one line through zap between the stamp
		// and the prover failure -- otherwise the zap half has nothing observable to
		// assert on here.
		command.SetArgs([]string{"--config", configPath})
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		err = command.Execute()

		// Taken before the probe below, so that "start emitted a zap line" is still a
		// claim about start and not about the probe.
		entries := logs.All()

		// The zap half, which the first version of this test could not see: Start and
		// the builders emit their lifecycle lines through the injected logger, and a
		// stamp that only touches log.Default() leaves every one of them
		// unattributable in a merged log.
		if len(entries) == 0 {
			t.Fatalf("start emitted no zap line before failing (%v), so the scoped logger "+
				"is unverified here; the test needs a line between the stamp and the failure", err)
		}
		for _, e := range entries {
			got, ok := e.ContextMap()[relayPathLogField]
			if !ok {
				t.Fatalf("zap line %q carries no %q field: start used the unscoped logger "+
					"(command returned %v)", e.Message, relayPathLogField, err)
			}
			if !strings.Contains(fmt.Sprint(got), "cosmoshub-1") {
				t.Fatalf("zap line %q has %s = %v, want the relay path", e.Message, relayPathLogField, got)
			}
		}

		// The standard-library half. The engine's own lines go through log.Printf, so a
		// line logged after Start returned has to reach the same scoped logger and
		// carry the path there. If start never stamped, it goes to the buffer set above
		// and the observer sees nothing.
		const probe = "[probe] logged after start returned"
		log.Print(probe)
		all := logs.All()
		if len(all) != len(entries)+1 || all[len(all)-1].Message != probe {
			t.Fatalf("a standard-library line logged after start returned never reached the scoped "+
				"logger (command returned %v); start never called stampRelayPath, so every relay "+
				"line is unlabelled", err)
		}
		if got := all[len(all)-1].ContextMap()[relayPathLogField]; !strings.Contains(fmt.Sprint(got), "cosmoshub-1") {
			t.Fatalf("standard-library line has %s = %v, want the relay path", relayPathLogField, got)
		}
	})

	// The stamp sits on the boundary between "answerable from the file" and "needs a
	// read", so a finding from the read-based gate is already labelled.
	//
	// It moved there when #467's validateStartupConfig was merged in. That gate
	// reports live disagreements -- a chain id the endpoint does not confirm, a
	// contract address holding no code -- and those are exactly the lines that stop
	// saying which process they came from once several relayers share a journal. The
	// two gates above it report a config the operator can read back from the file,
	// where a label adds nothing.
	//
	// Without this, nothing holds the order: the test above passes with the stamp on
	// either side of that gate, because its config gets past it.
	t.Run("stamps the path before the checks that need a read", func(t *testing.T) {
		// Start points the standard logger at its scoped logger and never points it
		// back, so this puts it back, and keeps the buffer's lines out of the output.
		origFlags, origPrefix, origOut := log.Flags(), log.Prefix(), log.Writer()
		t.Cleanup(func() {
			log.SetFlags(origFlags)
			log.SetPrefix(origPrefix)
			log.SetOutput(origOut)
		})
		log.SetOutput(&bytes.Buffer{})

		// From a scratch directory: checkLegacyStateLayout looks for the previous
		// layout in the working directory too, so a stray .relayer-state left in the
		// checkout would stop this run at THAT gate and the ordering below would never
		// be exercised.
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(wd) })
		if err := os.Chdir(t.TempDir()); err != nil {
			t.Fatalf("chdir: %v", err)
		}

		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		config := `{"modules":[{"name":"cosmos_to_eth","src_chain":"cosmoshub-1","config":{` +
			`"tm_rpc_url":"http://127.0.0.1:26657","eth_rpc_url":"http://127.0.0.1:8545",` +
			`"ics26_address":"0x80741a37e3644612f0465145c9709a90b6d77ee3","ics26_client_id":"cosmoshub-1"}}]}`
		if err := os.WriteFile(configPath, []byte(config), configFilePerm); err != nil {
			t.Fatalf("write config: %v", err)
		}
		const testKey = "a81f9eb900c02f35d28ec80d1da79dde52cc4cc37d6fbaef5768ab59f32cbfdd"
		t.Setenv("ETH_PRIVATE_KEY", testKey)
		t.Setenv("COSMOS_PRIVATE_KEY", testKey)
		// Deliberately NOT set, so validateStartupConfig is the gate that stops this
		// run. That is the failure whose label this test is about.
		t.Setenv("COSMOS_CHAIN_ID", "")

		core, logs := observer.New(zapcore.InfoLevel)
		command := Start(zap.New(core))
		command.SetArgs([]string{"--config", configPath})
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		err = command.Execute()

		if err == nil {
			t.Fatal("start succeeded without COSMOS_CHAIN_ID; this test needs that gate to be the one that stops it")
		}
		if !strings.Contains(err.Error(), "COSMOS_CHAIN_ID") {
			t.Fatalf("start stopped at a different gate (%v); the ordering this test pins was not exercised", err)
		}
		// A line logged now, after the gate has reported, must already be labelled: the
		// stamp has to have happened before that gate, or by the time it fails the
		// standard logger still points at the buffer above.
		const probe = "[probe] logged after the read-based gate reported"
		log.Print(probe)
		entries := logs.All()
		if len(entries) == 0 || entries[len(entries)-1].Message != probe {
			t.Fatal("a standard-library line logged after the read-based gate reported its finding never " +
				"reached the scoped logger: every line from that gate onward is unattributable in a merged log")
		}
		if got := entries[len(entries)-1].ContextMap()[relayPathLogField]; !strings.Contains(fmt.Sprint(got), "cosmoshub-1") {
			t.Fatalf("standard-library line has %s = %v, want the relay path: every line from that gate "+
				"onward is unattributable in a merged log", relayPathLogField, got)
		}
	})

	t.Run("refuses legacy state before it gets anywhere", func(t *testing.T) {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(wd) })

		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		config := `{"modules":[{"name":"cosmos_to_eth","src_chain":"cosmoshub-1","config":{` +
			`"tm_rpc_url":"http://127.0.0.1:26657","eth_rpc_url":"http://127.0.0.1:8545",` +
			`"ics26_address":"0x80741a37e3644612f0465145c9709a90b6d77ee3","ics26_client_id":"cosmoshub-1"}}]}`
		if err := os.WriteFile(configPath, []byte(config), configFilePerm); err != nil {
			t.Fatalf("write config: %v", err)
		}
		// State from the previous layout, sitting where an upgrade would leave it.
		if err := os.Mkdir(filepath.Join(dir, legacyTrackerDirName), 0o700); err != nil {
			t.Fatalf("mkdir legacy tracker: %v", err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatalf("chdir: %v", err)
		}

		const testKey = "a81f9eb900c02f35d28ec80d1da79dde52cc4cc37d6fbaef5768ab59f32cbfdd"
		t.Setenv("ETH_PRIVATE_KEY", testKey)
		t.Setenv("COSMOS_PRIVATE_KEY", testKey)
		t.Setenv("PROVER_ADDR", "127.0.0.1:1")

		command := Start(zap.NewNop())
		command.SetArgs([]string{"--config", configPath})
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		command.SilenceUsage = true

		err = command.Execute()
		if err == nil {
			t.Fatal("start accepted a config sitting on top of the previous state layout")
		}
		if !strings.Contains(err.Error(), legacyTrackerDirName) {
			t.Fatalf("start failed for another reason (%v); the legacy check is not wired in, so an "+
				"upgrade would begin with an empty pending tracker and say nothing", err)
		}
	})

	// The guard is only worth having if `start` opts in, and nothing else checks
	// that it does. Asserted structurally because the alternative is booting a real
	// relayer.
	t.Run("opts into the wasm client ID requirement", func(t *testing.T) {
		// start.go, not main.go: #459 moved the command bodies out of main.go, and
		// this test is about what `start` passes, not about where it lives.
		src, err := os.ReadFile("start.go")
		if err != nil {
			t.Fatalf("read start.go: %v", err)
		}
		body := string(src)
		start := strings.Index(body, "buildCosmosToEthSourceOptions{")
		if start < 0 {
			t.Fatal("start no longer builds a cosmos_to_eth source")
		}
		end := strings.Index(body[start:], "}")
		if !strings.Contains(body[start:start+end], "relaysEVMToCosmos: true") {
			t.Fatal("start does not opt into the cosmos_wasm_client_id requirement; a missing id " +
				"would then surface at the first eth->cosmos packet instead of at startup")
		}
	})
}
