// Package buildinfo exposes metadata injected into release binaries at link time.
package buildinfo

var (
	Version  = "dev"
	BuildTag = "unknown"
	Commit   = "unknown"
	Date     = "unknown"
)
