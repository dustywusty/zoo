package main

import (
	"fmt"
	"strings"
)

// usageError makes the fake exit 2: the real CLI would reject the call too,
// or the fake doesn't know it (so the contract suite notices).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// flagSet describes the flags one subcommand accepts. Keys are spellings
// ("-d", "--detach"); values are the canonical name plus whether a value follows.
type flagSet map[string]flagDef

type flagDef struct {
	canon string
	value bool
}

// defs builds a flagSet from specs like "-d,--detach" (boolean) or
// "-e,--env=" (takes a value). The canonical name is the last spelling
// without dashes.
func defs(specs ...string) flagSet {
	fs := flagSet{}
	for _, s := range specs {
		value := strings.HasSuffix(s, "=")
		s = strings.TrimSuffix(s, "=")
		names := strings.Split(s, ",")
		canon := strings.TrimLeft(names[len(names)-1], "-")
		for _, n := range names {
			fs[n] = flagDef{canon: canon, value: value}
		}
	}
	return fs
}

// parsed holds flag values by canonical name; booleans are "true".
type parsed map[string][]string

func (p parsed) has(k string) bool { return len(p[k]) > 0 }

func (p parsed) last(k string) string {
	if v := p[k]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

// parse reads flags from args. With stopAtPositional, parsing ends at the
// first positional argument (run/exec: image or id, then the command);
// otherwise flags and positionals may be interleaved.
func parse(cmd string, fs flagSet, args []string, stopAtPositional bool) (parsed, []string, error) {
	p := parsed{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return p, append(pos, args[i+1:]...), nil
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if stopAtPositional {
				return p, append(pos, args[i:]...), nil
			}
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") {
			hasVal = false
			name = a
		}
		def, ok := fs[name]
		if !ok && !strings.HasPrefix(a, "--") && len(a) > 2 {
			// Combined short booleans: -it.
			okAll := true
			for _, ch := range a[1:] {
				d, ok := fs["-"+string(ch)]
				if !ok || d.value {
					okAll = false
					break
				}
			}
			if okAll {
				for _, ch := range a[1:] {
					d := fs["-"+string(ch)]
					p[d.canon] = append(p[d.canon], "true")
				}
				continue
			}
			// Short flag with attached value: -m512M.
			if d, ok := fs[a[:2]]; ok && d.value {
				p[d.canon] = append(p[d.canon], a[2:])
				continue
			}
		}
		if !ok {
			return nil, nil, usagef("unknown flag: %s (for %q)", name, cmd)
		}
		switch {
		case !def.value && hasVal:
			p[def.canon] = append(p[def.canon], val)
		case !def.value:
			p[def.canon] = append(p[def.canon], "true")
		case hasVal:
			p[def.canon] = append(p[def.canon], val)
		default:
			if i+1 >= len(args) {
				return nil, nil, usagef("flag needs an argument: %s (for %q)", name, cmd)
			}
			i++
			p[def.canon] = append(p[def.canon], args[i])
		}
	}
	return p, pos, nil
}
