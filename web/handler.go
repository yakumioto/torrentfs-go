package web

import (
	"bytes"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

func newStaticHandler(assets fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}

		requestPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if requestPath == "." || requestPath == "" {
			requestPath = "index.html"
		}

		filePath := requestPath
		info, err := fs.Stat(assets, filePath)
		if err != nil || info.IsDir() {
			if requestPath == "assets" || strings.HasPrefix(requestPath, "assets/") || path.Ext(requestPath) != "" {
				http.NotFound(w, r)
				return
			}
			filePath = "index.html"
			info, err = fs.Stat(assets, filePath)
			if err != nil || info.IsDir() {
				http.Error(w, "embedded index is unavailable", http.StatusInternalServerError)
				return
			}
		}

		file, err := assets.Open(filePath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = file.Close() }()

		contentType := mime.TypeByExtension(path.Ext(filePath))
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if filePath == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.HasPrefix(filePath, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}

		content, ok := file.(io.ReadSeeker)
		if !ok {
			data, err := io.ReadAll(file)
			if err != nil {
				http.Error(w, "embedded asset could not be read", http.StatusInternalServerError)
				return
			}
			content = bytes.NewReader(data)
		}
		http.ServeContent(w, r, path.Base(filePath), info.ModTime(), content)
	})
}
