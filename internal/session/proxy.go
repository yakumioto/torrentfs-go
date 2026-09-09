package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	xproxy "golang.org/x/net/proxy"
)

var errUnsupportedUDPTracker = errors.New("SOCKS5 proxy does not support UDP trackers")

type socks5Dialer struct {
	endpoint string
	dialer   xproxy.ContextDialer
}

func filterProxyTrackers(trackers [][]string) [][]string {
	var filtered [][]string
	for _, tier := range trackers {
		kept := make([]string, 0, len(tier))
		for _, rawURL := range tier {
			u, err := url.Parse(rawURL)
			if err == nil {
				switch strings.ToLower(u.Scheme) {
				case "udp", "udp4", "udp6":
					continue
				}
			}
			kept = append(kept, rawURL)
		}
		if len(kept) > 0 {
			filtered = append(filtered, kept)
		}
	}
	return filtered
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("session: SOCKS5 proxy only supports TCP network %q", network)
	}
	conn, err := d.dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("session: dial %s through SOCKS5 proxy %q: %w", address, d.endpoint, err)
	}
	return conn, nil
}

func configureProxy(cfg *torrent.ClientConfig, rawURL string) (torrent.Dialer, error) {
	if rawURL == "" {
		return nil, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("session: parse SOCKS5 proxy %q: %w", rawURL, err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	dialer, err := xproxy.FromURL(u, nil)
	if err != nil {
		return nil, fmt.Errorf("session: create SOCKS5 proxy %q: %w", rawURL, err)
	}
	contextDialer, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("session: SOCKS5 proxy %q does not support context dialing", rawURL)
	}
	safeURL := *u
	safeURL.User = nil
	wrapped := &socks5Dialer{endpoint: safeURL.String(), dialer: contextDialer}
	cfg.DialForPeerConns = false
	cfg.DisableUTP = true
	cfg.NoDHT = true
	cfg.HTTPProxy = nil
	cfg.HTTPDialContext = wrapped.DialContext
	cfg.TrackerDialContext = wrapped.DialContext
	cfg.TrackerListenPacket = rejectUDPTracker
	cfg.MetainfoSourcesMerger = func(t *torrent.Torrent, info *metainfo.MetaInfo) error {
		spec := torrent.TorrentSpecFromMetaInfo(info)
		spec.Trackers = filterProxyTrackers(spec.Trackers)
		return t.MergeSpec(spec)
	}
	return torrent.NetworkDialer{Network: "tcp", Dialer: wrapped}, nil
}

func rejectUDPTracker(network, address string) (net.PacketConn, error) {
	return nil, fmt.Errorf("session: SOCKS5 proxy cannot use UDP tracker %s %s: %w", network, address, errUnsupportedUDPTracker)
}
