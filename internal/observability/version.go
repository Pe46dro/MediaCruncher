package observability

var (
	// BuildVersion holds the compiled version string (e.g. v1.0.0 or dev)
	BuildVersion = "dev"
	// BuildCommit holds the git commit hash at build time
	BuildCommit = "unknown"
	// BuildTime holds the timestamp when the binary was built
	BuildTime = "unknown"
)
