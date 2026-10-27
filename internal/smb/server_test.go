package smb

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	client "github.com/hirochachacha/go-smb2"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

func TestServerRealClientAuthenticationPaginationAndReadOnly(t *testing.T) {
	cfg := config.Default()
	cfg.Filesystem.Mode = "smb"
	cfg.SMB.Username, cfg.SMB.Password = "reader", "secret"
	source := sourceWith(strings.NewReader("hello"), 5)
	for i := range 1400 {
		hash := metainfo.Hash{byte(i), byte(i >> 8), 2}
		source.views = append(source.views, filesystem.TorrentView{Hash: hash, Name: fmt.Sprintf("%04d.txt", i), SingleFile: true, Files: []filesystem.FileView{{Path: "a", Size: 5}}})
	}
	srv, err := New(cfg, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct {
		user, password, domain string
		allow                  bool
	}{
		{"reader", "secret", "WORKGROUP", true}, {"reader", "secret", "", true},
		{"reader", "wrong", "WORKGROUP", false}, {"", "", "", false},
	} {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		dialer := &client.Dialer{Initiator: &client.NTLMInitiator{User: tc.user, Password: tc.password, Domain: tc.domain}}
		session, err := dialer.Dial(c)
		if !tc.allow {
			if err == nil {
				session.Logoff()
				t.Error("unauthorized login succeeded")
			}
			c.Close()
			continue
		}
		if err != nil {
			c.Close()
			t.Fatalf("authenticate: %v", err)
		}
		share, err := session.Mount("torrentfs")
		if err != nil {
			t.Fatal(err)
		}
		entries, err := share.ReadDir("")
		if err != nil || len(entries) != 1401 {
			t.Fatalf("paginated listing: %d %v", len(entries), err)
		}
		file, err := share.Open("电影😀.txt")
		if err != nil {
			t.Fatalf("Unicode open: %v", err)
		}
		got, err := io.ReadAll(file)
		if err != nil || string(got) != "hello" {
			t.Fatalf("read: %q %v", got, err)
		}
		if err := file.Truncate(0); err == nil {
			t.Fatal("truncate succeeded")
		}
		file.Close()
		if _, err := share.Create("new.txt"); err == nil {
			t.Fatal("create succeeded")
		}
		if err := share.Mkdir("new", 0755); err == nil {
			t.Fatal("mkdir succeeded")
		}
		if err := share.Remove("电影😀.txt"); err == nil {
			t.Fatal("delete succeeded")
		}
		if err := share.Chtimes("电影😀.txt", time.Now(), time.Now()); err == nil {
			t.Fatal("set time succeeded")
		}
		share.Umount()
		session.Logoff()
		c.Close()
	}
}
