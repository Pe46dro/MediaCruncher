package config

import (
	"os"
	"testing"
)

func TestWebSettingsListenAddr(t *testing.T) {
	tests := []struct {
		name     string
		settings WebSettings
		expected string
	}{
		{
			name: "default port and addr",
			settings: WebSettings{
				Port: 8080,
				Addr: "0.0.0.0",
			},
			expected: "0.0.0.0:8080",
		},
		{
			name: "custom port with ip addr",
			settings: WebSettings{
				Port: 9000,
				Addr: "127.0.0.1",
			},
			expected: "127.0.0.1:9000",
		},
		{
			name: "empty addr uses 0.0.0.0",
			settings: WebSettings{
				Port: 3000,
				Addr: "",
			},
			expected: "0.0.0.0:3000",
		},
		{
			name: "addr with existing port overridden by explicit port",
			settings: WebSettings{
				Port: 8888,
				Addr: "192.168.1.50:8080",
			},
			expected: "192.168.1.50:8888",
		},
		{
			name: "zero port defaults to 8080",
			settings: WebSettings{
				Port: 0,
				Addr: "0.0.0.0",
			},
			expected: "0.0.0.0:8080",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr := tc.settings.ListenAddr()
			if addr != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, addr)
			}
		})
	}
}

func TestWebPortEnvOverride(t *testing.T) {
	os.Setenv("MC_WEB_PORT", "9999")
	defer os.Unsetenv("MC_WEB_PORT")

	cfg := DefaultConfig()
	applyEnv(&cfg)

	if cfg.Web.Port != 9999 {
		t.Errorf("expected port 9999 from env, got %d", cfg.Web.Port)
	}
	if cfg.Web.ListenAddr() != "0.0.0.0:9999" {
		t.Errorf("expected listen addr 0.0.0.0:9999, got %s", cfg.Web.ListenAddr())
	}
}
