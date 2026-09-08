package observability

// BuildVersion is set via ldflags at build time.
var BuildVersion = "dev"

// BuildCommit is set via ldflags at build time.
var BuildCommit = "unknown"

// BuildTime is set via ldflags at build time.
var BuildTime = ""

// VersionInfo returns a structured string of build metadata.
func VersionInfo() string {
	info := BuildVersion
	if BuildCommit != "" && BuildCommit != "unknown" {
		info += "@" + BuildCommit
	}
	if BuildTime != "" {
		info += " (built " + BuildTime + ")"
	}
	return info
}
