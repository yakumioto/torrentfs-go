package cache

import (
	"reflect"
	"testing"
)

func TestPlanAcrossPiecesAndFileOffset(t *testing.T) {
	got := Plan(ReadRequest{
		FileOffset:    2,
		Length:        7,
		FileStart:     3,
		FileSize:      20,
		PieceLength:   4,
		TorrentLength: 20,
	})
	wantSpans := []PieceSpan{
		{Index: 1, Offset: 1, Length: 3},
		{Index: 2, Offset: 0, Length: 4},
	}
	if !reflect.DeepEqual(got.Spans, wantSpans) {
		t.Fatalf("Spans = %+v, want %+v", got.Spans, wantSpans)
	}
	if !reflect.DeepEqual(got.Wanted, []int{1, 2}) {
		t.Fatalf("Wanted = %v, want [1 2]", got.Wanted)
	}
	if got.Priority != PriorityNormal || got.Readahead != 4 {
		t.Fatalf("priority/readahead = %v/%d, want normal/4", got.Priority, got.Readahead)
	}
}

func TestPlanClampsEOFAndFinalShortPiece(t *testing.T) {
	got := Plan(ReadRequest{
		FileOffset:    1,
		Length:        20,
		FileStart:     7,
		FileSize:      30,
		PieceLength:   8,
		TorrentLength: 18,
	})
	want := []PieceSpan{
		{Index: 1, Offset: 0, Length: 8},
		{Index: 2, Offset: 0, Length: 2},
	}
	if !reflect.DeepEqual(got.Spans, want) {
		t.Fatalf("Spans = %+v, want %+v", got.Spans, want)
	}
}

func TestPlanRejectsZeroAndEOFRequests(t *testing.T) {
	cases := []ReadRequest{
		{Length: 0, FileSize: 10, PieceLength: 4},
		{FileOffset: -1, Length: 1, FileSize: 10, PieceLength: 4},
		{FileOffset: 10, Length: 1, FileSize: 10, PieceLength: 4},
		{Length: 1, FileSize: 10, PieceLength: 0},
		{Length: 1, FileStart: 20, FileSize: 10, PieceLength: 4, TorrentLength: 20},
	}
	for i, request := range cases {
		if got := Plan(request); len(got.Spans) != 0 {
			t.Fatalf("case %d: Spans = %+v, want none", i, got.Spans)
		}
	}
}
