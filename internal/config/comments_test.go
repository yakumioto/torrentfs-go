package config

import (
	"strings"
	"testing"
)

// TestReadCommentsFindsEveryKey covers the contract that the documentation of
// a configuration key is readable from the source: the comment above a field
// is the documentation a user reads.
func TestReadCommentsFindsEveryKey(t *testing.T) {
	files, err := sourceFiles()
	if err != nil {
		t.Fatalf("sourceFiles: %v", err)
	}
	reader, err := readComments(files, "Config")
	if err != nil {
		t.Fatalf("readComments: %v", err)
	}
	documentation, ok := reader.comments["cache.capacity"]
	if !ok {
		t.Fatalf("the comments do not name cache.capacity: %v", reader.comments)
	}
	if !strings.Contains(documentation, "maximum number of bytes") {
		t.Errorf("the documentation of cache.capacity is %q", documentation)
	}
}

// TestReadCommentsCoversEveryKey covers the contract that every configuration
// key has its documentation: a key without a comment would reach the user as
// a blank line in the generated example.
func TestReadCommentsCoversEveryKey(t *testing.T) {
	files, err := sourceFiles()
	if err != nil {
		t.Fatalf("sourceFiles: %v", err)
	}
	reader, err := readComments(files, "Config")
	if err != nil {
		t.Fatalf("readComments: %v", err)
	}
	for _, key := range Schema() {
		if _, ok := reader.comments[key.Path]; !ok {
			t.Errorf("the comments do not name %q", key.Path)
		}
	}
}
