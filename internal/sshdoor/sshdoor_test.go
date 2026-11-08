package sshdoor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestCageFromHost(t *testing.T) {
	for in, want := range map[string]string{
		"zoo-myproj-1a2b3c": "myproj-1a2b3c",
		"myproj-1a2b3c":     "myproj-1a2b3c",
		" zoo-x ":           "x",
	} {
		if got := CageFromHost(in); got != want {
			t.Errorf("CageFromHost(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "a b", "x;rm", "A", "x$(y)", "x/y", strings.Repeat("a", 65)} {
		if ValidCageName(bad) {
			t.Errorf("ValidCageName(%q) = true", bad)
		}
	}
	if !ValidCageName("myproj-1a2b3c") {
		t.Error("real cage name rejected")
	}
}

func TestClientConfig(t *testing.T) {
	cfg, err := ClientConfig("/opt/zoo bin/zoo", "~/.zoo/ssh/id_ed25519", "", []string{"web-abc123", "api-def456", "web-abc123", "bad name"})
	if err != nil {
		t.Fatal(err)
	}
	hosts := regexp.MustCompile(`(?m)^Host (.*)$`).FindAllStringSubmatch(cfg, -1)
	var got []string
	for _, h := range hosts {
		got = append(got, h[1])
	}
	if strings.Join(got, ",") != "zoo-web-abc123,zoo-api-def456,zoo-*" {
		t.Fatalf("hosts = %v (duplicates and invalid names dropped, catch-all last)", got)
	}
	blocks := strings.Split(cfg, "\nHost ")[1:]
	for _, b := range blocks {
		for _, want := range []string{
			`ProxyCommand "/opt/zoo bin/zoo" ssh-proxy %n`,
			"IdentityFile ~/.zoo/ssh/id_ed25519",
			"User agent", "IdentitiesOnly yes", "ForwardAgent no", "ForwardX11 no",
			"ClearAllForwardings no", "StrictHostKeyChecking no",
			"UserKnownHostsFile /dev/null", "LogLevel ERROR",
		} {
			if !strings.Contains(b, "\n  "+want+"\n") {
				t.Errorf("block %q missing %q", strings.SplitN(b, "\n", 2)[0], want)
			}
		}
		for _, bad := range []string{"RemoteForward", "ForwardAgent yes", "ForwardX11 yes", "DynamicForward"} {
			if strings.Contains(b, bad) {
				t.Errorf("block has %q", bad)
			}
		}
	}
	if !strings.Contains(cfg, "Include ~/.zoo/ssh/config") {
		t.Error("config should say how to include it")
	}
}

func TestClientConfigZooHomeAndEscapes(t *testing.T) {
	cfg, err := ClientConfig("/x/zoo%1", "/h/ssh/id_ed25519", "/h", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "ProxyCommand env ZOO_HOME=/h /x/zoo%%1 ssh-proxy %n\n") {
		t.Errorf("ZOO_HOME not passed through or %% not escaped:\n%s", cfg)
	}
	if strings.Count(cfg, "\nHost ") != 1 {
		t.Error("no cages: only the catch-all")
	}
	for _, bad := range []string{`/a"b/zoo`, "/a$b/zoo", "/a`b`/zoo", "/a\nb"} {
		if _, err := ClientConfig(bad, "k", "", nil); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
}

func TestEnsureClientKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ssh")
	line, err := EnsureClientKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, KeyOptions+" ssh-ed25519 ") {
		t.Fatalf("authorized_keys line = %q", line)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil {
		t.Fatalf("not an authorized_keys line: %v", err)
	}
	for p, mode := range map[string]os.FileMode{dir: 0o700, KeyPath(dir): 0o600} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != mode {
			t.Errorf("%s mode %v, want %v", p, st.Mode().Perm(), mode)
		}
	}
	again, err := EnsureClientKey(dir)
	if err != nil || again != line {
		t.Errorf("second call made a new key: %q vs %q (%v)", again, line, err)
	}
	b, _ := os.ReadFile(KeyPath(dir))
	if _, err := ssh.ParsePrivateKey(b); err != nil {
		t.Errorf("private key unreadable by ssh: %v", err)
	}
}

func TestEnvConf(t *testing.T) {
	conf, skipped := EnvConf([]string{
		"ZOO_CAGE=web", "HTTPS_PROXY=http://10.0.0.2:3128", "PGHOST=pg.zoo",
		"SPACED=a b # c", `QUOTE=a"b`, `BS=a\b`, "NL=a\nb", "1BAD=x", "NOEQ", "ZOO_CAGE=dup",
	})
	if strings.Count(conf, "SetEnv") != 1 {
		t.Fatalf("want exactly one SetEnv line (sshd keeps only the first):\n%s", conf)
	}
	want := `SetEnv "ZOO_CAGE=web" "HTTPS_PROXY=http://10.0.0.2:3128" "PGHOST=pg.zoo" "SPACED=a b # c"` + "\n"
	if !strings.HasSuffix(conf, want) {
		t.Errorf("conf = %q, want suffix %q", conf, want)
	}
	if strings.Join(skipped, ",") != "QUOTE,BS,NL,1BAD,NOEQ" {
		t.Errorf("skipped = %v", skipped)
	}
	if c, _ := EnvConf(nil); strings.Contains(c, "SetEnv") {
		t.Error("empty env should not write SetEnv")
	}
}

func TestBaseEnvHasNoCredentials(t *testing.T) {
	for _, e := range BaseEnv("web") {
		k, _, _ := strings.Cut(e, "=")
		if strings.Contains(k, "TOKEN") || strings.Contains(k, "KEY") || strings.Contains(k, "SECRET") {
			t.Errorf("credential-like variable %s in the SSH environment", k)
		}
	}
}

// The image's sshd_config is the security boundary for the door: check
// every setting the design depends on.
func TestSSHDConfig(t *testing.T) {
	b, err := os.ReadFile("../../assets/images/cage/sshd_config")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, v, _ := strings.Cut(l, " ")
		if _, dup := got[k]; dup {
			t.Errorf("%s set twice (sshd keeps the first)", k)
		}
		got[k] = strings.TrimSpace(v)
	}
	want := map[string]string{
		"HostKey":                      GuestHostKeyDir + "/host_ed25519",
		"AuthorizedKeysFile":           GuestAuthorizedKeys,
		"PasswordAuthentication":       "no",
		"KbdInteractiveAuthentication": "no",
		"PubkeyAuthentication":         "yes",
		"UsePAM":                       "no",
		"StrictModes":                  "no",
		"PidFile":                      "none",
		"AllowAgentForwarding":         "no",
		"AllowTcpForwarding":           "local",
		"AllowStreamLocalForwarding":   "no",
		"GatewayPorts":                 "no",
		"PermitTunnel":                 "no",
		"X11Forwarding":                "no",
		"PermitUserEnvironment":        "no",
		"PermitUserRC":                 "no",
		"PermitTTY":                    "yes",
		"Include":                      GuestEnvConf,
		"Subsystem":                    "sftp internal-sftp",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("sshd_config %s = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"ListenAddress", "Port", "AcceptEnv", "ForceCommand", "PermitRootLogin", "SetEnv"} {
		if _, ok := got[k]; ok {
			t.Errorf("sshd_config sets %s; it shouldn't", k)
		}
	}
}

func TestDockerfileInstallsDoor(t *testing.T) {
	b, err := os.ReadFile("../../assets/images/cage/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"openssh-server", "rm -f /etc/ssh/ssh_host_*", "COPY sshd_config " + GuestConfig, "chown root:root " + GuestConfig, GuestAuthorizedKeys, GuestEnvConf} {
		if !strings.Contains(s, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
}

func TestScripts(t *testing.T) {
	if !strings.Contains(SSHDScript, "exec "+GuestSSHD+" -i -e -f "+GuestConfig) {
		t.Error("SSHDScript must exec sshd in inetd mode with zoo's config")
	}
	// Everything before exec must keep stdout clean: it is the connection.
	if !strings.Contains(SSHDScript, ">/dev/null 2>&1") {
		t.Error("ssh-keygen output must not reach stdout")
	}
	for _, want := range []string{"exit 3", "mv -f \"$t\" " + GuestAuthorizedKeys, "mv -f \"$t\" " + GuestEnvConf, "chmod 0644"} {
		if !strings.Contains(InstallScript, want) {
			t.Errorf("InstallScript missing %q", want)
		}
	}
}
