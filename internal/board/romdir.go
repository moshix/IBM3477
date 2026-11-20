package board

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"ibm3486/internal/roms"
)

// hasROMs reports whether fsys holds every ROM file of model m.
func hasROMs(fsys fs.FS, m *Model) bool {
	for _, r := range m.ROMs {
		if _, err := fs.Stat(fsys, r.Name); err != nil {
			return false
		}
	}
	return true
}

// ROMSource picks where the ROMs for model come from and describes it:
//  1. dir, when given (-romdir overrides everything);
//  2. the ROMs embedded in the binary (internal/roms);
//  3. the current directory, the executable's directory, or its parent.
func ROMSource(dir, model string) (fs.FS, string, error) {
	m, ok := Models[model]
	if !ok {
		return nil, "", fmt.Errorf("unknown model %q (use 3477 or 3486)", model)
	}
	if dir != "" {
		if !hasROMs(os.DirFS(dir), m) {
			return nil, "", fmt.Errorf("IBM %s ROMs not found in %s", model, dir)
		}
		return os.DirFS(dir), dir, nil
	}
	if emb := roms.FS(); hasROMs(emb, m) {
		return emb, "built-in", nil
	}
	cands := []string{"."}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		d := filepath.Dir(exe)
		cands = append(cands, d, filepath.Dir(d))
	}
	for _, d := range cands {
		if hasROMs(os.DirFS(d), m) {
			return os.DirFS(d), d, nil
		}
	}
	return nil, "", fmt.Errorf("IBM %s ROMs are not built in and not found in %v; "+
		"use -romdir DIR", model, cands)
}
