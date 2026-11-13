// Command routeparity checks that a Gateway API setup behaves like the
// ingress-nginx setup it replaces.
package main

import (
	"os"

	"github.com/asaleem9/routeparity/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
}
