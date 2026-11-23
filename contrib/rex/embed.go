// Package rexcontrib embeds the Rex integration script so `lambe setup rex`
// can install exactly the version that shipped with the binary.
package rexcontrib

import _ "embed"

// LambeLua is contrib/rex/lambe.lua, the Lambe command-palette actions.
//
//go:embed lambe.lua
var LambeLua []byte
