// Command rencrow-harness is the standalone work-execution program of RenCrow_Harness.
// All behaviour lives in internal/cli; this file only connects it to the process.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Nyukimin/RenCrow_Harness/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
