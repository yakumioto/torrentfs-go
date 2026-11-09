package api

import (
	"context"
	"errors"
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

// ConfigSaver is the save surface the management API serves. The controller
// lives outside the API package, so the server can be driven by the real
// controller and by a test double.
type ConfigSaver interface {
	// Save validates a sparse patch, writes it back, and applies it.
	Save(ctx context.Context, request configctl.SaveRequest) (configctl.SaveResult, error)
}

// WithConfigSaver serves PUT /api/v1/config from saver.
//
// The save route is registered unconditionally and sits inside the
// authentication middleware. A server without a saver reports the missing
// wiring instead of silently discarding a save.
func WithConfigSaver(saver ConfigSaver) Option {
	return func(options *serverOptions) {
		options.configSaver = saver
	}
}

// maxConfigSaveBodyBytes caps the save request body. A patch holds a text
// value per changed key, so the limit only stops a client from streaming an
// unbounded body into the decoder.
const maxConfigSaveBodyBytes = 1 << 20

// configSaveRequest is the wire form of a save: a sparse patch of text values
// and the file baseline the page was rendered from.
type configSaveRequest struct {
	Baseline string            `json:"baseline"`
	Patch    map[string]string `json:"patch"`
}

// configSaveResponse is the wire form of a committed save.
type configSaveResponse struct {
	Applied         []string `json:"applied"`
	RestartRequired []string `json:"restart_required"`
	Persisted       bool     `json:"persisted"`
}

// configFieldErrorResponse is one rejected configuration key.
type configFieldErrorResponse struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// configSaveErrorResponse is the wire form of a rejected save. Applied and
// RestartRequired stay in the shape of a successful save, because a durability
// failure happens after the new values are in force.
type configSaveErrorResponse struct {
	Error           string                     `json:"error"`
	Code            string                     `json:"code"`
	FieldErrors     []configFieldErrorResponse `json:"field_errors"`
	Applied         []string                   `json:"applied"`
	RestartRequired []string                   `json:"restart_required"`
	Persisted       bool                       `json:"persisted"`
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if s.configSaver == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration save unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigSaveBodyBytes)
	var body configSaveRequest
	if err := decodeJSON(r.Body, &body); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeConfigSaveError(w, http.StatusBadRequest, configSaveErrorResponse{
			Error: "配置保存请求格式无效",
			Code:  configctl.SaveInvalid,
		})
		return
	}
	result, err := s.configSaver.Save(r.Context(), configctl.SaveRequest{
		Baseline: body.Baseline,
		Patch:    body.Patch,
	})
	if err != nil {
		var saveErr *configctl.SaveError
		if !errors.As(err, &saveErr) {
			s.logger.Error("configuration save failed", "err", err)
			writeError(w, http.StatusInternalServerError, "configuration save failed")
			return
		}
		status, message := configSaveStatus(saveErr.Code)
		if status == http.StatusInternalServerError {
			s.logger.Error("configuration save failed", "code", saveErr.Code, "err", err)
		}
		response := configSaveErrorResponse{
			Error:           message,
			Code:            saveErr.Code,
			FieldErrors:     newConfigFieldErrors(saveErr.FieldErrors),
			Applied:         nonNilKeys(saveErr.Applied),
			RestartRequired: nonNilKeys(saveErr.RestartRequired),
			Persisted:       saveErr.Persisted,
		}
		writeConfigSaveError(w, status, response)
		return
	}
	writeJSON(w, http.StatusOK, configSaveResponse{
		Applied:         nonNilKeys(result.Applied),
		RestartRequired: nonNilKeys(result.RestartRequired),
		Persisted:       result.Persisted,
	})
}

// configSaveStatus maps a stable save code onto the HTTP status and the
// display message. The page branches on the code, never on the message.
func configSaveStatus(code string) (int, string) {
	switch code {
	case configctl.SaveInvalid:
		return http.StatusBadRequest, "配置校验失败，请修正标记的配置项"
	case configctl.SaveConflict:
		return http.StatusConflict, "配置文件已被外部修改，请重新读取后再保存"
	case configctl.SaveReadOnly:
		return http.StatusConflict, "配置为只读，无法保存"
	case configctl.SaveNotConfigured:
		return http.StatusConflict, "后台服务没有加载配置文件，无法保存"
	case configctl.SavePersistenceUnavailable:
		return http.StatusServiceUnavailable, "配置未能写入，文件与运行值均未改变"
	case configctl.SaveDurabilityUnconfirmed:
		return http.StatusServiceUnavailable, "新配置已生效，但无法确认重启后仍能保留"
	default:
		return http.StatusInternalServerError, "配置保存失败"
	}
}

// newConfigFieldErrors converts the controller problems to the wire form. The
// result is an empty list instead of null, so a client always reads the same
// shape.
func newConfigFieldErrors(fields []configctl.FieldError) []configFieldErrorResponse {
	converted := make([]configFieldErrorResponse, 0, len(fields))
	for _, field := range fields {
		converted = append(converted, configFieldErrorResponse{Path: field.Path, Message: field.Message})
	}
	return converted
}

// nonNilKeys returns an empty list instead of a nil slice, so a client always
// reads the same shape.
func nonNilKeys(keys []string) []string {
	if keys == nil {
		return []string{}
	}
	return keys
}

// writeConfigSaveError writes a structured save error.
func writeConfigSaveError(w http.ResponseWriter, status int, response configSaveErrorResponse) {
	if response.FieldErrors == nil {
		response.FieldErrors = []configFieldErrorResponse{}
	}
	if response.Applied == nil {
		response.Applied = []string{}
	}
	if response.RestartRequired == nil {
		response.RestartRequired = []string{}
	}
	writeJSON(w, status, response)
}
