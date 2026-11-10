package cage

import (
	"fmt"

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/keeper"
)

// SyncNetwork reads only approved project configuration. Agent edits to
// zoo.yaml cannot widen its network policy.
func SyncNetwork(cages []Info) (bool, error) {
	rules := []keeper.ProjectAccess{}
	for _, c := range cages {
		if !c.Running {
			continue
		}
		if c.IP == "" {
			return false, fmt.Errorf("running cage %s has no address for its network policy", c.Name)
		}
		p, err := config.LoadProject(c.Worktree)
		if err != nil {
			return false, err
		}
		rules = append(rules, keeper.ProjectAccess{IP: c.IP, Allow: p.Network.Allow})
	}
	return keeper.SyncProjects(rules)
}
