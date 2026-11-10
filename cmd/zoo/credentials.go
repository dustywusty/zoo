package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/inject"
	"github.com/dusty/zoo/internal/keys"
)

// harnessCredentials resolves direct credentials on the host and gives brokered
// sessions placeholders only. A stored Codex API key takes precedence over a
// subscription auth file, which cannot currently be brokered.
func harnessCredentials(g config.Global, h config.Harness, active map[string]config.Credential, cageName string, grants []string) (env []string, files map[string]string, missing []string, err error) {
	files = map[string]string{}
	refs := map[string]string{}
	for name, item := range h.Env {
		if cred, ok := strings.CutPrefix(item, "cred:"); ok {
			c, ok := active[cred]
			if !ok || !inject.Granted(c, grants, cred) {
				return nil, nil, nil, fmt.Errorf("%s: credential %q is missing or not granted", name, cred)
			}
			value, e := keys.Resolve(c.Secret)
			if e != nil {
				return nil, nil, nil, fmt.Errorf("read credential %s: %w", cred, e)
			}
			if value == "" {
				missing = append(missing, c.Secret)
				continue
			}
			refs[name] = item
			if name == "OPENAI_API_KEY" && h.Command == "codex" {
				ph, e := inject.Placeholder(c, cred, cageName)
				if e != nil {
					return nil, nil, nil, e
				}
				b, _ := json.Marshal(map[string]string{"OPENAI_API_KEY": ph})
				files[".codex/auth.json"] = string(b)
			}
			continue
		}
		value, e := keys.Resolve(item)
		if e != nil {
			return nil, nil, nil, e
		}
		if value == "" {
			missing = append(missing, item)
		} else {
			env = append(env, name+"="+value)
		}
	}
	brokerEnv, err := inject.Env(g, active, cageName, grants, refs)
	if err != nil {
		return nil, nil, nil, err
	}
	env = append(env, brokerEnv...)
	for rel, item := range h.Files {
		if _, generated := files[rel]; generated {
			continue
		}
		value, e := keys.Resolve(item)
		if e != nil {
			return nil, nil, nil, e
		}
		if value != "" {
			files[rel] = value
		}
	}
	return env, files, missing, nil
}
