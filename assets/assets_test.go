package assets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Image sources run in Linux; a CRLF shebang ("#!/bin/sh\r") doesn't.
func TestWriteImageIsLF(t *testing.T) {
	for _, name := range []string{"cage", "keeper"} {
		dir, err := WriteImage(name, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, _ := os.ReadFile(p)
			if bytes.Contains(b, []byte("\r")) {
				t.Errorf("%s has CR line endings", p)
			}
			return nil
		})
	}
}
