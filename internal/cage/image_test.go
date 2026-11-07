package cage

import (
	"testing"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/config"
)

func TestPlanCustomImage(t *testing.T) {
	g := config.Defaults()
	pl, err := NewPlan(t.TempDir(), g, config.Project{}, "")
	if err != nil {
		t.Fatal(err)
	}
	def := pl.Spec(g, "", nil)
	if def.Image != g.CageImage || def.Labels[LabelImage] != g.CageImage {
		t.Fatalf("default image: %s %s", def.Image, def.Labels[LabelImage])
	}
	pl.Image = "zoo-cage-custom:0123456789ab"
	custom := pl.Spec(g, "", nil)
	if custom.Image != pl.Image || custom.Labels[LabelImage] != pl.Image {
		t.Fatalf("custom image: %s %s", custom.Image, custom.Labels[LabelImage])
	}
	// A cage created from the default image is recreated (when stopped)
	// once the repo has its own, and again when that changes.
	if Changed(backend.Status{Labels: def.Labels}, custom) != "image" {
		t.Error("switch to a custom image not noticed")
	}
	pl.Image = "zoo-cage-custom:ba9876543210"
	if Changed(backend.Status{Labels: custom.Labels}, pl.Spec(g, "", nil)) != "image" {
		t.Error("changed custom image not noticed")
	}
}
