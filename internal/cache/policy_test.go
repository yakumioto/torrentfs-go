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

func TestPlanPreservesMultiGiBOffsets(t *testing.T) {
	const pieceLength = int64(1 << 20)
	const fileSize = int64(5<<30 + 3*pieceLength)

	cases := []struct {
		name       string
		offset     int64
		length     int64
		wantIndex  int
		wantOffset int64
	}{
		{name: "two-gib", offset: 2<<30 + 17, length: 31, wantIndex: 2048, wantOffset: 17},
		{name: "four-gib", offset: 4<<30 + pieceLength + 23, length: 19, wantIndex: 4097, wantOffset: 23},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Plan(ReadRequest{
				FileOffset:    tc.offset,
				Length:        tc.length,
				FileSize:      fileSize,
				PieceLength:   pieceLength,
				TorrentLength: fileSize,
			})
			want := []PieceSpan{{Index: tc.wantIndex, Offset: tc.wantOffset, Length: tc.length}}
			if !reflect.DeepEqual(got.Spans, want) {
				t.Fatalf("Spans = %+v, want %+v", got.Spans, want)
			}
		})
	}
}

func TestPlanRejectsOffsetArithmeticOverflow(t *testing.T) {
	const maxInt64 = int64(^uint64(0) >> 1)

	cases := []ReadRequest{
		{
			FileOffset:    1,
			Length:        1,
			FileStart:     maxInt64,
			FileSize:      2,
			PieceLength:   1,
			TorrentLength: maxInt64,
		},
		{
			Length:      2,
			FileSize:    2,
			FileStart:   maxInt64 - 1,
			PieceLength: 1,
		},
	}
	for i, request := range cases {
		if got := Plan(request); len(got.Spans) != 0 {
			t.Fatalf("case %d: Spans = %+v, want none", i, got.Spans)
		}
	}
}
