package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// doctorCheck is one line of zoo doctor: from zoo-doctor inside the cage
// ("PASS|FAIL|WARN <check>: <detail>") or a host-side check.
type doctorCheck struct {
	Verdict string `json:"verdict"` // PASS, FAIL, WARN, or NOTE for stray output
	Check   string `json:"check"`
	Detail  string `json:"detail"`
}

type doctorOpts struct {
	cage string
	deep bool
	json bool
}

func parseDoctorArgs(args []string) (doctorOpts, error) {
	var o doctorOpts
	for _, a := range args {
		switch {
		case a == "--deep":
			o.deep = true
		case a == "--json":
			o.json = true
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %s (zoo doctor [--deep] [--json] [cage])", a)
		case o.cage == "":
			o.cage = a
		default:
			return o, fmt.Errorf("one cage at a time (zoo doctor [--deep] [--json] [cage])")
		}
	}
	return o, nil
}

// parseDoctorOutput reads zoo-doctor's lines. Anything that isn't a verdict
// line (a stray error message) is kept as a NOTE so it isn't lost.
func parseDoctorOutput(out string) []doctorCheck {
	var res []doctorCheck
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		verdict, rest, _ := strings.Cut(line, " ")
		switch verdict {
		case "PASS", "FAIL", "WARN":
			check, detail, ok := strings.Cut(rest, ": ")
			if !ok {
				check, detail = strings.TrimSuffix(rest, ":"), ""
			}
			res = append(res, doctorCheck{verdict, check, detail})
		default:
			res = append(res, doctorCheck{"NOTE", "output", line})
		}
	}
	return res
}

// writeDoctorJSON prints one JSON object per check, one per line.
func writeDoctorJSON(w io.Writer, checks []doctorCheck) error {
	enc := json.NewEncoder(w)
	for _, c := range checks {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return nil
}

func writeDoctorText(w io.Writer, checks []doctorCheck, color bool) {
	paint := func(code, s string) string {
		if !color {
			return s
		}
		return "\033[" + code + "m" + s + "\033[0m"
	}
	for _, c := range checks {
		text := c.Check + ": " + c.Detail
		switch c.Verdict {
		case "PASS":
			fmt.Fprintln(w, paint("32", "  ✓ ")+text)
		case "FAIL":
			fmt.Fprintln(w, paint("31", "  ✗ ")+text)
		case "WARN":
			fmt.Fprintln(w, paint("33", "  ! ")+text)
		default:
			fmt.Fprintln(w, "    "+c.Detail)
		}
	}
}
