package config

import (
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestLoadSTRM(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{name: "omitted"},
		{name: "empty section", body: "[strm]\n"},
		{name: "disabled", body: "[strm]\nbase_url = \"\"\n"},
		{name: "enabled", body: "[strm]\nbase_url = \"http://media.example:8080\"\n", want: "http://media.example:8080"},
		{name: "prefix", body: "[strm]\nbase_url = \"https://media.example/torrentfs/\"\n", want: "https://media.example/torrentfs/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := load(writeConfigFile(t, tt.body), lookupEnvironment(nil))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.STRM.BaseURL != tt.want {
				t.Fatalf("STRM.BaseURL = %q, want %q", cfg.STRM.BaseURL, tt.want)
			}
		})
	}
}

func TestFileConfigPreservesSTRM(t *testing.T) {
	cfg := Default()
	cfg.STRM.BaseURL = "https://media.example/torrentfs/"
	if got := newFileConfig(cfg).runtimeConfig(); !reflect.DeepEqual(got, cfg) {
		t.Fatalf("file config round trip = %+v, want %+v", got, cfg)
	}
}

func TestLoadSTRMEnvironmentPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name   string
		file   string
		env    string
		listen string
	}{
		{name: "override", file: "https://file.example/prefix/", env: "https://env.example/other/", listen: defaultHTTPListenAddr},
		{name: "replace invalid", file: "not a URL", env: "http://env.example:8080", listen: defaultHTTPListenAddr},
		{name: "explicit empty disables", file: "https://file.example", listen: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, "[strm]\nbase_url = "+strconv.Quote(tt.file)+"\n")
			cfg, err := load(path, lookupEnvironment(map[string]string{
				"TORRENTFS_STRM_BASE_URL":    tt.env,
				"TORRENTFS_HTTP_LISTEN_ADDR": tt.listen,
			}))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.STRM.BaseURL != tt.env {
				t.Fatalf("STRM.BaseURL = %q, want %q", cfg.STRM.BaseURL, tt.env)
			}
		})
	}
}

func TestValidateSTRMURLs(t *testing.T) {
	for _, raw := range []string{
		"",
		"http://media.example",
		"https://media.example/",
		"HTTP://media.example:1",
		"https://media.example:65535/torrentfs/",
		"http://127.0.0.1:8080",
		"http://localhost:8080",
		"http://[::1]:8080/torrentfs",
		"http://[fe80::1%25eth0]:8080",
		"https://media.example/目录/空 格/",
		"https://media.example/%E7%9B%AE%E5%BD%95/a%20b/",
		"https://media.example/a%23b%3Fc%25d+e/",
		"https://media.example/%252F/",
	} {
		t.Run(raw, func(t *testing.T) {
			cfg := Default()
			cfg.STRM.BaseURL = raw
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate(%q): %v", raw, err)
			}
		})
	}
}

