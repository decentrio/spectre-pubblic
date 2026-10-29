package utils

import (
	"os"
	"strings"
	"sync/atomic"
)

// Benchmark mode toggles per-call instrumentation that's useful for performance
// measurement but noisy in production. A single switch:
//
//   - RELAYER_BENCHMARK=1 (or --benchmark) → enables [bench][prover],
//     [bench][eth], [bench][cosmos] summary lines per tx/proof, and the
//     per-inner-call gas trace inside SendEthTxBatch (via
//     debug_traceTransaction).
const BenchEnvKey = "RELAYER_BENCHMARK"

var benchOverride atomic.Bool

// SetBenchEnabled lets tests force benchmark mode on/off without touching env.
func SetBenchEnabled(enabled bool) {
	benchOverride.Store(enabled)
}

// BenchEnabled reports whether benchmark-mode logging is on.
func BenchEnabled() bool {
	return benchOverride.Load() || envBool(BenchEnvKey)
}

func envBool(key string) bool {
	raw, ok := os.LookupEnv(key)
	return ok && parseBenchBool(raw)
}

func parseBenchBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "t", "yes", "y", "on", "enable", "enabled":
		return true
	default:
		return false
	}
}
