package cache

// ReadRequest describes a file-local read and its position in the torrent.
type ReadRequest struct {
	FileOffset    int64
	Length        int64
	FileStart     int64
	FileSize      int64
	PieceLength   int64
	TorrentLength int64
}

// PieceSpan describes the part of one piece needed by a read.
type PieceSpan struct {
	Index  int
	Offset int64
	Length int64
}

// Priority is the transient priority assigned to a read plan.
type Priority uint8

const (
	PriorityNone Priority = iota
	PriorityNormal
)

// ReadPlan contains the pieces and transient priority for a read request.
type ReadPlan struct {
	Spans     []PieceSpan
	Wanted    []int
	Priority  Priority
	Readahead int64
}

// Plan maps a file-local request to global piece spans.
func Plan(req ReadRequest) ReadPlan {
	if req.FileOffset < 0 || req.Length <= 0 || req.FileSize <= req.FileOffset || req.PieceLength <= 0 {
		return ReadPlan{}
	}
	length := req.Length
	if available := req.FileSize - req.FileOffset; length > available {
		length = available
	}
	start := req.FileStart + req.FileOffset
	if start < req.FileStart || start < 0 {
		return ReadPlan{}
	}
	if req.TorrentLength > 0 {
		if start >= req.TorrentLength {
			return ReadPlan{}
		}
		if available := req.TorrentLength - start; length > available {
			length = available
		}
	}
	if length <= 0 {
		return ReadPlan{}
	}
	end := start + length
	if end < start {
		return ReadPlan{}
	}

	first := start / req.PieceLength
	last := (end-1)/req.PieceLength + 1
	plan := ReadPlan{
		Spans:     make([]PieceSpan, 0, last-first),
		Wanted:    make([]int, 0, last-first),
		Priority:  PriorityNormal,
		Readahead: req.PieceLength,
	}
	for index := first; index < last; index++ {
		pieceStart := index * req.PieceLength
		spanStart := start
		if spanStart < pieceStart {
			spanStart = pieceStart
		}
		pieceEnd := pieceStart + req.PieceLength
		spanEnd := end
		if spanEnd > pieceEnd {
			spanEnd = pieceEnd
		}
		plan.Spans = append(plan.Spans, PieceSpan{
			Index:  int(index),
			Offset: spanStart - pieceStart,
			Length: spanEnd - spanStart,
		})
		plan.Wanted = append(plan.Wanted, int(index))
	}
	return plan
}
