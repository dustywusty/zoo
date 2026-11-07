//go:build !windows

package shims

import "errors"

// SetUserPath is Windows-only: elsewhere PATH lives in shell startup files,
// which zoo prints instructions for instead of editing.
func SetUserPath(string, bool) (bool, error) {
	return false, errors.New("--path is for Windows; add the shims dir to PATH in your shell's startup file")
}
