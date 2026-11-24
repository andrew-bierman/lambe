// Command gen writes the generated configuration reference.
//
//	go run ./internal/config/gen docs/reference/config.md
package main

import (
	"fmt"
	"os"

	"github.com/andrew-bierman/lambe/internal/config"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen OUTPUT.md")
		os.Exit(2)
	}
	if err := os.WriteFile(os.Args[1], []byte(config.ReferenceMarkdown()), 0o600); err != nil { //nolint:gosec // G703: operator-supplied output path of a dev tool
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}
