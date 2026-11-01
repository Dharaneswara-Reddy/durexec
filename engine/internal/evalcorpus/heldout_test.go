package evalcorpus

import "testing"

func TestHeldOutIsBalanced(t *testing.T) {
	pairs, err := HeldOut()
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 300 {
		t.Fatalf("held-out corpus has %d pairs, want 300", len(pairs))
	}
	dup, hard := 0, 0
	for _, p := range pairs {
		if p.Truth == Duplicate {
			dup++
		}
		if p.Hard {
			hard++
		}
	}
	if dup != 150 {
		t.Fatalf("%d duplicates, want 150", dup)
	}
	t.Logf("held-out: 300 pairs, 150 duplicate, %d hard negatives", hard)
}
