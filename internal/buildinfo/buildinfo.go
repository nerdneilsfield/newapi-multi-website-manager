// Package buildinfo carries release metadata injected with -ldflags.
package buildinfo

// Build metadata, overridden at link time.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)
