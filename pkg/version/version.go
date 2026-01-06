package version

import (
	"fmt"
	"runtime"
)

// Build-time variables set via ldflags
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// Info returns formatted version information
func Info() string {
	return fmt.Sprintf("fastsync %s (%s) built %s %s/%s",
		Version, Commit, BuildDate, runtime.GOOS, runtime.GOARCH)
}

// Short returns just the version string
func Short() string {
	return Version
}
