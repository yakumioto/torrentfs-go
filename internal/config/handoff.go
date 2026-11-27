package config

import (
	"errors"
	"net/url"
	"strings"
)

const handoffTransmissionPath = "handoff.transmission"

var (
	errHandoffURL      = errors.New("must be an http or https URL with a host")
	errHandoffPassword = errors.New("must be a single line without null bytes")
)

// Handoff groups the settings of handing a torrent off to an external
// downloader: the external client takes over downloading and seeding, and the
// local torrent is deleted once the handoff succeeds.
type Handoff struct {
	// Transmission groups the settings of the Transmission handoff target.
	Transmission HandoffTransmission `toml:"transmission"`
}

// HandoffTransmission describes the Transmission instance that receives a
// handed-off torrent.
//
// The handoff feature has no separate switch: an empty URL means it is off,
// and the management interface disables its entry points. The RPC endpoint of
// Transmission 3.00 and later is the full path ending in /transmission/rpc.
type HandoffTransmission struct {
	// URL is the Transmission RPC endpoint, for example
	// http://127.0.0.1:9091/transmission/rpc. An empty value disables the
	// handoff feature. The endpoint is applied without a restart.
	URL string `toml:"url" component:"handoff" hotapply:"true"`
	// Username is the HTTP basic authentication user name of the RPC
	// endpoint. Transmission without authentication leaves it empty.
	Username string `toml:"username" component:"handoff" hotapply:"true"`
	// Password is the HTTP basic authentication password of the RPC
	// endpoint. Transmission without authentication leaves it empty.
	Password string `toml:"password" secret:"true" component:"handoff" hotapply:"true"`
}

// Configured reports whether a handoff target is configured. The URL is the
// switch: an empty one leaves every handoff entry point disabled.
func (h HandoffTransmission) Configured() bool {
	return strings.TrimSpace(h.URL) != ""
}

// ValidateShape checks the handoff target values that can never work. The
// shape is checked even while the feature is off, so a mistyped endpoint is
// reported instead of staying hidden until the first handoff.
func (h HandoffTransmission) ValidateShape() error {
	if h.Configured() {
		parsed, err := url.Parse(strings.TrimSpace(h.URL))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return invalid(handoffTransmissionPath+".url", errHandoffURL)
		}
	}
	if strings.ContainsAny(h.Password, "\r\n\x00") {
		return invalid(handoffTransmissionPath+".password", errHandoffPassword)
	}
	return nil
}
