package shims

import "strings"

// withFirst returns a ';'-separated PATH value with dir first (once),
// and whether that changed anything. Comparison ignores case and a
// trailing backslash, as Windows does.
func withFirst(cur, dir string) (string, bool) {
	rest := without(cur, dir)
	next := dir
	if rest != "" {
		next += ";" + rest
	}
	return next, next != cur
}

// without drops dir (and empty entries) from a ';'-separated PATH value.
func without(cur, dir string) string {
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), `\/`)) }
	var keep []string
	for _, e := range strings.Split(cur, ";") {
		if strings.TrimSpace(e) == "" || norm(e) == norm(dir) {
			continue
		}
		keep = append(keep, e)
	}
	return strings.Join(keep, ";")
}
