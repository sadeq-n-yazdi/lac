// Command lacd is the LAC coordination daemon. It listens on a Unix domain socket and arbitrates
// messaging, resource leases and reporting between the AI agents running on this machine.
package main

import (
	"flag"
	"fmt"
	"os"

	"code.sadeq.uk/lac/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Fprintf(os.Stdout, "lacd %s\n", version.String())
		return
	}

	fmt.Fprintln(os.Stderr, "lacd: the daemon is not implemented yet; see https://github.com/sadeq-n-yazdi/lac/issues")
	os.Exit(1)
}
