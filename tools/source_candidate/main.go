package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/internal/sourcecandidate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if err := fsperm.InitializeProcessOwner(); err != nil {
		return errors.New("source candidate process owner could not be initialized")
	}
	if len(args) == 0 {
		return errors.New("source candidate command is required")
	}
	switch args[0] {
	case "prepare-logs":
		return prepareLogs(args[1:])
	case "capture":
		return capture(args[1:])
	case "verify":
		return verify(args[1:])
	default:
		return errors.New("unknown source candidate command")
	}
}

func prepareLogs(args []string) error {
	flags := flag.NewFlagSet("prepare-logs", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	runs := flags.String("owner-runs", "", "owner Tmp/test-runtime/_runs directory")
	directory := flags.String("directory", "", "fresh private source-candidate log directory below owner _runs")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid log-directory arguments")
	}
	if flags.NArg() != 0 || *runs == "" || *directory == "" {
		return errors.New("prepare-logs requires --owner-runs and --directory")
	}
	if err := sourcecandidate.PrepareLogDirectory(*runs, *directory); err != nil {
		return err
	}
	fmt.Println("status=private-log-directory")
	return nil
}

func capture(args []string) error {
	flags := flag.NewFlagSet("capture", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	source := flags.String("source", "", "source repository root")
	runs := flags.String("owner-runs", "", "owner Tmp/test-runtime/_runs directory")
	workspace := flags.String("workspace", "", "fresh private workspace below owner _runs")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid capture arguments")
	}
	if flags.NArg() != 0 || *source == "" || *runs == "" || *workspace == "" {
		return errors.New("capture requires --source, --owner-runs, and --workspace")
	}
	manifest, err := sourcecandidate.Capture(*source, *runs, *workspace)
	if err != nil {
		return err
	}
	fmt.Printf("status=%s candidateId=%s entries=%d\n", manifest.Status, manifest.CandidateID, len(manifest.Entries))
	return nil
}

func verify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	runs := flags.String("owner-runs", "", "owner Tmp/test-runtime/_runs directory")
	workspace := flags.String("workspace", "", "private source candidate workspace")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid verify arguments")
	}
	if flags.NArg() != 0 || *runs == "" || *workspace == "" {
		return errors.New("verify requires --owner-runs and --workspace")
	}
	manifest, err := sourcecandidate.Verify(*runs, *workspace)
	if err != nil {
		return err
	}
	fmt.Printf("status=%s candidateId=%s entries=%d\n", manifest.Status, manifest.CandidateID, len(manifest.Entries))
	return nil
}
