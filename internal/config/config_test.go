package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Concurrency.WorkerCount <= 0 {
		t.Fatalf("expected positive worker count, got %d", cfg.Concurrency.WorkerCount)
	}
	if cfg.Database.BusyTimeout != 5000 {
		t.Fatalf("expected default busy timeout 5000, got %d", cfg.Database.BusyTimeout)
	}
	if cfg.Filesystem.ScanInterval <= 0 {
		t.Fatalf("expected positive default scan interval, got %v", cfg.Filesystem.ScanInterval)
	}
}

func TestLoadYAMLAndEnv(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	yamlData := `
database:
  path: "test.db"
  busy_timeout: 3000
filesystem:
  scan_interval: "20s"
concurrency:
  worker_count: 8
  gpu_semaphore_limit: 3
`
	if err := os.WriteFile(configFile, []byte(yamlData), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MEDIACRUNCHER_WORKERS", "16")

	mgr, err := Load(configFile)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	cfg := mgr.Get()
	if cfg.Database.BusyTimeout != 3000 {
		t.Errorf("expected busy timeout 3000, got %d", cfg.Database.BusyTimeout)
	}
	// Env should override YAML
	if cfg.Concurrency.WorkerCount != 16 {
		t.Errorf("expected worker count 16 from env override, got %d", cfg.Concurrency.WorkerCount)
	}
	if cfg.Concurrency.GPUSemaphoreLimit != 3 {
		t.Errorf("expected GPU limit 3, got %d", cfg.Concurrency.GPUSemaphoreLimit)
	}
}

func TestNormalizePath(t *testing.T) {
	p := NormalizePath("  a/b/c/../d  ")
	expected := filepath.Clean("a/b/d")
	if p != expected {
		t.Errorf("expected %s, got %s", expected, p)
	}
}
