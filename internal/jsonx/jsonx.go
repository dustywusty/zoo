// Package jsonx digs values out of JSON whose exact shape we don't control
// (the Apple container CLI's --format json output can change between releases).
package jsonx

import (
	"regexp"
	"strconv"
	"strings"
)

// Walk visits every value in v depth-first with its dotted key path.
func Walk(v any, fn func(path string, val any)) {
	walk("", v, fn)
}

func walk(path string, v any, fn func(string, any)) {
	fn(path, v)
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			walk(p, c, fn)
		}
	case []any:
		for i, c := range t {
			walk(path+"["+strconv.Itoa(i)+"]", c, fn)
		}
	}
}

func lastKey(path string) string {
	if i := strings.LastIndexAny(path, ".]"); i >= 0 {
		return strings.ToLower(path[i+1:])
	}
	return strings.ToLower(path)
}

// String returns the first string whose final key equals one of keys (case-insensitive),
// preferring shallower paths.
func String(v any, keys ...string) string {
	best, bestDepth := "", 1<<30
	Walk(v, func(path string, val any) {
		s, ok := val.(string)
		if !ok || s == "" {
			return
		}
		k := lastKey(path)
		for _, want := range keys {
			if k == strings.ToLower(want) {
				d := strings.Count(path, ".") + strings.Count(path, "[")
				if d < bestDepth {
					best, bestDepth = s, d
				}
			}
		}
	})
	return best
}

// Map returns the first map[string]any found under a key named key.
func Map(v any, key string) map[string]any {
	var out map[string]any
	Walk(v, func(path string, val any) {
		if out != nil {
			return
		}
		if m, ok := val.(map[string]any); ok && lastKey(path) == strings.ToLower(key) {
			out = m
		}
	})
	return out
}

var ipRe = regexp.MustCompile(`^(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})(/\d+)?$`)

// IPOnNetwork finds the IPv4 address of an attachment to the named network:
// any object that mentions network (as a string value) and holds an IPv4-looking
// string. Falls back to the first IPv4 found anywhere when network is "".
func IPOnNetwork(v any, network string) string {
	var found string
	Walk(v, func(path string, val any) {
		if found != "" {
			return
		}
		m, ok := val.(map[string]any)
		if !ok {
			return
		}
		mentions := network == ""
		ip := ""
		for k, c := range m {
			s, ok := c.(string)
			if !ok {
				continue
			}
			if network != "" && s == network {
				mentions = true
			}
			lk := strings.ToLower(k)
			if mm := ipRe.FindStringSubmatch(s); mm != nil && !strings.Contains(lk, "gateway") && !strings.Contains(lk, "dns") && !strings.Contains(lk, "subnet") {
				ip = mm[1]
			}
		}
		if mentions && ip != "" {
			found = ip
		}
	})
	return found
}

// Number converts JSON numbers (float64) and numeric strings.
func Number(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(t), "%"), 64)
		return f, err == nil
	}
	return 0, false
}

// Numbers flattens every numeric leaf into path -> value.
func Numbers(v any) map[string]float64 {
	out := map[string]float64{}
	Walk(v, func(path string, val any) {
		switch val.(type) {
		case float64, int, int64:
			f, _ := Number(val)
			out[path] = f
		}
	})
	return out
}
