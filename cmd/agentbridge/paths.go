package main

import (
	"encoding/json"
	"io"

	"github.com/berkayahi/agentbridge/internal/buildinfo"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

// runtimePathsReport is the contract a host uses to locate this daemon's
// transports. It reports locations only: the local API secret is read by the
// host from its own owner-only file, never carried here.
type runtimePathsReport struct {
	EngineVersion   string `json:"engine_version"`
	LocalAPIVersion int    `json:"local_api_version"`
	DataDir         string `json:"data_dir"`
	Database        string `json:"database"`
	Attachments     string `json:"attachments"`
	Worktrees       string `json:"worktrees"`
	RuntimeDir      string `json:"runtime_dir"`
	ControlSocket   string `json:"control_socket"`
	LocalAPISocket  string `json:"local_api_socket"`
	LocalAPISecret  string `json:"local_api_secret"`
}

// reportRuntimePaths prints the derived runtime layout so a desktop host never
// duplicates deriveRuntimePaths and silently drifts from the daemon.
func reportRuntimePaths(stdout io.Writer, dataDir string) error {
	paths, err := deriveRuntimePaths(dataDir)
	if err != nil {
		return err
	}
	report := runtimePathsReport{
		EngineVersion:   buildinfo.Version,
		LocalAPIVersion: localcontrol.APIVersion,
		DataDir:         paths.data,
		Database:        paths.database,
		Attachments:     paths.attachments,
		Worktrees:       paths.worktrees,
		RuntimeDir:      paths.runtime,
		ControlSocket:   paths.controlSocket,
		LocalAPISocket:  paths.localAPI,
		LocalAPISecret:  paths.localAPISecret,
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}
