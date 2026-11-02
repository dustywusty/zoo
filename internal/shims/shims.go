// Package shims writes PATH shims so any tool that spawns `claude`, `codex`,
// etc. (Conductor, herdr, scripts, your terminal) lands in a cage.
package shims

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dusty/zoo/internal/paths"
)

const marker = "# zoo-shim"

// script runs the real CLI when ZOO=off or when already inside a cage,
// otherwise hands off to `zoo run <harness>`.
const script = `#!/bin/sh
%s for %[2]s
if [ "${ZOO:-on}" = "off" ] || [ -n "${ZOO_INSIDE:-}" ]; then
  shim_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
  IFS=:
  for d in $PATH; do
    [ "$d" = "$shim_dir" ] && continue
    if [ -x "$d/%[2]s" ]; then exec "$d/%[2]s" "$@"; fi
  done
  echo "zoo shim: real %[2]s not found on PATH" >&2
  exit 127
fi
exec %[3]s run %[2]s -- "$@"
`

// Install writes a shim per harness into ~/.zoo/shims.
func Install(harnesses []string, zooBin string) ([]string, error) {
	if err := os.MkdirAll(paths.Shims(), 0o755); err != nil {
		return nil, err
	}
	sort.Strings(harnesses)
	if exeShims {
		return installExe(harnesses, zooBin)
	}
	var out []string
	for _, h := range harnesses {
		if h == "shell" {
			continue
		}
		p := filepath.Join(paths.Shims(), h)
		body := fmt.Sprintf(script, marker, h, shellQuote(zooBin))
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Uninstall removes shims zoo wrote.
func Uninstall() ([]string, error) {
	if exeShims {
		return uninstallExe(), nil
	}
	ents, err := os.ReadDir(paths.Shims())
	if err != nil {
		return nil, nil
	}
	var removed []string
	for _, e := range ents {
		p := filepath.Join(paths.Shims(), e.Name())
		b, err := os.ReadFile(p)
		if err == nil && strings.Contains(string(b), marker) {
			if os.Remove(p) == nil {
				removed = append(removed, p)
			}
		}
	}
	return removed, nil
}

// OnPath reports whether the shim dir comes before other entries on PATH.
func OnPath() bool {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if samePath(d, paths.Shims()) {
			return true
		}
	}
	return false
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
