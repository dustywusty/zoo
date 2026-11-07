package services

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/config"
)

var all = map[string]config.Service{
	"pg":     {Host: "127.0.0.1:5432"},
	"redis":  {Image: "redis:7", Port: 6379},
	"gh-ssh": {Remote: "github.com:22"},
	"keys":   {SSHAgent: "bitwarden", Keys: []string{"github"}, Hosts: []string{"github.com"}},
}

func TestValidate(t *testing.T) {
	if err := Validate(all); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]map[string]config.Service{
		"two kinds":      {"x": {Host: "127.0.0.1:1", Remote: "a:2"}},
		"no port":        {"x": {Image: "redis:7"}},
		"bad host":       {"x": {Host: "localhost"}},
		"bad name":       {"X_Y": {Remote: "a:1"}},
		"squid port":     {"x": {Remote: "a:3128"}},
		"port collision": {"a": {Host: "127.0.0.1:5432"}, "b": {Remote: "db:5432"}},
	} {
		if Validate(bad) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPorts(t *testing.T) {
	if ListenPort(all["gh-ssh"]) != 20022 || Port(all["gh-ssh"]) != 22 {
		t.Error("low ports move up by 20000 so the gate needs no privileges")
	}
	if ListenPort(all["pg"]) != 5432 || ListenPort(all["redis"]) != 6379 || ListenPort(all["keys"]) != 22022 {
		t.Error("ports")
	}
}

func TestEffectiveAndGate(t *testing.T) {
	g := Grants{}
	g.Add("api-1", "pg")
	g.Add("api-1", "pg")
	if len(g["api-1"]) != 1 {
		t.Fatal("duplicate grant")
	}
	eff := Effective(all, g, "api-1", []string{"redis", "gone"})
	if strings.Join(eff, ",") != "pg,redis" {
		t.Fatalf("effective: %v (unknown services must be dropped)", eff)
	}
	gc := BuildGate(all, []CageRef{
		{Name: "api-1", IP: "10.0.0.2", Grants: eff, Via: "10.0.0.254"},
		{Name: "web-2", IP: "10.0.0.3", Grants: nil, Via: "10.0.1.254"},
		{Name: "old", IP: "10.0.0.4", Grants: []string{"redis"}}, // keeper address unknown
		{Name: "stopped", IP: "", Grants: []string{"pg"}},
	}, "tok", "192.168.64.1:7778", func(n string) string { return map[string]string{"redis": "10.9.0.5"}[n] })
	by := map[string]GateService{}
	for _, s := range gc.Services {
		by[s.Name] = s
	}
	if by["pg"].Allow["10.0.0.2"] != "api-1" || len(by["pg"].Allow) != 1 {
		t.Errorf("pg allow: %v", by["pg"].Allow)
	}
	if by["redis"].Target != "10.9.0.5:6379" || by["gh-ssh"].Target != "github.com:22" || by["pg"].Target != "pg" {
		t.Errorf("targets: %+v", gc.Services)
	}
	if len(by["gh-ssh"].Allow) != 0 {
		t.Error("ungranted service must allow nobody")
	}
	// Anti-spoofing: each granted cage IP carries the keeper address it dials.
	if by["pg"].Via["10.0.0.2"] != "10.0.0.254" || len(by["pg"].Via) != 1 {
		t.Errorf("pg via: %v", by["pg"].Via)
	}
	if by["redis"].Allow["10.0.0.4"] != "old" || len(by["redis"].Via) != 1 {
		t.Errorf("redis: a cage with no known keeper address is allowed but not checked: %+v", by["redis"])
	}
	if by["gh-ssh"].Via != nil {
		t.Error("ungranted service must have no via")
	}
	// Old gate.json stays valid: via is omitted when empty.
	b, _ := json.Marshal(by["gh-ssh"])
	if strings.Contains(string(b), "via") {
		t.Errorf("empty via serialized: %s", b)
	}
	if !g.Remove("api-1", "pg") || g.Remove("api-1", "pg") {
		t.Error("remove")
	}
}

func TestCageEnv(t *testing.T) {
	env := strings.Join(CageEnv(all, []string{"pg", "redis"}), " ")
	if !strings.Contains(env, "PGHOST=pg.zoo") || !strings.Contains(env, "REDIS_URL=redis://redis.zoo:6379") {
		t.Fatal(env)
	}
	s := strings.Join(SessionEnv(config.Global{Services: all}, []string{"keys"}), " ")
	if !strings.Contains(s, "ZOO_SSH_AGENT=keys.zoo:22022") {
		t.Fatal(s)
	}
}
