package cage

import (
	"os"
	"path/filepath"

	"github.com/dusty/zoo/internal/backend"
)

// KeptHome are the folders in the agent's home that survive when zoo
// recreates a cage (a new image, new mounts): the agents' chat history,
// settings and logins. Without them, every tool change lost every chat.
// Each cage has its own copy under ~/.zoo/home/<cage>, so a file that an
// agent plants there (a hook in ~/.claude/settings.json) stays in that
// cage only. zoo reset deletes the copy.
var KeptHome = []string{".claude", ".codex"}

// AgentHomeTarget is the agent's home folder in the cage.
const AgentHomeTarget = "/home/agent"

// EnsureHomeDir creates the kept folders, as you, so the agent (your uid)
// owns them.
func EnsureHomeDir(dir string) error {
	for _, d := range KeptHome {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// homeMounts mounts each kept folder over the agent's home.
func (pl Plan) homeMounts() []backend.Mount {
	if pl.HomeDir == "" {
		return nil
	}
	var out []backend.Mount
	for _, d := range KeptHome {
		out = append(out, backend.Mount{Source: filepath.Join(pl.HomeDir, d), Target: AgentHomeTarget + "/" + d})
	}
	return out
}
