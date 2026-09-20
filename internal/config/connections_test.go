package config_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

func TestDefaultDisablesPortForwarding(t *testing.T) {
	cfg := config.Default()
	if !cfg.Connections.NoPortForwarding {
		t.Fatal("Default no_port_forwarding = false, want true")
	}
	if cfg.Connections.DisableIPv4 || cfg.Connections.DisableIPv6 {
		t.Fatalf("Default disables an address family: %+v", cfg.Connections)
	}
	if len(cfg.Connections.BootstrapNodes) != 0 {
		t.Fatalf("Default bootstrap nodes = %v, want none", cfg.Connections.BootstrapNodes)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default config is invalid: %v", err)
	}
}

func TestLoadReadsConnectionSection(t *testing.T) {
	path := writeConfig(t, `[paths]
data_dir = `+quote(filepath.Join(t.TempDir(), "data"))+`

[connections]
listen_host = "127.0.0.1"
listen_port = 6881
disable_ipv4 = false
disable_ipv6 = true
no_port_forwarding = false
bootstrap_nodes = ["router.example:6881", "192.0.2.10:6881"]
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := config.Connections{
		ListenHost:       "127.0.0.1",
		ListenPort:       6881,
		DisableIPv6:      true,
		NoPortForwarding: false,
		BootstrapNodes:   []string{"router.example:6881", "192.0.2.10:6881"},
	}
	if !reflect.DeepEqual(cfg.Connections, want) {
		t.Fatalf("Connections = %+v, want %+v", cfg.Connections, want)
	}
}

func TestLoadAppliesConnectionEnvironmentBindings(t *testing.T) {
	t.Setenv("TORRENTFS_CONNECTIONS_DISABLE_IPV4", "false")
	t.Setenv("TORRENTFS_CONNECTIONS_DISABLE_IPV6", "true")
	t.Setenv("TORRENTFS_CONNECTIONS_NO_PORT_FORWARDING", "false")
	t.Setenv("TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES", "router.example:6881, 192.0.2.10:6881 ,")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Connections.DisableIPv4 || !cfg.Connections.DisableIPv6 {
		t.Fatalf("address family flags = ipv4 %t ipv6 %t, want false/true", cfg.Connections.DisableIPv4, cfg.Connections.DisableIPv6)
	}
	if cfg.Connections.NoPortForwarding {
		t.Fatal("no_port_forwarding = true, want the environment value false")
	}
	want := []string{"router.example:6881", "192.0.2.10:6881"}
	if !reflect.DeepEqual(cfg.Connections.BootstrapNodes, want) {
		t.Fatalf("bootstrap nodes = %v, want %v", cfg.Connections.BootstrapNodes, want)
	}
}

// TestShippedConfigurationsDecode guards the strict-decoder contract: a key
// that exists in the Config struct but not in the shipped files, or the other
// way around, breaks startup for anyone copying one of them.
func TestShippedConfigurationsDecode(t *testing.T) {
	for _, name := range []string{"torrentfs.example.toml", "docker/torrentfs.toml"} {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Load(filepath.Join("..", "..", name)); err != nil {
				t.Fatalf("Load(%s): %v", name, err)
			}
		})
	}
}

func TestValidateRejectsInvalidConnections(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*config.Config)
		field string
	}{
		{
			name: "both address families disabled",
			setup: func(cfg *config.Config) {
				cfg.Connections.DisableIPv4 = true
				cfg.Connections.DisableIPv6 = true
			},
			field: "connections.disable_ipv4",
		},
		{
			name: "bootstrap node without a port",
			setup: func(cfg *config.Config) {
				cfg.Connections.BootstrapNodes = []string{"router.example"}
			},
			field: "connections.bootstrap_nodes",
		},
		{
			name: "bootstrap node with port zero",
			setup: func(cfg *config.Config) {
				cfg.Connections.BootstrapNodes = []string{"router.example:0"}
			},
			field: "connections.bootstrap_nodes",
		},
		{
			name: "bootstrap node with a port out of range",
			setup: func(cfg *config.Config) {
				cfg.Connections.BootstrapNodes = []string{"router.example:65536"}
			},
			field: "connections.bootstrap_nodes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Paths.DataDir = filepath.Join(t.TempDir(), "data")
			tt.setup(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", cfg.Connections)
			}
			var invalid *config.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("Validate error = %v, want a ValidationError", err)
			}
			if invalid.Field != tt.field {
				t.Fatalf("Validate field = %q, want %q", invalid.Field, tt.field)
			}
		})
	}
}
