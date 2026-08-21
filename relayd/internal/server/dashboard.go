package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// The dashboard is a Vite+React app; `make dashboard` builds it into webui/
// before the Go build, and the binary embeds the result. One file ships
// everything, no runtime assets, no external requests.
//
//go:embed all:webui
var webuiFS embed.FS

func dashboardHandler() http.Handler {
	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		panic("webui not embedded. Run `make dashboard` before building: " + err.Error())
	}
	return http.FileServerFS(sub)
}
