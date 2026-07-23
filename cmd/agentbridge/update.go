package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/update"
)

func runUpdateCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	metadataPath := flags.String("metadata", "", "protected local signed metadata file")
	trustRootPath := flags.String("trust-root", "", "protected local update trust-root file")
	targetPath := flags.String("target", "", "absolute installed AgentBridge binary path")
	stagedPath := flags.String("staged", "", "absolute staged candidate binary path")
	floorPath := flags.String("floor", "", "absolute protected update floor path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	paths := []*string{metadataPath, trustRootPath, targetPath, stagedPath, floorPath}
	for _, path := range paths {
		if strings.TrimSpace(*path) == "" || !filepath.IsAbs(filepath.Clean(*path)) {
			fmt.Fprintln(stderr, "agentbridge: update requires protected absolute local paths")
			return 2
		}
	}
	metadata, err := update.ReadMetadataFile(*metadataPath)
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: update metadata is not trusted")
		return 1
	}
	root, err := update.ReadTrustRootFile(*trustRootPath)
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: update trust root is not trusted")
		return 1
	}
	floor, err := update.NewFileFloorStore(*floorPath)
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: update floor is not trusted")
		return 1
	}
	now := time.Now().UTC()
	current, err := floor.Load(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: update floor could not be read")
		return 1
	}
	if err := update.Verify(now, metadata, root, current); err != nil {
		fmt.Fprintln(stderr, "agentbridge: update metadata verification failed")
		return 1
	}
	installer := update.Installer{
		Target: *targetPath,
		Verify: func(_ context.Context, candidate update.Metadata) error {
			return update.Verify(now, candidate, root, current)
		},
		Health: func(healthCtx context.Context, target string) error {
			command := exec.CommandContext(healthCtx, target, "version")
			command.Stdout = io.Discard
			command.Stderr = io.Discard
			return command.Run()
		},
		Floor: floor,
	}
	if err := installer.Install(ctx, metadata, *stagedPath, now); err != nil {
		fmt.Fprintln(stderr, "agentbridge: update failed and rollback was attempted")
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "updated AgentBridge to %s (%s)\n", metadata.Identity.ProductVersion, metadata.Identity.BuildTag); err != nil {
		return 1
	}
	return 0
}
