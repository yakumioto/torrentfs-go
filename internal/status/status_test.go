package status

import "testing"

func TestRenderPieceStates(t *testing.T) {
	states := []PieceState{
		{Known: true, Complete: true},
		{Known: true, Partial: true, Bytes: 17},
		{Known: true},
		{Known: true, Wanted: true},
		{Known: true, Partial: true, Bytes: 9, Wanted: false},
		{Known: true, Checking: true, Wanted: false},
		{Wanted: false},
	}

	if got, want := Render(states), "[x] [X 17] [N] [] [X 9] [] []\n"; got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
}

func TestRenderEmpty(t *testing.T) {
	if got := Render(nil); got != "" {
		t.Fatalf("Render(nil) = %q, want empty output", got)
	}
}
