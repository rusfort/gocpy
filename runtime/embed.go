// Package runtime embeds the Gocpy C runtime sources into the compiler.
package runtime

import "embed"

// Files contains the C runtime: c/gocpy_rt.h, c/gocpy_rt.c and c/gocpy_lib.c.
//
//go:embed c/gocpy_rt.h c/gocpy_rt.c c/gocpy_lib.c
var Files embed.FS
