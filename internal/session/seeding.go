package session

import "github.com/anacrolix/torrent"

func configureSeeding(cfg *torrent.ClientConfig) {
	cfg.Seed = true
	cfg.NoUpload = false
}
