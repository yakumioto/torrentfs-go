package config

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

var (
	errSTRMURL      = errors.New("must be an absolute HTTP(S) URL without credentials, query, or fragment")
	errSTRMHost     = errors.New("must include a valid hostname")
	errSTRMPort     = errors.New("URL port must be between 1 and 65535")
	errSTRMPrefix   = errors.New("must use an unambiguous path prefix")
	errSTRMListener = errors.New("requires a non-empty http.listen_addr")
)

func validateSTRM(cfg STRM, listenAddr string) error {
	if cfg.BaseURL == "" {
		return nil
	}
	// Never return the parser's error: it can contain URL credentials.
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return errSTRMURL
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.BaseURL, "#") {
		return errSTRMURL
	}
	host := u.Hostname()
	if host == "" {
		return errSTRMHost
	}
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() {
			return errSTRMHost
		}
	} else if strings.ContainsAny(host, ":[]") {
		return errSTRMHost
	}
	if strings.HasSuffix(u.Host, ":") {
		return errSTRMPort
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return errSTRMPort
		}
	}
	// A single trailing slash is harmless. Empty interior components and
	// encoded separators would make a reverse proxy's routing ambiguous.
	prefix := strings.TrimSuffix(u.EscapedPath(), "/")
	if prefix != "" {
		if !strings.HasPrefix(prefix, "/") {
			return errSTRMPrefix
		}
		for _, escaped := range strings.Split(prefix[1:], "/") {
			component, err := url.PathUnescape(escaped)
			if err != nil || component == "" || component == "." || component == ".." ||
				strings.ContainsAny(component, "/\\\x00") {
				return errSTRMPrefix
			}
		}
	}
	if listenAddr == "" {
		return errSTRMListener
	}
	return nil
}