func TestLoadRejectsInvalidSTRMURLs(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
	}{
		{name: "scheme", raw: "ftp://media.example"},
		{name: "missing scheme", raw: "media.example"},
		{name: "relative", raw: "/torrentfs"},
		{name: "scheme relative", raw: "//media.example/torrentfs"},
		{name: "opaque", raw: "http:media.example/torrentfs"},
		{name: "missing host", raw: "http://"},
		{name: "missing hostname", raw: "http://:8080"},
		{name: "invalid IPv6", raw: "http://[invalid]:8080"},
		{name: "unbracketed IPv6", raw: "http://::1"},
		{name: "invalid bracket", raw: "http://media[example]"},
		{name: "username", raw: "http://alice@media.example"},
		{name: "password", raw: "https://alice:secret@media.example"},
		{name: "empty userinfo", raw: "http://@media.example"},
		{name: "query", raw: "https://media.example?token=x"},
		{name: "bare query", raw: "https://media.example?"},
		{name: "prefix query", raw: "https://media.example/prefix/?"},
		{name: "fragment", raw: "https://media.example#section"},
		{name: "bare fragment", raw: "https://media.example#"},
		{name: "prefix fragment", raw: "https://media.example/prefix/#"},
		{name: "zero port", raw: "http://media.example:0"},
		{name: "large port", raw: "http://media.example:65536"},
		{name: "negative port", raw: "http://media.example:-1"},
		{name: "non-numeric port", raw: "http://media.example:bad"},
		{name: "empty port", raw: "http://media.example:"},
		{name: "whitespace", raw: " https://media.example"},
		{name: "invalid escape", raw: "https://media.example/%zz"},
		{name: "empty component", raw: "https://media.example/a//b"},
		{name: "double root slash", raw: "https://media.example//"},
		{name: "double trailing slash", raw: "https://media.example/prefix//"},
		{name: "dot", raw: "https://media.example/a/./b"},
		{name: "dot dot", raw: "https://media.example/a/../b"},
		{name: "encoded dot", raw: "https://media.example/%2e/"},
		{name: "encoded dot dot", raw: "https://media.example/%2E%2e/"},
		{name: "encoded slash", raw: "https://media.example/a%2Fb/"},
		{name: "encoded lowercase slash", raw: "https://media.example/a%2fb"},
		{name: "backslash", raw: "https://media.example/a\\b"},
		{name: "encoded backslash", raw: "https://media.example/a%5Cb"},
		{name: "NUL", raw: "https://media.example/a%00b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, source := range []string{"toml", "environment"} {
				t.Run(source, func(t *testing.T) {
					path := ""
					values := map[string]string{}
					if source == "toml" {
						path = writeConfigFile(t, "[strm]\nbase_url = "+strconv.Quote(tt.raw)+"\n")
					} else {
						values["TORRENTFS_STRM_BASE_URL"] = tt.raw
					}
					_, err := load(path, lookupEnvironment(values))
					requireSTRMValidationError(t, err)
				})
			}
		})
	}
}

func TestValidateSTRMRequiresHTTPListener(t *testing.T) {
	cfg := Default()
	cfg.HTTP.ListenAddr = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate disabled STRM and HTTP: %v", err)
	}
	cfg.STRM.BaseURL = "https://media.example/torrentfs"
	requireSTRMValidationError(t, cfg.Validate())
}

func TestLoadSTRMErrorsDoNotExposeUserinfo(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
	}{
		{name: "userinfo", raw: "http://alice:secret@media.example"},
		{name: "invalid port", raw: "http://alice:secret@media.example:bad"},
		{name: "invalid path escape", raw: "http://alice:secret@media.example/%zz"},
		{name: "invalid user escape", raw: "http://alice:secret%zz@media.example"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := tt.raw
			for _, source := range []string{"toml", "environment"} {
				t.Run(source, func(t *testing.T) {
					path := ""
					values := map[string]string{}
					if source == "toml" {
						path = writeConfigFile(t, "[strm]\nbase_url = "+strconv.Quote(raw)+"\n")
					} else {
						values["TORRENTFS_STRM_BASE_URL"] = raw
					}
					_, err := load(path, lookupEnvironment(values))
					requireSTRMValidationError(t, err)
					for _, secret := range []string{"alice", "secret", raw} {
						if strings.Contains(err.Error(), secret) {
							t.Fatalf("load error = %q, must not contain %q", err, secret)
						}
					}
				})
			}
		})
	}
}

func TestExampleConfigsDisableSTRM(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "torrentfs.example.toml"),
		filepath.Join("..", "..", "docker", "torrentfs.toml"),
	} {
		t.Run(path, func(t *testing.T) {
			cfg, err := load(path, lookupEnvironment(nil))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.STRM.BaseURL != "" {
				t.Fatalf("STRM.BaseURL = %q, want disabled", cfg.STRM.BaseURL)
			}
		})
	}
}

func requireSTRMValidationError(t *testing.T, err error) {
	t.Helper()
	var validationErr *ValidationError
	if !errors.Is(err, ErrInvalid) || !errors.As(err, &validationErr) || validationErr.Field != "strm.base_url" {
		t.Fatalf("error = %v, want strm.base_url validation error", err)
	}
}
