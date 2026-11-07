package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseDoctorArgs(t *testing.T) {
	o, err := parseDoctorArgs([]string{"--deep", "api-1", "--json"})
	if err != nil || !o.deep || !o.json || o.cage != "api-1" {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := parseDoctorArgs([]string{"--nope"}); err == nil {
		t.Error("unknown flag accepted")
	}
	if _, err := parseDoctorArgs([]string{"a", "b"}); err == nil {
		t.Error("two cages accepted")
	}
}

func TestDoctorOutput(t *testing.T) {
	out := "PASS direct-egress: blocked\nFAIL lateral: 10.0.0.7:22 reachable: other cage\nWARN init\nbash: oops\n\n"
	got := parseDoctorOutput(out)
	want := []doctorCheck{
		{"PASS", "direct-egress", "blocked"},
		{"FAIL", "lateral", "10.0.0.7:22 reachable: other cage"},
		{"WARN", "init", ""},
		{"NOTE", "output", "bash: oops"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	var buf bytes.Buffer
	if err := writeDoctorJSON(&buf, got); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(&buf)
	n := 0
	for sc.Scan() {
		var m map[string]string
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %d not JSON: %s", n, sc.Text())
		}
		if len(m) != 3 || m["verdict"] != want[n].Verdict || m["check"] != want[n].Check || m["detail"] != want[n].Detail {
			t.Errorf("line %d: %v", n, m)
		}
		n++
	}
	if n != len(want) {
		t.Errorf("%d JSON lines, want %d", n, len(want))
	}
	var text bytes.Buffer
	writeDoctorText(&text, got, false)
	if !strings.Contains(text.String(), "  ✗ lateral: 10.0.0.7:22") || !strings.Contains(text.String(), "    bash: oops") {
		t.Errorf("text:\n%s", text.String())
	}
}
