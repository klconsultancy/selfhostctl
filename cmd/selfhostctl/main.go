// Command selfhostctl manages the lifecycle of one self-managed Cluster API
// management cluster. See https://github.com/klconsultancy/selfhostctl/issues/1.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at release time with -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "usage: selfhostctl version")
	return 2
}
