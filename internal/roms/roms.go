// Package roms holds the terminal ROM images embedded into the binaries.
// build.bash copies them into files/ before building; a build
// without them still works and falls back to ROM files on disk.
package roms

import (
	"embed"
	"io/fs"
)

//go:embed files
var files embed.FS

// FS returns the embedded ROM files (possibly none).
func FS() fs.FS {
	sub, err := fs.Sub(files, "files")
	if err != nil {
		panic(err) // the embedded directory always exists
	}
	return sub
}
