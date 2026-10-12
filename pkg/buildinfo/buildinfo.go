// Package buildinfo reports which build of a service is running.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Version is set at build time for releases:
//
//	go build -ldflags "-X parley/pkg/buildinfo.Version=1.2.3"
var Version = "dev"

// Info identifies a build. Commit comes from the VCS stamp the Go toolchain
// embeds when building inside a git checkout.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Modified  bool   `json:"modified,omitempty"` // built from a tree with uncommitted changes
	GoVersion string `json:"go_version"`
}

func Get() Info {
	info := Info{Version: Version, GoVersion: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
				if len(info.Commit) > 12 {
					info.Commit = info.Commit[:12]
				}
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	return info
}
