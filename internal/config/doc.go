// Package config loads the lambe daemon configuration.
//
// Configuration values are resolved in a strict precedence order, lowest to
// highest: built-in defaults, then an optional config file, then environment
// variables, then command-line flags.
//
// # Environment variable flattening rule
//
// An environment variable maps to a config key by dropping the LAMBE_ prefix,
// lowercasing the remainder, and replacing the FIRST underscore with the
// section delimiter ("."). Config keys are exactly two levels deep
// (section.leaf), so the first underscore separates section from leaf key and
// any later underscores are literal parts of the leaf name:
//
//	supervisor.tick_seconds  -> LAMBE_SUPERVISOR_TICK_SECONDS
//	supervisor.attempt_cap   -> LAMBE_SUPERVISOR_ATTEMPT_CAP
//
// Leaf keys in new config sections must therefore never start a new level.
package config
