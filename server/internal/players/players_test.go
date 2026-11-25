package players

import (
	"testing"

	"cribbage/internal/expert"
)

// The lobby levels must be exactly the specs measured in PLAN M43.
func TestLevelsMatchMeasuredSpecs(t *testing.T) {
	measured := map[string]string{
		"expert-easy":   "10,nokeep,nopeg,heurdiscard",
		"expert-medium": "50,nokeep,nopeg",
		expert.BotName:  "400,islambda=0.5,cond",
	}
	if len(expert.Levels) != len(measured) {
		t.Fatalf("%d levels, %d measured", len(expert.Levels), len(measured))
	}
	for _, l := range expert.Levels {
		spec, ok := measured[l.Name]
		if !ok {
			t.Fatalf("level %s was not measured", l.Name)
		}
		opt, err := ParseOptions(spec)
		if err != nil {
			t.Fatal(err)
		}
		if opt != l.Options {
			t.Errorf("%s: options %+v, measured spec %q gives %+v", l.Name, l.Options, spec, opt)
		}
	}
}

func TestSeesHidden(t *testing.T) {
	for spec, want := range map[string]bool{
		"heuristic":                          false,
		"runs/x.bin@400,cond":                false,
		"runs/x.bin@400,oracle=hand":         true,
		"runs/x.bin@oracle=six,islambda=0.5": true,
	} {
		if got := SeesHidden(spec); got != want {
			t.Errorf("SeesHidden(%q) = %v, want %v", spec, got, want)
		}
	}
}
