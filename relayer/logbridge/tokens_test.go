package logbridge

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// logMethods are the standard logger's printing methods, which is how the relay
// engine logs: the package-level log.Printf and the *log.Logger it injects.
var logMethods = map[string]bool{
	"Printf": true, "Print": true, "Println": true,
	"Fatalf": true, "Fatal": true, "Fatalln": true,
	"Panicf": true, "Panic": true, "Panicln": true,
}

var (
	// A level word that is not one of the three tokens. split would log it at info
	// with the word left in the message.
	notAToken = regexp.MustCompile(`^(WARNING|ERR|FATAL|TRACE|PANIC|CRITICAL|INFO)\b`)
	// A real token glued to what follows by anything other than one space.
	badGlue = regexp.MustCompile(`^(DEBUG|WARN|ERROR)([^ A-Za-z0-9_]|  )`)
	// A real token behind the [Component] tag, where split does not look.
	behindTag = regexp.MustCompile(`^\[[^\]]*\] (DEBUG|WARN|ERROR) `)
)

// leadingLiteral returns the string a format argument starts with: the literal
// itself, or the left-most one of a "a" + b concatenation.
func leadingLiteral(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return leadingLiteral(v.X)
		}
	}
	return "", false
}

// tokenProblems parses the non-test Go files under root and describes every log
// call that misuses the token convention. It also returns how many log calls it
// looked at, so a caller can tell "clean" from "looked in the wrong place".
func tokenProblems(root string) (problems []string, scanned int, err error) {
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "bindings", "node_modules", "vendor", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !logMethods[sel.Sel.Name] {
				return true
			}
			msg, ok := leadingLiteral(call.Args[0])
			if !ok {
				return true
			}
			scanned++
			pos := fset.Position(call.Pos())
			switch {
			case notAToken.MatchString(msg):
				problems = append(problems, fmt.Sprintf("%s: %q starts with a level word that is not a token; "+
					"use WARN, ERROR or DEBUG, or nothing for info", pos, msg))
			case badGlue.MatchString(msg):
				problems = append(problems, fmt.Sprintf("%s: %q: a level token must be followed by exactly one space", pos, msg))
			case behindTag.MatchString(msg):
				problems = append(problems, fmt.Sprintf("%s: %q: the level token must come before the [Component] tag", pos, msg))
			case strings.Contains(msg, "ATTENTION") && !strings.HasPrefix(msg, Warn) && !strings.HasPrefix(msg, Error):
				problems = append(problems, fmt.Sprintf("%s: %q: an ATTENTION line asks an operator to look, "+
					"so it must start with WARN or ERROR; unmarked it is info and a viewer will not surface it", pos, msg))
			}
			return true
		})
		return nil
	})
	sort.Strings(problems)
	return problems, scanned, err
}

// The token convention is stringly typed, and its failure is silent: a message
// that starts "WARNING " or "ERR " is logged at info with the word still in it,
// which looks fine in a terminal and is wrong in the one place it matters, the
// log viewer. So the tree is checked rather than trusted.
//
// It reads Go source for one narrow property, as cmd's own tests already do for
// start.go, and it covers every package because the convention is the engine's
// and not any one package's.
func TestLogCallsCarryCanonicalLevelTokens(t *testing.T) {
	root := ".." // the relayer module, from this package's directory
	problems, scanned, err := tokenProblems(root)
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("scanned no log calls under %s; the check is looking in the wrong place", root)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// A check that has never failed has not been shown to check anything. This runs it
// over a tree that makes each mistake once, next to calls that are right, and
// expects exactly the mistakes back.
func TestTokenCheckCatchesTheMistakes(t *testing.T) {
	dir := t.TempDir()
	src := `package x

import "log"

type l struct{ logger *log.Logger }

func f(s l) {
	log.Printf("WARN [A] fine")
	log.Printf("ERROR [A] fine")
	log.Printf("DEBUG [A] fine")
	log.Printf("[A] no token is info")
	s.logger.Printf("WARN [A] a concatenation " + "is read from its first literal")
	log.Printf("WARNING [A] not a token")
	log.Printf("ERR [A] not a token")
	log.Printf("INFO [A] info has no token")
	log.Printf("WARN: [A] glued by a colon")
	log.Printf("ERROR  [A] two spaces")
	log.Printf("[A] WARN behind the tag")
	log.Fatalf("FATAL [A] not a token")
	log.Print("WARN ", "fine")
	log.Printf("[A][ATTENTION] unmarked is info")
	log.Printf("DEBUG [A][ATTENTION] debug is hidden by default")
	log.Printf("WARN [A][ATTENTION] fine")
	log.Printf("ERROR [A] ATTENTION: fine")
}
`
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	// A test file is not scanned: tests print whatever they like.
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"),
		[]byte("package x\n\nimport \"log\"\n\nfunc g() { log.Printf(\"WARNING in a test\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	problems, scanned, err := tokenProblems(dir)
	if err != nil {
		t.Fatal(err)
	}
	if scanned != 17 {
		t.Errorf("scanned %d log calls, want 17 (the test file must not count)", scanned)
	}
	wantSubstrings := []string{
		`"WARNING [A] not a token"`,
		`"ERR [A] not a token"`,
		`"INFO [A] info has no token"`,
		`"WARN: [A] glued by a colon"`,
		`"ERROR  [A] two spaces"`,
		`"[A] WARN behind the tag"`,
		`"FATAL [A] not a token"`,
		`"[A][ATTENTION] unmarked is info"`,
		`"DEBUG [A][ATTENTION] debug is hidden by default"`,
	}
	if len(problems) != len(wantSubstrings) {
		t.Fatalf("got %d problems, want %d:\n%s", len(problems), len(wantSubstrings), strings.Join(problems, "\n"))
	}
	for _, want := range wantSubstrings {
		found := false
		for _, p := range problems {
			if strings.Contains(p, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no problem reported for %s; got:\n%s", want, strings.Join(problems, "\n"))
		}
	}
}
