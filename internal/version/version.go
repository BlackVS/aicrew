// Package version reports which aicrew build is running. A release build
// stamps Override at link time:
//
//	-ldflags "-X github.com/BlackVS/aicrew/internal/version.Override=vX.Y.Z"
//
// Otherwise the module's build information gives the version, and a source
// build reports "dev" with the commit it was built from.
package version

import (
	"runtime"
	"runtime/debug"
)

// Override is the release version stamped by the release build.
var Override string

// Info is the running build.
type Info struct {
	Version  string `json:"version"`
	Commit   string `json:"commit,omitempty"`
	Modified bool   `json:"modified,omitempty"` // built from a tree with uncommitted changes
	Go       string `json:"go"`
}

// Get reports the running build.
func Get() Info {
	i := Info{Version: "dev", Go: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			i.Version = v
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				i.Commit = s.Value
			case "vcs.modified":
				i.Modified = s.Value == "true"
			}
		}
	}
	if Override != "" {
		i.Version = Override
	}
	return i
}
