package zstack

import "testing"

func TestCandidateOffsets(t *testing.T) {
	offsets := candidateOffsets()
	if offsets[0] != 8*3600 || offsets[1] != 0 {
		t.Fatalf("China and UTC must be tried first, got %v", offsets[:2])
	}
	seen := map[int]bool{}
	for _, o := range offsets {
		if seen[o] {
			t.Fatalf("offset %d listed twice", o)
		}
		seen[o] = true
	}
	// Every real zone must lie within the 15 minutes ZStack tolerates of some
	// candidate.
	for _, zone := range []int{-12 * 3600, -9*3600 - 30*60, 5*3600 + 30*60, 5*3600 + 45*60, 12*3600 + 45*60, 14 * 3600} {
		if !seen[zone] {
			t.Errorf("zone offset %d is not a candidate", zone)
		}
	}
}
