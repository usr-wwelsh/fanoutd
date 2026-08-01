package server

import (
	"net/http"
	"strings"
)

// previewPrefix roots every workspace served as a site. The path below it is
// the task id, so one server hosts every workspace at once without the caller
// picking a port.
const previewPrefix = "/preview/"

// handlePreview serves a task's workspace as a static site. The /raw endpoint
// names its file in a query parameter, which gives a page's relative links
// nothing to resolve against: index.html loads, its script and stylesheet
// 404. Here the file is in the path, so ./app.js under /preview/<id>/ lands in
// the same workspace - what `python -m http.server` in that directory does,
// without leaving the board to find the directory.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, _, hasTail := strings.Cut(strings.TrimPrefix(r.URL.Path, previewPrefix), "/")
	if id == "" {
		http.Error(w, "task id is required", http.StatusBadRequest)
		return
	}
	if !hasTail {
		// A relative link resolves against the directory of the current URL, so
		// without the trailing slash every asset would be looked for a level up,
		// outside the workspace. Redirect rather than serve the wrong base.
		http.Redirect(w, r, previewPrefix+id+"/", http.StatusMovedPermanently)
		return
	}
	ws, err := s.loop.Workspace(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// http.Dir keeps the request inside the root, and FileServer gives a
	// directory its index.html or a listing - the two behaviours that make a
	// deliverable openable whether or not it names its entry point.
	fileServer := http.StripPrefix(previewPrefix+id, http.FileServer(http.Dir(ws.Root())))
	fileServer.ServeHTTP(w, r)
}
