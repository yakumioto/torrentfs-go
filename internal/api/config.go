package api

import (
	"net/http"

	"github.com/yakumioto/torrentfs-go/internal/configctl"
)

// ConfigViewer is the configuration surface the management API serves. The
// controller lives outside the API package, so the server can be driven by
// the real controller and by a test double.
type ConfigViewer interface {
	// View renders the read-only configuration view.
	View() (configctl.View, error)
}

// WithConfigViewer serves the configuration view from viewer.
//
// The configuration route is registered unconditionally and sits inside the
// authentication middleware like every other management route. A server
// without a viewer reports the missing wiring instead of a plausible empty
// page.
func WithConfigViewer(viewer ConfigViewer) Option {
	return func(options *serverOptions) {
		options.configViewer = viewer
	}
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	if s.configViewer == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration view unavailable")
		return
	}
	view, err := s.configViewer.View()
	if err != nil {
		s.logger.Error("configuration view failed", "err", err)
		writeError(w, http.StatusInternalServerError, "configuration view failed")
		return
	}
	writeJSON(w, http.StatusOK, view)
}
