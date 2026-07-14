// Command leasepin is an HTTP lock service with leases and fencing
// tokens, plus a withlock wrapper that runs any command under a lock.
package main

import (
	"os"

	"github.com/JaydenCJ/leasepin/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
