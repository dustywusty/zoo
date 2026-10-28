package jsonx

import (
	"encoding/json"
	"testing"
)

func TestIPOnNetwork(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"configuration":{"id":"zoo-keeper","labels":{"zoo.role":"keeper"}},
	  "status":"running",
	  "networks":[{"network":"default","address":"192.168.64.3/24","gateway":"192.168.64.1"},
	              {"network":"zoo-cages","address":"192.168.65.2/24","gateway":"192.168.65.1"}]}`), &v)
	if got := IPOnNetwork(v, "zoo-cages"); got != "192.168.65.2" {
		t.Fatalf("got %q", got)
	}
	if got := String(v, "id"); got != "zoo-keeper" {
		t.Fatalf("id %q", got)
	}
	if m := Map(v, "labels"); m["zoo.role"] != "keeper" {
		t.Fatalf("labels %v", m)
	}
}
