// Package paths knows where zoo keeps its state on the host.
package paths

import (
	"os"
	"path/filepath"
)

// Home is ~/.zoo, or $ZOO_HOME when set.
func Home() string {
	if h := os.Getenv("ZOO_HOME"); h != "" {
		return h
	}
	u, err := os.UserHomeDir()
	if err != nil {
		u = "."
	}
	return filepath.Join(u, ".zoo")
}

func Config() string                 { return filepath.Join(Home(), "config.yaml") }
func State() string                  { return filepath.Join(Home(), "state.json") }
func Token() string                  { return filepath.Join(Home(), "token") }
func DaemonPID() string              { return filepath.Join(Home(), "daemon.pid") }
func DaemonLog() string              { return filepath.Join(Home(), "daemon.log") }
func KeeperEtc() string              { return filepath.Join(Home(), "keeper", "etc") }
func KeeperLog() string              { return filepath.Join(Home(), "keeper", "log") }
func AccessLog() string              { return filepath.Join(KeeperLog(), "access.log") }
func Allowlist() string              { return filepath.Join(KeeperEtc(), "allowlist.txt") }
func LearnList() string              { return filepath.Join(KeeperEtc(), "learn.txt") }
func SquidConf() string              { return filepath.Join(KeeperEtc(), "squid.conf") }
func Shims() string                  { return filepath.Join(Home(), "shims") }
func Tmp() string                    { return filepath.Join(Home(), "tmp") }
func Build() string                  { return filepath.Join(Home(), "build") }
func Stage() string                  { return filepath.Join(Home(), "stage") }
func Grants() string                 { return filepath.Join(Home(), "grants.json") }
func GateToken() string              { return filepath.Join(Home(), "gate-token") }
func GateConf() string               { return filepath.Join(KeeperEtc(), "gate.json") }
func GateLog() string                { return filepath.Join(KeeperLog(), "gate.log") }
func ServiceData(name string) string { return filepath.Join(Home(), "services", name) }
func LearnCages() string             { return filepath.Join(KeeperEtc(), "learn-cages.txt") }

// MaskFile is the empty, read-only file mounted over secrets in cages. It
// sits alone in its own directory: some runtimes share a single-file mount's
// whole parent directory with the guest.
func MaskFile() string { return filepath.Join(Home(), "mask", "empty") }

// Ensure creates the directory tree. The keeper log dir is world-writable
// because squid writes it from inside its VM as the "proxy" user.
func Ensure() error {
	for _, d := range []string{Home(), KeeperEtc(), Shims(), Tmp(), Build()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(KeeperLog(), 0o777); err != nil {
		return err
	}
	_ = os.Chmod(KeeperLog(), 0o777)
	if err := os.MkdirAll(filepath.Dir(MaskFile()), 0o755); err != nil {
		return err
	}
	if st, err := os.Stat(MaskFile()); err != nil || st.Size() != 0 {
		_ = os.Remove(MaskFile())
		if err := os.WriteFile(MaskFile(), nil, 0o444); err != nil {
			return err
		}
	}
	// keeper/ itself must be traversable for the mount.
	_ = os.Chmod(filepath.Join(Home(), "keeper"), 0o755)
	_ = os.Chmod(KeeperEtc(), 0o755)
	return nil
}
