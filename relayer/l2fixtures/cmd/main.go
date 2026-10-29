// Command capture-l2-fixture captures reviewable fixed-block rollup evidence.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"relayer/l2fixtures"
)

func main() {
	configPath := flag.String("config", "", "capture configuration JSON path")
	outDir := flag.String("out", "", "output directory for fixture.json and provenance.json")
	flag.Parse()
	if *configPath == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "usage: capture-l2-fixture -config capture.json -out fixtures/config")
		os.Exit(2)
	}
	contents, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read config: %v\n", err)
		os.Exit(1)
	}
	var config l2fixtures.Config
	if err := json.Unmarshal(contents, &config); err != nil {
		fmt.Fprintf(os.Stderr, "decode config: %v\n", err)
		os.Exit(1)
	}
	fixture, provenance, err := l2fixtures.DialAndCapture(context.Background(), config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "capture: %v\n", err)
		os.Exit(1)
	}
	if err := l2fixtures.WriteCapture(*outDir, fixture, provenance); err != nil {
		fmt.Fprintf(os.Stderr, "write capture: %v\n", err)
		os.Exit(1)
	}
}
