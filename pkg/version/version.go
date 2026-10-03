// Package version reports which build of decypharr is running.
//
// Release builds write "<version> <channel>" into release.txt before
// compiling (see the Dockerfile and .github/workflows/release.yml); the file
// is embedded, so nothing is injected with -ldflags. Other builds leave it
// empty and fall back to the Go build information: the module version when
// built with `go install`, otherwise "dev" plus the VCS revision.
package version

import (
	_ "embed"
	"fmt"
	"runtime/debug"
	"strings"
)

// Info identifies a build.
type Info struct {
	Version string `json:"version"`
	Channel string `json:"channel"`
}

func (i Info) String() string {
	return fmt.Sprintf("%s-%s", i.Version, i.Channel)
}

// devChannel is the channel of a build that is not a release.
const devChannel = "dev"

// shortRevision is how much of a VCS revision a development version shows.
const shortRevision = 12

//go:embed release.txt
var release string

// GetInfo returns the running build's version and channel.
func GetInfo() Info {
	buildInfo, _ := debug.ReadBuildInfo()
	return resolve(release, buildInfo)
}

// resolve prefers the release stamp, then the build information.
func resolve(stamp string, buildInfo *debug.BuildInfo) Info {
	fields := strings.Fields(stamp)
	info := Info{Channel: devChannel}
	if len(fields) > 0 {
		info.Version = fields[0]
	}
	if len(fields) > 1 {
		info.Channel = fields[1]
	}
	if info.Version == "" {
		info.Version = buildVersion(buildInfo)
	}
	return info
}

// buildVersion derives a version from Go build information: the module
// version for `go install`ed builds, else "dev", plus the VCS revision (and
// "-dirty" for uncommitted changes) when the build recorded one.
func buildVersion(buildInfo *debug.BuildInfo) string {
	if buildInfo == nil {
		return devChannel
	}
	if v := buildInfo.Main.Version; v != "" && v != "(devel)" {
		return strings.TrimPrefix(v, "v")
	}
	var revision string
	var modified bool
	for _, setting := range buildInfo.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return devChannel
	}
	version := devChannel + "+" + revision[:min(len(revision), shortRevision)]
	if modified {
		version += "-dirty"
	}
	return version
}
