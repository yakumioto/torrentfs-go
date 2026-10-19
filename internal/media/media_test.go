package media_test

import (
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/media"
)

func TestVideoExtension(t *testing.T) {
	for _, ext := range []string{
		".3gp", ".avi", ".flv", ".m2ts", ".m4v", ".mkv", ".mov",
		".mp4", ".mpeg", ".mpg", ".mts", ".ts", ".webm", ".wmv",
	} {
		for _, spelling := range []string{ext, strings.ToUpper(ext)} {
			t.Run(spelling, func(t *testing.T) {
				got, ok := media.VideoExtension("目录/Movie" + spelling)
				if !ok || got != spelling {
					t.Fatalf("VideoExtension = %q, %v, want %q, true", got, ok, spelling)
				}
			})
		}
	}
	for _, tt := range []struct {
		name string
		ext  string
		ok   bool
	}{
		{name: "Movie.Mp4", ext: ".Mp4", ok: true},
		{name: "Disc.iso", ext: ".iso"},
		{name: "Disc.ISO", ext: ".ISO"},
		{name: "Movie.mkv.strm", ext: ".strm"},
		{name: "Movie.SRT", ext: ".SRT"},
		{name: "Movie"},
		{name: "folder.mkv/file"},
		{name: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := media.VideoExtension(tt.name)
			if got != tt.ext || ok != tt.ok {
				t.Fatalf("VideoExtension(%q) = %q, %v, want %q, %v", tt.name, got, ok, tt.ext, tt.ok)
			}
		})
	}
}

func TestIsDiscImage(t *testing.T) {
	for _, tt := range []struct {
		name string
		want bool
	}{
		{name: "Disc.iso", want: true},
		{name: "目录/Disc.IsO", want: true},
		{name: "Disc.iso.strm"},
		{name: "Disc.mkv"},
		{name: "folder.iso/file"},
		{name: "Disc"},
		{name: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := media.IsDiscImage(tt.name); got != tt.want {
				t.Fatalf("IsDiscImage(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestContentType(t *testing.T) {
	for _, tt := range []struct {
		ext  string
		want string
	}{
		{ext: ".3gp", want: "video/3gpp"},
		{ext: ".avi", want: "video/x-msvideo"},
		{ext: ".flv", want: "video/x-flv"},
		{ext: ".m2ts", want: "video/mp2t"},
		{ext: ".m4v", want: "video/x-m4v"},
		{ext: ".mkv", want: "video/x-matroska"},
		{ext: ".mov", want: "video/quicktime"},
		{ext: ".mp4", want: "video/mp4"},
		{ext: ".mpeg", want: "video/mpeg"},
		{ext: ".mpg", want: "video/mpeg"},
		{ext: ".mts", want: "video/mp2t"},
		{ext: ".ts", want: "video/mp2t"},
		{ext: ".webm", want: "video/webm"},
		{ext: ".wmv", want: "video/x-ms-wmv"},
		{ext: ".iso", want: "application/x-iso9660-image"},
		{ext: ".txt", want: "application/octet-stream"},
		{ext: "", want: "application/octet-stream"},
	} {
		for _, ext := range []string{tt.ext, strings.ToUpper(tt.ext)} {
			t.Run(ext, func(t *testing.T) {
				if got := media.ContentType("nonexistent/目录/Movie" + ext); got != tt.want {
					t.Fatalf("ContentType(%q) = %q, want %q", ext, got, tt.want)
				}
			})
		}
	}
}
