package main

import (
	"context"
	"fmt"
	"os"

	"morde-sync/client"
)

func main() {
	if len(os.Args) > 1 {
		if len(os.Args) == 6 && os.Args[1] == "--hub-command-runner" {
			if err := runHubCommandRunner(os.Args[2], os.Args[3], os.Args[4], os.Args[5]); err != nil {
				fmt.Fprintln(os.Stderr, "morde-sync: command sandbox setup failed")
				os.Exit(2)
			}
			return
		}
		if len(os.Args) == 3 && os.Args[1] == "--probe-operation-lock" {
			os.Exit(probeOperationLock(os.Args[2]))
		}
		fmt.Fprintln(os.Stderr, "morde-sync: unsupported arguments")
		os.Exit(2)
	}
	ctx, stop := commandSignalContext()
	defer stop()
	if err := runContext(ctx, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "morde-sync: stdio protocol stopped")
	}
}

func probeOperationLock(directory string) int {
	held, err := client.IsOperationLockHeld(context.Background(), directory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "morde-sync: probe operation lock: %v\n", err)
		return 2
	}
	if held {
		return 3
	}
	return 0
}
