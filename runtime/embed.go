// Package runtime embeds the Gocpy C runtime sources into the compiler.
package runtime

import "embed"

// Files contains c/gocpy_rt.h and c/gocpy_rt.c.
//
//go:embed c/gocpy_rt.h c/gocpy_rt.c
var Files embed.FS
