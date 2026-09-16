package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dist/*
var embeddedFiles embed.FS

// Handler serves the embedded Web UI.
func Handler() http.Handler {
	assets, err := fs.Sub(embeddedFiles, "dist")
	if err != nil {
		panic(err)
	}
	return newStaticHandler(assets)
}
