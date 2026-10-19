package stream

import (
	"encoding/hex"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// ValidatePath checks an original slash-separated torrent-relative file path.
// Percent signs are literal here; only ParsePath decodes URL escaping.
func ValidatePath(filePath string) error {
	for _, component := range strings.Split(filePath, "/") {
		if !validComponent(component) {
			return ErrMalformed
		}
	}
	return nil
}

func validComponent(component string) bool {
	return component != "" && component != "." && component != ".." &&
		!strings.ContainsAny(component, "/\\\x00")
}

// BuildURL appends a signed stream location to an HTTP(S) base URL, preserving
// an optional reverse proxy prefix and escaping each payload path component.
func BuildURL(base string, hash metainfo.Hash, signature, filePath string) (string, error) {
	if base == "" {
		return "", ErrDisabled
	}
	u, err := parseBaseURL(base)
	if err != nil {
		return "", err
	}
	if err := ValidatePath(filePath); err != nil {
		return "", err
	}
	if _, ok := decodeSignature(signature); !ok {
		return "", ErrMalformed
	}
	components := strings.Split(filePath, "/")
	for i, component := range components {
		components[i] = url.PathEscape(component)
	}
	location := "/stream/" + hash.HexString() + "/" + signature + "/"
	escapedPrefix := strings.TrimSuffix(u.EscapedPath(), "/")
	u.Path = strings.TrimSuffix(u.Path, "/") + location + filePath
	u.RawPath = escapedPrefix + location + strings.Join(components, "/")
	return u.String(), nil
}

// ParsePath parses the HTTP handler's URL.EscapedPath, rooted at /stream.
// Reverse proxies must strip any external base URL prefix before forwarding.
// Components are decoded exactly once, so %252F names a literal %2F, not a slash.
func ParsePath(escapedPath string) (Location, error) {
	if !strings.HasPrefix(escapedPath, "/") || strings.ContainsAny(escapedPath, "?#") {
		return Location{}, ErrMalformed
	}
	components := strings.Split(escapedPath[1:], "/")
	if len(components) < 4 {
		return Location{}, ErrMalformed
	}
	for i, escaped := range components {
		component, err := url.PathUnescape(escaped)
		if err != nil || !validComponent(component) {
			return Location{}, ErrMalformed
		}
		components[i] = component
	}
	if components[0] != "stream" {
		return Location{}, ErrMalformed
	}
	var hash metainfo.Hash
	if len(components[1]) != len(hash)*2 {
		return Location{}, ErrMalformed
	}
	rawHash, err := hex.DecodeString(components[1])
	if err != nil || len(rawHash) != len(hash) {
		return Location{}, ErrMalformed
	}
	copy(hash[:], rawHash)
	if hash.HexString() != components[1] {
		return Location{}, ErrMalformed
	}
	if _, ok := decodeSignature(components[2]); !ok {
		return Location{}, ErrMalformed
	}
	return Location{
		Hash:      hash,
		Signature: components[2],
		Path:      strings.Join(components[3:], "/"),
	}, nil
}

func parseBaseURL(base string) (*url.URL, error) {
	// Parser errors may contain userinfo; only return the stable sentinel.
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(base, "#") {
		return nil, ErrMalformed
	}
	host := u.Hostname()
	if host == "" {
		return nil, ErrMalformed
	}
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() {
			return nil, ErrMalformed
		}
	} else if strings.ContainsAny(host, ":[]") {
		return nil, ErrMalformed
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, ErrMalformed
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return nil, ErrMalformed
		}
	}
	prefix := strings.TrimSuffix(u.EscapedPath(), "/")
	if prefix != "" {
		if !strings.HasPrefix(prefix, "/") {
			return nil, ErrMalformed
		}
		for _, escaped := range strings.Split(prefix[1:], "/") {
			component, err := url.PathUnescape(escaped)
			if err != nil || !validComponent(component) {
				return nil, ErrMalformed
			}
		}
	}
	return u, nil
}
