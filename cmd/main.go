package main

import (
	"fmt"
	"os"
	"path/filepath"

	"mediacruncher/internal/config"
	"mediacruncher/internal/observability"
)

func main() {
	configDir, err := configDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cfg, err := config.LoadConfig(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	logger := observability.NewStdLogger(parseLogLevel(cfg.Observability.LogLevel), "main")
	logger.Info("initializing mediacruncher")

	metrics := observability.NewRegistry(cfg.Observability.MetricsAddr)
	if err := metrics.Start(); err != nil {
		logger.WithFields(observability.Field{Key: "error", Value: err}).Error("failed to start metrics server")
	}

	health := observability.NewHealthChecker()
	health.SetModuleStatus("config", "healthy")

	_ = health

	logger.WithFields(
		observability.Field{Key: "workers", Value: cfg.Concurrency.WorkerCount},
		observability.Field{Key: "queue_capacity", Value: cfg.Concurrency.QueueCapacity},
		observability.Field{Key: "database", Value: cfg.Persistence.DatabasePath},
	).Info("mediacruncher initialized")

	logger.Info("mediacruncher shutting down")
}

func configDir() (string, error) {
	// Check for explicit config dir in MC_CONFIG_DIR env var,
	// then fall back to the directory of the executable,
	// then fall back to the current working directory.
	if v := os.Getenv("MC_CONFIG_DIR"); v != "" {
		return v, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", nil // fall through to cwd
	}
	dir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(dir, "mediacruncher.json")); err == nil {
		return dir, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", nil
	}
	return cwd, nil
}

func parseLogLevel(s string) observability.Level {
	switch s {
	case "debug":
		return observability.DebugLevel
	case "warn":
		return observability.WarnLevel
	case "error":
		return observability.ErrorLevel
	case "critical":
		return observability.CriticalLevel
	case "info", "":
		return observability.InfoLevel
	default:
		return observability.InfoLevel
	}
}
