package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
)

func TestConfigureProxyWiresTCPDialers(t *testing.T) {
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for unused proxy address: %v", err)
	}
	proxyAddress := proxyListener.Addr().String()
	if err := proxyListener.Close(); err != nil {
		t.Fatalf("close unused proxy listener: %v", err)
	}

	cfg := torrent.NewDefaultClientConfig()
	peerDialer, err := configureProxy(cfg, "socks5://user:password@"+proxyAddress)
	if err != nil {
		t.Fatalf("configureProxy: %v", err)
	}
	if peerDialer == nil {
		t.Fatal("configureProxy returned nil peer dialer")
	}
	if cfg.DialForPeerConns {
		t.Fatal("DialForPeerConns remains enabled")
	}
	if !cfg.DisableUTP || !cfg.NoDHT {
		t.Fatalf("proxy transport restrictions = DisableUTP:%v NoDHT:%v", cfg.DisableUTP, cfg.NoDHT)
	}
	if cfg.HTTPProxy != nil {
		t.Fatal("HTTPProxy is set alongside HTTPDialContext")
	}
	if cfg.HTTPDialContext == nil || cfg.TrackerDialContext == nil {
		t.Fatal("HTTP and tracker dial contexts are not configured")
	}
	if cfg.TrackerListenPacket == nil {
		t.Fatal("UDP tracker rejection is not configured")
	}
	if peerDialer.DialerNetwork() != "tcp" {
		t.Fatalf("peer dialer network = %q, want tcp", peerDialer.DialerNetwork())
	}
	if _, err := cfg.TrackerListenPacket("udp", "tracker.example:80"); !errors.Is(err, errUnsupportedUDPTracker) {
		t.Fatalf("TrackerListenPacket error = %v, want unsupported UDP tracker", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := peerDialer.Dial(ctx, "peer.example:6881"); err == nil {
		t.Fatal("peer dial unexpectedly succeeded without a proxy listener")
	}
}

func TestFilterProxyTrackers(t *testing.T) {
	trackers := [][]string{
		{"udp://udp.example:6969", "https://http.example/announce"},
		{"udp4://udp4.example:6969"},
		{"udp6://udp6.example:6969", "ws://websocket.example/announce"},
	}
	want := [][]string{
		{"https://http.example/announce"},
		{"ws://websocket.example/announce"},
	}
	if got := filterProxyTrackers(trackers); !reflect.DeepEqual(got, want) {
		t.Fatalf("filterProxyTrackers = %#v, want %#v", got, want)
	}
}

func TestConfigureProxyDialPathsUseLocalSOCKS5(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen SOCKS5 fixture: %v", err)
	}
	defer func() { _ = listener.Close() }()

	const calls = 3
	targets := make(chan string, calls)
	serverErr := make(chan error, 1)
	go func() {
		for i := 0; i < calls; i++ {
			conn, err := listener.Accept()
			if err != nil {
				serverErr <- err
				return
			}
			if err := serveSOCKS5(conn, targets); err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- nil
	}()

	cfg := torrent.NewDefaultClientConfig()
	peerDialer, err := configureProxy(cfg, "socks5://user:password@"+listener.Addr().String())
	if err != nil {
		t.Fatalf("configureProxy: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialers := []struct {
		name string
		dial func(context.Context) (net.Conn, error)
	}{
		{
			name: "http",
			dial: func(ctx context.Context) (net.Conn, error) {
				return cfg.HTTPDialContext(ctx, "tcp", "http.example:80")
			},
		},
		{
			name: "tracker",
			dial: func(ctx context.Context) (net.Conn, error) {
				return cfg.TrackerDialContext(ctx, "tcp", "tracker.example:443")
			},
		},
		{
			name: "peer",
			dial: func(ctx context.Context) (net.Conn, error) {
				return peerDialer.Dial(ctx, "peer.example:6881")
			},
		},
	}
	for _, tt := range dialers {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := tt.dial(ctx)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			if err := conn.Close(); err != nil {
				t.Fatalf("close dialed connection: %v", err)
			}
		})
	}

	wantTargets := []string{"http.example:80", "tracker.example:443", "peer.example:6881"}
	for _, want := range wantTargets {
		select {
		case got := <-targets:
			if got != want {
				t.Fatalf("SOCKS5 target = %q, want %q", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for SOCKS5 target %q", want)
		}
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("SOCKS5 fixture: %v", err)
	}
}

func serveSOCKS5(conn net.Conn, targets chan<- string) error {
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	if header[0] != 5 {
		return fmt.Errorf("SOCKS version = %d, want 5", header[0])
	}
	methods := make([]byte, header[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	if err := writeAll(conn, []byte{5, 2}); err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	if header[0] != 1 || header[1] == 0 {
		return fmt.Errorf("invalid username/password request: %v", header)
	}
	user := make([]byte, header[1])
	if _, err := io.ReadFull(conn, user); err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, header[:1]); err != nil {
		return err
	}
	password := make([]byte, header[0])
	if _, err := io.ReadFull(conn, password); err != nil {
		return err
	}
	if string(user) != "user" || string(password) != "password" {
		return fmt.Errorf("credentials = %q/%q", user, password)
	}
	if err := writeAll(conn, []byte{1, 0}); err != nil {
		return err
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return err
	}
	if request[0] != 5 || request[1] != 1 || request[2] != 0 {
		return fmt.Errorf("invalid connect request: %v", request)
	}
	var host string
	switch request[3] {
	case 1:
		address := make([]byte, 4)
		if _, err := io.ReadFull(conn, address); err != nil {
			return err
		}
		host = net.IP(address).String()
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return err
		}
		address := make([]byte, size[0])
		if _, err := io.ReadFull(conn, address); err != nil {
			return err
		}
		host = string(address)
	case 4:
		address := make([]byte, 16)
		if _, err := io.ReadFull(conn, address); err != nil {
			return err
		}
		host = net.IP(address).String()
	default:
		return fmt.Errorf("unknown address type: %d", request[3])
	}
	var port [2]byte
	if _, err := io.ReadFull(conn, port[:]); err != nil {
		return err
	}
	targets <- net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1])))
	return writeAll(conn, []byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
