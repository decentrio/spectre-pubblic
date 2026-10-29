package logbridge

import (
	"bytes"
	"fmt"
	"log"
	"runtime"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		in        string
		wantLevel zapcore.Level
		wantMsg   string
	}{
		{"WARN [SendEthTx] nonce too low", zapcore.WarnLevel, "[SendEthTx] nonce too low"},
		{"ERROR [Relay] DROP packet", zapcore.ErrorLevel, "[Relay] DROP packet"},
		{"DEBUG [Tick] nothing to do", zapcore.DebugLevel, "[Tick] nothing to do"},
		{"ERROR boom", zapcore.ErrorLevel, "boom"},
		{"WARN ", zapcore.WarnLevel, ""},

		// No token: info, and the message is untouched.
		{"[BatchBuilder] Flushing eth batch", zapcore.InfoLevel, "[BatchBuilder] Flushing eth batch"},
		{"", zapcore.InfoLevel, ""},

		// Near misses are message text, not tokens. A token has to be exactly one of
		// the three, first, and followed by a space -- these are what
		// TestLogCallsCarryCanonicalLevelTokens exists to keep out of the tree.
		{"WARNING: disk is full", zapcore.InfoLevel, "WARNING: disk is full"},
		{"WARN", zapcore.InfoLevel, "WARN"},
		{"WARN: x", zapcore.InfoLevel, "WARN: x"},
		{"warn x", zapcore.InfoLevel, "warn x"},
		{" WARN x", zapcore.InfoLevel, " WARN x"},
		{"INFO x", zapcore.InfoLevel, "INFO x"},
		{"[Relay] WARN x", zapcore.InfoLevel, "[Relay] WARN x"},
	}
	for _, tc := range cases {
		gotLevel, gotMsg := split(tc.in)
		if gotLevel != tc.wantLevel || gotMsg != tc.wantMsg {
			t.Errorf("split(%q) = (%v, %q), want (%v, %q)", tc.in, gotLevel, gotMsg, tc.wantLevel, tc.wantMsg)
		}
	}
}

// installObserved routes the standard logger into an observer and puts it back
// when the test ends, so no other test's log.Printf lands here.
func installObserved(t *testing.T, level zapcore.Level, opts ...zap.Option) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(level)
	t.Cleanup(Install(zap.New(core, opts...)))
	return logs
}

func TestInstall_RoutesEachSeverityAndStripsTheToken(t *testing.T) {
	logs := installObserved(t, zapcore.DebugLevel)

	log.Printf("DEBUG [Tick] %d packets queued", 0)
	log.Printf("[Relay] relayed seq=%d", 7)
	log.Printf("WARN [SendEthTx] nonce %d too low", 5)
	log.Printf("ERROR [Relay] DROP packet seq=%d: %v", 9, fmt.Errorf("reverted"))

	want := []struct {
		level zapcore.Level
		msg   string
	}{
		{zapcore.DebugLevel, "[Tick] 0 packets queued"},
		{zapcore.InfoLevel, "[Relay] relayed seq=7"},
		{zapcore.WarnLevel, "[SendEthTx] nonce 5 too low"},
		{zapcore.ErrorLevel, "[Relay] DROP packet seq=9: reverted"},
	}
	got := logs.All()
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Level != w.level || got[i].Message != w.msg {
			t.Errorf("entry %d = (%v, %q), want (%v, %q)", i, got[i].Level, got[i].Message, w.level, w.msg)
		}
	}
}

// The standard logger writes a timestamp and a prefix of its own. zap stamps the
// time and the relay path is a field, so a message that still carried either
// would print it twice -- the shape the old Lmsgprefix prefix would take if
// stampRelayPath kept setting it.
func TestInstall_NothingButTheMessageIsLogged(t *testing.T) {
	logs := installObserved(t, zapcore.InfoLevel)

	if got := log.Flags(); got != 0 {
		t.Errorf("log.Flags() = %d after Install, want 0", got)
	}
	if got := log.Prefix(); got != "" {
		t.Errorf("log.Prefix() = %q after Install, want empty", got)
	}

	log.Printf("[Relay] one line\n")
	log.Println("[Relay] two", "words")

	got := logs.All()
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Message != "[Relay] one line" {
		t.Errorf("message = %q, want the text with its newline trimmed and nothing added", got[0].Message)
	}
	if got[1].Message != "[Relay] two words" {
		t.Errorf("message = %q, want %q", got[1].Message, "[Relay] two words")
	}
}

func TestInstall_RespectsTheLoggersLevel(t *testing.T) {
	logs := installObserved(t, zapcore.WarnLevel)

	log.Printf("DEBUG hidden")
	log.Printf("hidden too")
	log.Printf("WARN shown")
	log.Printf("ERROR shown")

	got := logs.All()
	if len(got) != 2 || got[0].Message != "shown" || got[1].Message != "shown" {
		t.Fatalf("got %+v, want only the warn and the error", got)
	}
}

func TestInstall_RestoreReturnsTheStandardLogger(t *testing.T) {
	origFlags, origPrefix, origOut := log.Flags(), log.Prefix(), log.Writer()
	t.Cleanup(func() {
		log.SetFlags(origFlags)
		log.SetPrefix(origPrefix)
		log.SetOutput(origOut)
	})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[prefix] ")

	core, _ := observer.New(zapcore.InfoLevel)
	restore := Install(zap.New(core))
	restore()

	if log.Flags() != log.LstdFlags|log.Lmsgprefix || log.Prefix() != "[prefix] " || log.Writer() != &buf {
		t.Fatalf("restore left flags=%d prefix=%q writer=%v, want the originals back",
			log.Flags(), log.Prefix(), log.Writer())
	}
}

// The "caller" field is only useful if it names the statement that logged. The
// depth between zap and that statement is the standard library's, so this is the
// test that notices when a Go release moves it. The second call is how the
// relayer's injected loggers reach the bridge: log.Default(), not the package
// function.
func TestInstall_CallerIsTheLogStatement(t *testing.T) {
	logs := installObserved(t, zapcore.InfoLevel, zap.AddCaller())

	_, file, line, _ := runtime.Caller(0)
	log.Printf("[Relay] package-level")
	log.Default().Printf("[Relay] injected logger")

	got := logs.All()
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	for i, wantLine := range []int{line + 1, line + 2} {
		c := got[i].Caller
		if !c.Defined || c.File != file || c.Line != wantLine {
			t.Errorf("entry %d caller = %s:%d (defined=%t), want %s:%d", i, c.File, c.Line, c.Defined, file, wantLine)
		}
	}
}

// Concurrent writers are the normal case: every subscriber, scanner and relay
// loop logs from its own goroutine. Run with -race.
func TestInstall_ConcurrentWriters(t *testing.T) {
	logs := installObserved(t, zapcore.DebugLevel)

	const writers, each = 8, 100
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				switch i % 3 {
				case 0:
					log.Printf("WARN [w%d] %d", w, i)
				case 1:
					log.Printf("ERROR [w%d] %d", w, i)
				default:
					log.Printf("[w%d] %d", w, i)
				}
			}
		}(w)
	}
	wg.Wait()

	if got := logs.Len(); got != writers*each {
		t.Fatalf("got %d entries, want %d", got, writers*each)
	}
}
