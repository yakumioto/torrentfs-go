package stream_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/stream"
)

func TestValidatePath(t *testing.T) {
	for _, path := range []string{
		"Movie.mkv", "目录/视频 100%#?+.mkv", ".hidden/Movie.mkv",
		"a%2Fb.mkv", "%2e%2e/file.mkv", "literal%zz.mkv",
	} {
		t.Run(path, func(t *testing.T) {
			if err := stream.ValidatePath(path); err != nil {
				t.Fatalf("ValidatePath: %v", err)
			}
		})
	}
	for _, path := range []string{
		"", "/Movie.mkv", "Movie.mkv/", "dir//Movie.mkv", ".", "..",
		"./Movie.mkv", "dir/../Movie.mkv", "dir/./Movie.mkv",
		"dir/a\x00b.mkv", "dir\\Movie.mkv",
	} {
		t.Run(path, func(t *testing.T) {
			if err := stream.ValidatePath(path); !errors.Is(err, stream.ErrMalformed) {
				t.Fatalf("ValidatePath error = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestURLSpecialCharacterRoundTrip(t *testing.T) {
	signer := testSigner(t)
	hash := testHash()
	for _, path := range []string{
		"Movie.mkv", "目录/视频 名称%#?+.mkv", "Disc.ISO",
		"literal%2F.mkv", "%2e%2e/file.mkv", "literal%zz.mkv",
	} {
		t.Run(path, func(t *testing.T) {
			signature := signer.Sign(hash, path)
			raw, err := stream.BuildURL("https://media.example/", hash, signature, path)
			if err != nil {
				t.Fatalf("BuildURL: %v", err)
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("url.Parse: %v", err)
			}
			if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
				t.Fatalf("payload path became query or fragment: %q", raw)
			}
			got, err := stream.ParsePath(u.EscapedPath())
			if err != nil {
				t.Fatalf("ParsePath: %v", err)
			}
			want := stream.Location{Hash: hash, Signature: signature, Path: path}
			if got != want {
				t.Fatalf("ParsePath = %+v, want %+v", got, want)
			}
			if !signer.Verify(got.Hash, got.Path, got.Signature) {
				t.Fatal("round-tripped location no longer verifies")
			}
		})
	}
}

func TestBuildURLPrefixesAndEscaping(t *testing.T) {
	signer := testSigner(t)
	hash := testHash()
	const path = "目录/Movie 100%#?+.mkv"
	signature := signer.Sign(hash, path)
	const escapedFile = "/%E7%9B%AE%E5%BD%95/Movie%20100%25%23%3F+.mkv"
	for _, tt := range []struct {
		name          string
		base          string
		wantBase      string
		escapedPrefix string
	}{
		{name: "no path", base: "http://media.example", wantBase: "http://media.example"},
		{name: "root slash", base: "https://media.example/", wantBase: "https://media.example"},
		{name: "prefix", base: "https://media.example/torrentfs", wantBase: "https://media.example/torrentfs", escapedPrefix: "/torrentfs"},
		{name: "prefix slash", base: "https://media.example/torrentfs/", wantBase: "https://media.example/torrentfs", escapedPrefix: "/torrentfs"},
		{name: "port", base: "http://media.example:65535/a/b/", wantBase: "http://media.example:65535/a/b", escapedPrefix: "/a/b"},
		{name: "IPv6", base: "http://[::1]:8080/", wantBase: "http://[::1]:8080"},
		{name: "unicode prefix", base: "https://media.example/前 缀/", wantBase: "https://media.example/%E5%89%8D%20%E7%BC%80", escapedPrefix: "/%E5%89%8D%20%E7%BC%80"},
		{name: "escaped prefix", base: "https://media.example/a%23b%3Fc%25d+e/", wantBase: "https://media.example/a%23b%3Fc%25d+e", escapedPrefix: "/a%23b%3Fc%25d+e"},
		{name: "literal percent prefix", base: "https://media.example/%252F/", wantBase: "https://media.example/%252F", escapedPrefix: "/%252F"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := stream.BuildURL(tt.base, hash, signature, path)
			if err != nil {
				t.Fatalf("BuildURL: %v", err)
			}
			want := tt.wantBase + "/stream/" + hash.HexString() + "/" + signature + escapedFile
			if got != want {
				t.Fatalf("BuildURL = %q, want %q", got, want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse: %v", err)
			}
			// The external prefix is stripped by the reverse proxy.
			location, err := stream.ParsePath(strings.TrimPrefix(u.EscapedPath(), tt.escapedPrefix))
			if err != nil || location.Path != path || location.Hash != hash || location.Signature != signature {
				t.Fatalf("ParsePath after prefix removal = %+v, %v", location, err)
			}
		})
	}
}

func TestBuildURLRejectsMalformedInput(t *testing.T) {
	hash := testHash()
	signature := testSigner(t).Sign(hash, "Movie.mkv")
	for _, tt := range []struct {
		name string
		base string
		path string
		sig  string
		want error
	}{
		{name: "disabled", path: "Movie.mkv", sig: signature, want: stream.ErrDisabled},
		{name: "scheme", base: "ftp://media.example", path: "Movie.mkv", sig: signature},
		{name: "opaque", base: "http:media.example", path: "Movie.mkv", sig: signature},
		{name: "relative", base: "/prefix", path: "Movie.mkv", sig: signature},
		{name: "missing hostname", base: "http://:8080", path: "Movie.mkv", sig: signature},
		{name: "invalid IPv6", base: "http://[invalid]:8080", path: "Movie.mkv", sig: signature},
		{name: "unbracketed IPv6", base: "http://::1", path: "Movie.mkv", sig: signature},
		{name: "credentials", base: "http://alice:secret@media.example", path: "Movie.mkv", sig: signature},
		{name: "credential parse error", base: "http://alice:secret@media.example/%zz", path: "Movie.mkv", sig: signature},
		{name: "query", base: "http://media.example?x=y", path: "Movie.mkv", sig: signature},
		{name: "bare query", base: "http://media.example?", path: "Movie.mkv", sig: signature},
		{name: "fragment", base: "http://media.example#x", path: "Movie.mkv", sig: signature},
		{name: "bare fragment", base: "http://media.example#", path: "Movie.mkv", sig: signature},
		{name: "zero port", base: "http://media.example:0", path: "Movie.mkv", sig: signature},
		{name: "large port", base: "http://media.example:65536", path: "Movie.mkv", sig: signature},
		{name: "empty port", base: "http://media.example:", path: "Movie.mkv", sig: signature},
		{name: "invalid port", base: "http://media.example:bad", path: "Movie.mkv", sig: signature},
		{name: "prefix empty component", base: "http://media.example/a//b", path: "Movie.mkv", sig: signature},
		{name: "prefix dot", base: "http://media.example/a/./b", path: "Movie.mkv", sig: signature},
		{name: "prefix dot dot", base: "http://media.example/a/../b", path: "Movie.mkv", sig: signature},
		{name: "prefix encoded dot", base: "http://media.example/%2e", path: "Movie.mkv", sig: signature},
		{name: "prefix encoded slash", base: "http://media.example/a%2fb", path: "Movie.mkv", sig: signature},
		{name: "prefix backslash", base: "http://media.example/a%5Cb", path: "Movie.mkv", sig: signature},
		{name: "prefix NUL", base: "http://media.example/a%00b", path: "Movie.mkv", sig: signature},
		{name: "empty path", base: "http://media.example", sig: signature},
		{name: "absolute path", base: "http://media.example", path: "/Movie.mkv", sig: signature},
		{name: "path empty component", base: "http://media.example", path: "dir//Movie.mkv", sig: signature},
		{name: "path dot", base: "http://media.example", path: "dir/./Movie.mkv", sig: signature},
		{name: "path dot dot", base: "http://media.example", path: "dir/../Movie.mkv", sig: signature},
		{name: "path NUL", base: "http://media.example", path: "Movie\x00.mkv", sig: signature},
		{name: "path backslash", base: "http://media.example", path: "dir\\Movie.mkv", sig: signature},
		{name: "empty signature", base: "http://media.example", path: "Movie.mkv"},
		{name: "padded signature", base: "http://media.example", path: "Movie.mkv", sig: signature + "="},
		{name: "noncanonical signature", base: "http://media.example", path: "Movie.mkv", sig: nonCanonicalSignature(signature)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.want
			if want == nil {
				want = stream.ErrMalformed
			}
			got, err := stream.BuildURL(tt.base, hash, tt.sig, tt.path)
			if got != "" || !errors.Is(err, want) {
				t.Fatalf("BuildURL = %q, %v, want empty URL and %v", got, err, want)
			}
			for _, secret := range []string{"alice", "secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("BuildURL error exposes credentials: %v", err)
				}
			}
		})
	}
}

func TestParsePathDecodesExactlyOnce(t *testing.T) {
	hash := testHash()
	signer := testSigner(t)
	for _, tt := range []struct {
		escaped string
		want    string
	}{
		{escaped: "literal%252F.mkv", want: "literal%2F.mkv"},
		{escaped: "%252e%252e/file.mkv", want: "%2e%2e/file.mkv"},
		{escaped: "literal%255C.mkv", want: "literal%5C.mkv"},
		{escaped: "a+b%20c%25%23%3F.mkv", want: "a+b c%#?.mkv"},
	} {
		t.Run(tt.escaped, func(t *testing.T) {
			signature := signer.Sign(hash, tt.want)
			got, err := stream.ParsePath("/stream/" + hash.HexString() + "/" + signature + "/" + tt.escaped)
			if err != nil {
				t.Fatalf("ParsePath: %v", err)
			}
			if got.Path != tt.want || !signer.Verify(got.Hash, got.Path, got.Signature) {
				t.Fatalf("ParsePath = %+v, want original path %q and valid signature", got, tt.want)
			}
		})
	}
}

func TestParsePathRejectsMalformedLocations(t *testing.T) {
	hash := testHash().HexString()
	signature := testSigner(t).Sign(testHash(), "Movie.mkv")
	prefix := "/stream/" + hash + "/" + signature + "/"
	for _, tt := range []struct {
		name string
		path string
	}{
		{name: "empty"},
		{name: "route only", path: "/stream"},
		{name: "route slash", path: "/stream/"},
		{name: "missing signature", path: "/stream/" + hash + "/Movie.mkv"},
		{name: "empty signature", path: "/stream/" + hash + "//Movie.mkv"},
		{name: "empty file", path: prefix},
		{name: "missing leading slash", path: strings.TrimPrefix(prefix, "/") + "Movie.mkv"},
		{name: "wrong route", path: "/other/" + hash + "/" + signature + "/Movie.mkv"},
		{name: "external prefix", path: "/torrentfs" + prefix + "Movie.mkv"},
		{name: "uppercase hash", path: "/stream/" + strings.ToUpper(hash) + "/" + signature + "/Movie.mkv"},
		{name: "short hash", path: "/stream/" + hash[1:] + "/" + signature + "/Movie.mkv"},
		{name: "invalid hex hash", path: "/stream/" + strings.Repeat("z", 40) + "/" + signature + "/Movie.mkv"},
		{name: "padded signature", path: "/stream/" + hash + "/" + signature + "=/Movie.mkv"},
		{name: "noncanonical signature", path: "/stream/" + hash + "/" + nonCanonicalSignature(signature) + "/Movie.mkv"},
		{name: "signature slash", path: "/stream/" + hash + "/%2f" + signature[1:] + "/Movie.mkv"},
		{name: "empty component", path: prefix + "dir//Movie.mkv"},
		{name: "trailing slash", path: prefix + "Movie.mkv/"},
		{name: "dot", path: prefix + "./Movie.mkv"},
		{name: "dot dot", path: prefix + "dir/../Movie.mkv"},
		{name: "encoded dot", path: prefix + "%2e/Movie.mkv"},
		{name: "encoded dot dot", path: prefix + "%2E%2e/Movie.mkv"},
		{name: "encoded slash", path: prefix + "dir%2FMovie.mkv"},
		{name: "encoded lowercase slash", path: prefix + "dir%2fMovie.mkv"},
		{name: "encoded root slash", path: prefix + "%2FMovie.mkv"},
		{name: "raw backslash", path: prefix + "dir\\Movie.mkv"},
		{name: "encoded backslash", path: prefix + "dir%5cMovie.mkv"},
		{name: "raw NUL", path: prefix + "Movie\x00.mkv"},
		{name: "encoded NUL", path: prefix + "Movie%00.mkv"},
		{name: "invalid escape", path: prefix + "Movie%zz.mkv"},
		{name: "incomplete escape", path: prefix + "Movie%2"},
		{name: "raw percent", path: prefix + "Movie%"},
		{name: "query string", path: prefix + "Movie.mkv?x=y"},
		{name: "fragment", path: prefix + "Movie.mkv#x"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := stream.ParsePath(tt.path)
			if got != (stream.Location{}) || !errors.Is(err, stream.ErrMalformed) {
				t.Fatalf("ParsePath = %+v, %v, want empty location and ErrMalformed", got, err)
			}
		})
	}
}
