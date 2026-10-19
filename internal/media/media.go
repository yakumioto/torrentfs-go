// Package media classifies torrent payload names without reading their content.
package media

import (
	"path"
	"strings"
)

var videoContentTypes = map[string]string{
	".3gp":  "video/3gpp",
	".avi":  "video/x-msvideo",
	".flv":  "video/x-flv",
	".m2ts": "video/mp2t",
	".m4v":  "video/x-m4v",
	".mkv":  "video/x-matroska",
	".mov":  "video/quicktime",
	".mp4":  "video/mp4",
	".mpeg": "video/mpeg",
	".mpg":  "video/mpeg",
	".mts":  "video/mp2t",
	".ts":   "video/mp2t",
	".webm": "video/webm",
	".wmv":  "video/x-ms-wmv",
}

// VideoExtension reports whether name has a supported video extension. Matching
// is case-insensitive; the returned extension retains its original spelling.
func VideoExtension(name string) (string, bool) {
	ext := path.Ext(name)
	_, ok := videoContentTypes[strings.ToLower(ext)]
	return ext, ok
}

// IsDiscImage reports whether name has a case-insensitive .iso extension.
func IsDiscImage(name string) bool {
	return strings.EqualFold(path.Ext(name), ".iso")
}

// ContentType returns an explicit media type based only on the final extension.
// Unknown names use application/octet-stream rather than content sniffing.
func ContentType(name string) string {
	if contentType, ok := videoContentTypes[strings.ToLower(path.Ext(name))]; ok {
		return contentType
	}
	if IsDiscImage(name) {
		return "application/x-iso9660-image"
	}
	return "application/octet-stream"
}
