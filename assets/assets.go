// Package assets embeds the cage and keeper image sources so `zoo build`
// works from the single binary.
package assets

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed images
var images embed.FS

// WriteImage copies images/<name> into dir and returns the build context path.
func WriteImage(name, dir string) (string, error) {
	root := filepath.Join(dir, name)
	err := fs.WalkDir(images, "images/"+name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("images/"+name, p)
		dst := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := images.ReadFile(p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if filepath.Base(p) != "Dockerfile" {
			mode = 0o755
		}
		return os.WriteFile(dst, b, mode)
	})
	return root, err
}
