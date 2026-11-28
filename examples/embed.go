// Package examples embeds the starter arcus configuration in this directory
// for arcus-install. arcus ignores this file if the directory is copied to
// ~/.config/arcus.
package examples

import "embed"

// FS holds the starter config.toml, modules/ and profiles/ trees.
//
//go:embed config.toml all:modules all:profiles
var FS embed.FS
