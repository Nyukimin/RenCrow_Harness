// Command rencrow-harness is the standalone work-execution program of RenCrow_Harness.
// All behaviour lives in internal/cli; this file only connects it to the process.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Nyukimin/RenCrow_Harness/internal/cli"
	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
)

func main() {
	if err := fsperm.InitializeProcessOwner(); err != nil {
		fmt.Fprintf(os.Stderr, "rencrow-harness: private file ownership cannot be initialized: %v\n", fsperm.WithoutPath(err))
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
