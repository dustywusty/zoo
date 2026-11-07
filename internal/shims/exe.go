package shims

// Executable shims, for Windows. A .cmd shim would be simpler but breaks
// the tools zoo exists for: Node refuses to spawn .cmd/.bat without a shell
// (since the 2024 BatBadBut fix), and Orca, Conductor and friends are Node
// apps; batch files also mangle arguments. So each shim is a copy of
// zoo.exe named after the harness (claude.exe): started under that name
// from the shims dir, zoo acts as the shim (see Invoked) and hands off to
// the installed zoo recorded in the manifest, so upgrading zoo never leaves
// a stale copy deciding how agents run.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dusty/zoo/internal/paths"
)

// manifest lists the shims zoo wrote and the zoo they hand off to.
func manifest() string { return filepath.Join(paths.Shims(), ".zoo-shims") }

// exeShims is whether this OS gets executable shims.
var exeShims = runtime.GOOS == "windows"

func installExe(harnesses []string, zooBin string) ([]string, error) {
	var names, out []string
	for _, h := range harnesses {
		if h == "shell" {
			continue
		}
		dst := filepath.Join(paths.Shims(), h+".exe")
		_ = os.Remove(dst)
		if err := os.Link(zooBin, dst); err != nil {
			if err := copyFile(zooBin, dst); err != nil {
				return out, fmt.Errorf("shim %s: %w", h, err)
			}
		}
		names = append(names, h)
		out = append(out, dst)
	}
	body := "zoo " + zooBin + "\n" + strings.Join(names, "\n") + "\n"
	return out, os.WriteFile(manifest(), []byte(body), 0o644)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// readManifest returns the zoo to hand off to and the shim names.
func readManifest() (zooBin string, names []string) {
	f, err := os.Open(manifest())
	if err != nil {
		return "", nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if z, ok := strings.CutPrefix(l, "zoo "); ok {
			zooBin = z
		} else if l != "" {
			names = append(names, l)
		}
	}
	return zooBin, names
}

func uninstallExe() []string {
	_, names := readManifest()
	var removed []string
	for _, n := range names {
		p := filepath.Join(paths.Shims(), n+".exe")
		if os.Remove(p) == nil {
			removed = append(removed, p)
		}
	}
	_ = os.Remove(manifest())
	return removed
}

// Invoked reports whether this process is a shim: zoo started as
// <harness>.exe from the shims dir. It returns the harness name.
func Invoked() (string, bool) {
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	if !samePath(filepath.Dir(self), paths.Shims()) {
		return "", false
	}
	name := strings.TrimSuffix(filepath.Base(self), filepath.Ext(self))
	if strings.EqualFold(name, "zoo") {
		return "", false
	}
	return name, true
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Run is a shim's whole life: the real CLI when ZOO=off or already inside a
// cage, otherwise `zoo run <harness> -- args` with the installed zoo. It
// returns the exit code to leave with.
func Run(harness string, args []string) int {
	var cmd *exec.Cmd
	if strings.EqualFold(os.Getenv("ZOO"), "off") || os.Getenv("ZOO_INSIDE") != "" {
		real, err := FindReal(harness)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zoo shim:", err)
			return 127
		}
		cmd = exec.Command(real, args...)
	} else {
		zooBin, _ := readManifest()
		if zooBin == "" {
			fmt.Fprintln(os.Stderr, "zoo shim: no zoo recorded; run `zoo shims install` again")
			return 127
		}
		cmd = exec.Command(zooBin, append([]string{"run", harness, "--"}, args...)...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl-C reaches the child through the shared console; the shim just
	// waits for it to finish.
	signal.Ignore(os.Interrupt)
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "zoo shim:", err)
		return 127
	}
	return 0
}

// FindReal finds the real harness CLI on PATH, skipping the shims dir.
func FindReal(name string) (string, error) {
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = []string{".exe", ".cmd", ".bat", ".com"}
		if pe := os.Getenv("PATHEXT"); pe != "" {
			exts = strings.Split(strings.ToLower(pe), ";")
		}
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == "" || samePath(d, paths.Shims()) {
			continue
		}
		for _, e := range exts {
			p := filepath.Join(d, name+e)
			if st, err := os.Stat(p); err == nil && !st.IsDir() && (runtime.GOOS == "windows" || st.Mode()&0o111 != 0) {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("real %s not found on PATH", name)
}
