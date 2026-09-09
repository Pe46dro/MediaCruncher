package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"mediacruncher/internal/config"
	"mediacruncher/internal/evaluation"
	"mediacruncher/internal/filesystem"
	"mediacruncher/internal/observability"
	"mediacruncher/internal/persistence"
	"mediacruncher/internal/transcoder"
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
	logger.Info("initializing mediacruncher daemon")

	metrics := observability.NewRegistry(cfg.Observability.MetricsAddr)
	if err := metrics.Start(); err != nil {
		logger.WithFields(observability.Field{Key: "error", Value: err}).Error("failed to start metrics server")
	}

	health := observability.NewHealthChecker()
	health.SetModuleStatus("config", "healthy")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialize Persistence Engine
	db := persistence.New(&cfg.Persistence, logger, metrics)
	defer db.Close()
	health.SetModuleStatus("persistence", "healthy")

	if recovered, err := db.RecoverProcessing(ctx); err != nil {
		logger.WithFields(observability.Field{Key: "error", Value: err}).Warn("failed to recover processing jobs")
	} else if recovered > 0 {
		logger.WithFields(observability.Field{Key: "recovered", Value: recovered}).Info("recovered pending jobs")
	}

	// 2. Initialize Evaluation Engine
	probeTimeout, _ := time.ParseDuration(cfg.Evaluation.ProbeTimeout)
	if probeTimeout <= 0 {
		probeTimeout = 30 * time.Second
	}
	evalEngine := evaluation.New(evaluation.Config{
		ProbeTimeout: probeTimeout,
		Persistence:  db,
		Logger:       logger,
		Config:       &cfg.Evaluation,
	})
	health.SetModuleStatus("evaluation", "healthy")

	// 3. Initialize Transcoder Engine
	maxEncDur, _ := time.ParseDuration(cfg.Transcoder.MaxEncodingDuration)
	if maxEncDur <= 0 {
		maxEncDur = 2 * time.Hour
	}
	stagingDir := filepath.Join(filepath.Dir(cfg.Persistence.DatabasePath), "staging")
	_ = os.MkdirAll(stagingDir, 0o755)

	transEngine := transcoder.New(transcoder.Config{
		HardwareAcceleration: cfg.Transcoder.HardwareAcceleration,
		PreferredDevice:      cfg.Transcoder.PreferredDevice,
		VMAFThreshold:        cfg.Transcoder.VMAFThreshold,
		MaxEncodingDuration:  maxEncDur,
		StagingDir:           stagingDir,
		Logger:               logger,
	})
	health.SetModuleStatus("transcoder", "healthy")

	// 4. Convert ScanScopes
	var fsScopes []filesystem.ScanScope
	for _, s := range cfg.ScanScopes {
		root := s.RootPath
		if !filepath.IsAbs(root) {
			if abs, err := filepath.Abs(root); err == nil {
				root = abs
			}
		}
		fsScopes = append(fsScopes, filesystem.ScanScope{
			RootPath:           root,
			IncludedExtensions: s.IncludedExtensions,
			ExclusionPatterns:  s.ExclusionPatterns,
			MaxDepth:           s.MaxDepth,
			SymlinkPolicy:      filesystem.SymlinkPolicy(s.SymlinkPolicy),
		})
	}
	health.SetModuleStatus("filesystem", "healthy")

	logger.WithFields(
		observability.Field{Key: "workers", Value: cfg.Concurrency.WorkerCount},
		observability.Field{Key: "queue_capacity", Value: cfg.Concurrency.QueueCapacity},
		observability.Field{Key: "database", Value: cfg.Persistence.DatabasePath},
		observability.Field{Key: "scopes", Value: len(fsScopes)},
	).Info("mediacruncher daemon initialized")

	processedFiles := make(map[string]bool)
	var mu sync.Mutex

	processScan := func() {
		mu.Lock()
		defer mu.Unlock()

		fsEngine := filesystem.New(filesystem.Config{
			Scopes: fsScopes,
		})

		disc, err := fsEngine.Scan(ctx)
		if err != nil {
			logger.WithFields(observability.Field{Key: "error", Value: err}).Error("filesystem scan failed")
			return
		}

		if disc.TotalDiscovered > 0 {
			logger.WithFields(
				observability.Field{Key: "discovered", Value: disc.TotalDiscovered},
				observability.Field{Key: "accepted", Value: disc.TotalAccepted},
			).Info("filesystem scan completed")
		}

		for _, file := range disc.Files {
			if processedFiles[file.AbsolutePath] {
				continue
			}
			// Skip already transcoded files
			if strings.Contains(file.AbsolutePath, "_optimized") {
				processedFiles[file.AbsolutePath] = true
				continue
			}

			logger.WithFields(
				observability.Field{Key: "path", Value: file.AbsolutePath},
				observability.Field{Key: "size_bytes", Value: file.Size},
			).Info("evaluating media file")

			queueID, err := db.Enqueue(ctx, file.AbsolutePath, persistence.PriorityNormal)
			if err != nil {
				logger.WithFields(observability.Field{Key: "error", Value: err}).Warn("failed to enqueue file")
			}

			decision, err := evalEngine.AnalyzeFile(ctx, file.AbsolutePath, queueID)
			if err != nil {
				logger.WithFields(observability.Field{Key: "error", Value: err}).Error("evaluation failed")
				continue
			}

			logger.WithFields(
				observability.Field{Key: "action", Value: decision.Action},
				observability.Field{Key: "rule", Value: decision.RuleID},
			).Info("evaluation decision rendered")

			if decision.Action == evaluation.DecisionTranscode {
				logger.WithFields(observability.Field{Key: "source", Value: file.AbsolutePath}).Info("initiating transcode job")

				jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
				ext := filepath.Ext(file.AbsolutePath)
				base := strings.TrimSuffix(filepath.Base(file.AbsolutePath), ext)
				outputPath := filepath.Join(filepath.Dir(file.AbsolutePath), base+"_optimized"+ext)

				targetCodec := cfg.Transcoder.TargetCodec
				if targetCodec == "" {
					targetCodec = cfg.Transcoder.Codec
				}
				if targetCodec == "" {
					targetCodec = "h.265"
				}

				job := &transcoder.TranscodeJob{
					JobID:         jobID,
					SourcePath:    file.AbsolutePath,
					OutputPath:    outputPath,
					VMAFThreshold: cfg.Transcoder.VMAFThreshold,
					MaxDuration:   maxEncDur,
					Preset: &transcoder.EncodingPreset{
						TargetCodec:          targetCodec,
						QualityLevel:         23,
						PresetSpeed:          cfg.Transcoder.VPreset,
						AudioCodec:           "aac",
						HardwareAcceleration: cfg.Transcoder.HardwareAcceleration,
						PreferredDevice:      cfg.Transcoder.PreferredDevice,
					},
				}

				outcome := transEngine.Transcode(ctx, job)
				if outcome.Status == transcoder.TranscodeStatusCompleted {
					logger.WithFields(
						observability.Field{Key: "output", Value: outcome.OutputPath},
						observability.Field{Key: "duration", Value: outcome.Duration.String()},
					).Info("transcoding completed successfully")
					if queueID > 0 {
						_ = db.Complete(ctx, queueID, 0)
					}
					processedFiles[file.AbsolutePath] = true
					processedFiles[outputPath] = true
				} else {
					logger.WithFields(
						observability.Field{Key: "status", Value: outcome.Status},
						observability.Field{Key: "error", Value: outcome.Error},
					).Error("transcoding failed")
					if queueID > 0 {
						_ = db.Fail(ctx, queueID, outcome.Error)
					}
				}
			} else {
				processedFiles[file.AbsolutePath] = true
			}
		}
	}

	// Run initial scan and periodic ticker
	go func() {
		processScan()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				processScan()
			}
		}
	}()

	// Wait for termination signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	sig := <-sigChan

	logger.WithFields(observability.Field{Key: "signal", Value: sig.String()}).Info("shutdown signal received")
	cancel()
	logger.Info("mediacruncher daemon stopped cleanly")
}

func configDir() (string, error) {
	if v := os.Getenv("MC_CONFIG_DIR"); v != "" {
		return v, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", nil
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

