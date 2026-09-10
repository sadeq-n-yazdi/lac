// Command lac is the command-line client for the LAC coordination daemon. Agents and their operator
// use it to register, exchange messages, queue for shared resources and collect reports.
package main

import (
	"flag"
	"fmt"
	"os"

	"code.sadeq.uk/lac/internal/version"
)

func main() {
	flag.Parse()

	if flag.Arg(0) == "version" {
		fmt.Fprintf(os.Stdout, "lac %s\n", version.String())
		return
	}

	fmt.Fprintln(os.Stderr, "lac: the client is not implemented yet; see https://github.com/sadeq-n-yazdi/lac/issues")
	os.Exit(1)
}
