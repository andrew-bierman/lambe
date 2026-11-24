// Package rexcontrib embeds the Rex integration so `lambe setup rex` can
// install exactly the version that shipped with the binary.
package rexcontrib

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
)

// FS holds the Lua sources under lua/: the lambe.lua shim and the lambe/
// modules it loads.
//
//go:embed lua
var FS embed.FS

// ShimDirPlaceholder is the quoted string literal in lambe.lua that
// `lambe setup rex` replaces with the Rex config directory.
const ShimDirPlaceholder = `"@LAMBE_DIR@"`

var versionRE = regexp.MustCompile(`VERSION\s*=\s*(\d+)`)

// Version is the plugin version declared in lua/lambe/version.lua.
func Version() (int, error) {
	data, err := FS.ReadFile("lua/lambe/version.lua")
	if err != nil {
		return 0, err
	}
	m := versionRE.FindSubmatch(data)
	if m == nil {
		return 0, fmt.Errorf("lua/lambe/version.lua declares no VERSION")
	}
	return strconv.Atoi(string(m[1]))
}

// Modules returns the module files under lua/lambe/, keyed by file name.
func Modules() (map[string][]byte, error) {
	entries, err := fs.ReadDir(FS, "lua/lambe")
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		data, err := FS.ReadFile("lua/lambe/" + e.Name())
		if err != nil {
			return nil, err
		}
		out[e.Name()] = data
	}
	return out, nil
}

// Shim returns lua/lambe.lua with its placeholder left in place.
func Shim() ([]byte, error) { return FS.ReadFile("lua/lambe.lua") }
