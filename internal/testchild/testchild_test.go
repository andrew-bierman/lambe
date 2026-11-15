package testchild

import (
	"testing"
)

func TestBehaviorsListed(t *testing.T) {
	want := map[string]bool{
		BehaviorHang:                 true,
		BehaviorSilent:               true,
		BehaviorExit0:                true,
		BehaviorSpam:                 true,
		BehaviorIgnoreSIGTERM:        true,
		BehaviorGrandchildHoldsPipe:  true,
		BehaviorLimitError:           true,
		BehaviorStreamingThenSettled: true,
	}
	if len(Behaviors) != len(want) {
		t.Fatalf("Behaviors has %d entries, want %d", len(Behaviors), len(want))
	}
	for _, b := range Behaviors {
		if !want[b] {
			t.Errorf("unexpected behavior %q in Behaviors", b)
		}
	}
}
