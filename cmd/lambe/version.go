package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/andrew-bierman/lambe/internal/httpstate"
)

// versionInfo is the machine-readable form of `lambe version --json`.
type versionInfo struct {
	// Version is a SemVer 2.0 string with a leading "v": the release tag for
	// release builds, vX.Y.Z-dev+<shortsha> for development builds.
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	// APIVersion is the state API wire contract version, versioned
	// separately from the binary (see docs/RELEASING.md).
	APIVersion int `json:"api_version"`
}

// semverCore matches the leading vX.Y.Z of a (pseudo-)version.
var semverCore = regexp.MustCompile(`^v\d+\.\d+\.\d+`)

// resolveVersion maps link-time values and embedded build info to one
// version string. Release builds (goreleaser sets ldflags to the tag without
// its "v", `go install ...@vX.Y.Z` embeds the tag) report the tag verbatim.
// Everything else — a checkout build, whose Go-stamped pseudo-version is
// vX.Y.(Z+1)-0.<time>-<sha>[+dirty] — reports vX.Y.Z-dev+<shortsha>, which is
// valid SemVer and sorts below the release it precedes.
func resolveVersion(ldVersion, ldCommit, ldDate string, info *debug.BuildInfo) versionInfo {
	out := versionInfo{Version: ldVersion, Commit: ldCommit, Date: ldDate, APIVersion: httpstate.SchemaVersion}
	var mainVersion string
	dirty := false
	if info != nil {
		mainVersion = info.Main.Version
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if out.Commit == "none" {
					out.Commit = s.Value
				}
			case "vcs.time":
				if out.Date == "unknown" {
					out.Date = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if out.Version != "dev" {
		if !strings.HasPrefix(out.Version, "v") {
			out.Version = "v" + out.Version
		}
		return out
	}
	core := "v0.0.0"
	if m := semverCore.FindString(mainVersion); m != "" {
		core = m
		if !strings.Contains(mainVersion, "-") {
			out.Version = mainVersion // exact tag, e.g. go install @v0.9.2
			return out
		}
	}
	build := shortSHA(out.Commit)
	if dirty || strings.HasSuffix(mainVersion, "+dirty") {
		build += ".dirty"
	}
	out.Version = core + "-dev+" + build
	return out
}

func shortSHA(c string) string {
	if c == "none" || c == "" {
		return "unknown"
	}
	return c[:min(len(c), 7)]
}

func buildVersion() versionInfo {
	info, _ := debug.ReadBuildInfo()
	return resolveVersion(version, commit, date, info)
}

func (v versionInfo) String() string {
	return fmt.Sprintf("lambe %s (commit %s, built %s, state API v%d)", v.Version, v.Commit, v.Date, v.APIVersion)
}

func runVersion(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := noArgs(fs); err != nil {
		return err
	}
	v := buildVersion()
	if *asJSON {
		return json.NewEncoder(out).Encode(v)
	}
	_, err := fmt.Fprintln(out, v)
	return err
}
