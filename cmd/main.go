package main

import (
	"context"
	"fmt"
	"io"
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
	"mediacruncher/internal/web"
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
	if cleaned, err := db.CleanupStaleQueue(ctx); err != nil {
		logger.WithFields(observability.Field{Key: "error", Value: err}).Warn("failed to clean stale queue jobs")
	} else if cleaned > 0 {
		logger.WithFields(observability.Field{Key: "cleaned", Value: cleaned}).Info("cleaned processed jobs from queue")
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
		MaxQualityDrop:       cfg.Transcoder.MaxQualityDrop,
		DiscardOnQualityLoss: cfg.Transcoder.DiscardOnQualityLoss,
		MaxEncodingDuration:  maxEncDur,
		StagingDir:           stagingDir,
		Logger:               logger,
		VMAFSampling:         cfg.Transcoder.VMAFSampling,
		VMAFSampleSegments:   cfg.Transcoder.VMAFSampleSegments,
		VMAFSampleDuration:   cfg.Transcoder.VMAFSampleDuration,
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

	// 5. Initialize Real-Time Web Server & SSE Broker
	broker := web.NewBroker()
	stateTracker := web.NewStateTracker()
	hwAccelName := "Software (CPU)"
	if cfg.Transcoder.HardwareAcceleration {
		if prof := transEngine.GetHardwareProfile(); prof != nil {
			for _, dev := range prof.Devices {
				if dev.Healthy && dev.Acceleration != "sw" {
					hwAccelName = dev.Name
					break
				}
			}
		}
	}
	stateTracker.SetSystemInfo(hwAccelName, cfg.Transcoder.TargetCodec, cfg.Transcoder.VMAFThreshold, cfg.Transcoder.MaxQualityDrop)

	var processScan func()
	var scanMu sync.Mutex

	if cfg.Web.Enabled {
		webAddr := cfg.Web.ListenAddr()
		webServer := web.NewServer(web.Config{
			Addr:            webAddr,
			Broker:          broker,
			State:           stateTracker,
			Persistence:     db,
			TriggerScanFunc: func() { processScan() },
			Logger:          logger,
		})
		if err := webServer.Start(); err != nil {
			logger.WithFields(observability.Field{Key: "error", Value: err}).Error("failed to start web dashboard server")
		} else {
			logger.WithFields(observability.Field{Key: "addr", Value: webAddr}).Info("real-time web dashboard running")
		}
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer shutdownCancel()
			_ = webServer.Shutdown(shutdownCtx)
		}()
	}

	logger.WithFields(
		observability.Field{Key: "workers", Value: cfg.Concurrency.WorkerCount},
		observability.Field{Key: "queue_capacity", Value: cfg.Concurrency.QueueCapacity},
		observability.Field{Key: "database", Value: cfg.Persistence.DatabasePath},
		observability.Field{Key: "scopes", Value: len(fsScopes)},
		observability.Field{Key: "vmaf_threshold", Value: cfg.Transcoder.VMAFThreshold},
		observability.Field{Key: "max_quality_drop", Value: cfg.Transcoder.MaxQualityDrop},
		observability.Field{Key: "discard_on_quality_loss", Value: cfg.Transcoder.DiscardOnQualityLoss},
		observability.Field{Key: "replace_existing_file", Value: cfg.Transcoder.ReplaceExistingFile},
		observability.Field{Key: "vmaf_sampling", Value: cfg.Transcoder.VMAFSampling},
	).Info("mediacruncher daemon initialized")

	processScan = func() {
		scanMu.Lock()
		defer scanMu.Unlock()

		stateTracker.SetStatus("scanning")
		broker.Broadcast("scan_started", "info", "Scansione filesystem avviata...", nil)

		fsEngine := filesystem.New(filesystem.Config{
			Scopes: fsScopes,
		})

		disc, err := fsEngine.Scan(ctx)
		if err != nil {
			logger.WithFields(observability.Field{Key: "error", Value: err}).Error("filesystem scan failed")
			broker.Broadcast("scan_failed", "error", fmt.Sprintf("Errore scansione: %v", err), nil)
			stateTracker.SetStatus("idle")
			return
		}

		stateTracker.RecordScanCompleted()
		broker.Broadcast("scan_completed", "info", fmt.Sprintf("Scansione completata: %d file scoperti, %d accettati", disc.TotalDiscovered, disc.TotalAccepted), map[string]interface{}{
			"discovered": disc.TotalDiscovered,
			"accepted":   disc.TotalAccepted,
		})

		if disc.TotalDiscovered > 0 {
			logger.WithFields(
				observability.Field{Key: "discovered", Value: disc.TotalDiscovered},
				observability.Field{Key: "accepted", Value: disc.TotalAccepted},
			).Info("filesystem scan completed")
		}

		for _, file := range disc.Files {
			// Skip files already created as optimized outputs
			if strings.Contains(file.AbsolutePath, "_optimized") {
				continue
			}

			// 1. Persistent Deduplication via Hash check in SQLite
			if file.PartialHash != "" {
				processedRec, checkErr := db.GetProcessedMediaByHash(ctx, file.PartialHash)
				if checkErr == nil && processedRec != nil {
					// File already processed in a previous run (survives docker compose restart!)
					if processedRec.Status == "completed" || processedRec.Status == "skipped_quality" || processedRec.Status == "skipped_larger" || processedRec.Status == "ignored" {
						logger.WithFields(
							observability.Field{Key: "path", Value: file.AbsolutePath},
							observability.Field{Key: "hash", Value: file.PartialHash},
							observability.Field{Key: "status", Value: processedRec.Status},
						).Debug("video already processed with matching hash, skipping")
						continue
					}
				}
			}

			logger.WithFields(
				observability.Field{Key: "path", Value: file.AbsolutePath},
				observability.Field{Key: "size_bytes", Value: file.Size},
				observability.Field{Key: "hash", Value: file.PartialHash},
			).Info("evaluating media file")

			broker.Broadcast("file_discovered", "info", fmt.Sprintf("Valutazione file: %s", filepath.Base(file.AbsolutePath)), map[string]interface{}{
				"path": file.AbsolutePath,
				"size": file.Size,
				"hash": file.PartialHash,
			})

			queueID, err := db.Enqueue(ctx, file.AbsolutePath, persistence.PriorityNormal)
			if err != nil {
				logger.WithFields(observability.Field{Key: "error", Value: err}).Warn("failed to enqueue file")
			}
			if queueID > 0 {
				_ = db.SetProcessing(ctx, queueID, "worker-1")
			}

			decision, err := evalEngine.AnalyzeFile(ctx, file.AbsolutePath, queueID)
			if err != nil {
				logger.WithFields(observability.Field{Key: "error", Value: err}).Error("evaluation failed")
				broker.Broadcast("evaluation_failed", "error", fmt.Sprintf("Valutazione fallita per %s: %v", filepath.Base(file.AbsolutePath), err), nil)
				if queueID > 0 {
					_ = db.Fail(ctx, queueID, err.Error())
				}
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
					JobID:          jobID,
					SourcePath:     file.AbsolutePath,
					OutputPath:     outputPath,
					VMAFThreshold:  cfg.Transcoder.VMAFThreshold,
					MaxQualityDrop: cfg.Transcoder.MaxQualityDrop,
					MaxDuration:    maxEncDur,
					Preset: &transcoder.EncodingPreset{
						TargetCodec:          targetCodec,
						QualityLevel:         23,
						PresetSpeed:          cfg.Transcoder.VPreset,
						AudioCodec:           "aac",
						HardwareAcceleration: cfg.Transcoder.HardwareAcceleration,
						PreferredDevice:      cfg.Transcoder.PreferredDevice,
					},
				}

				// Register active job in real-time tracker
				stateTracker.AddActiveJob(&web.ActiveJob{
					JobID:         jobID,
					SourcePath:    file.AbsolutePath,
					FileName:      filepath.Base(file.AbsolutePath),
					TargetCodec:   targetCodec,
					Preset:        cfg.Transcoder.VPreset,
					Stage:         "transcoding",
					StartTime:     time.Now(),
					WorkerID:      "worker-1",
					EstimatedProg: 25,
				})

				broker.Broadcast("transcode_started", "info", fmt.Sprintf("Inizio transcodifica: %s -> %s", filepath.Base(file.AbsolutePath), targetCodec), map[string]interface{}{
					"job_id": jobID,
					"source": file.AbsolutePath,
					"target": targetCodec,
				})

				outcome := transEngine.Transcode(ctx, job)
				stateTracker.RemoveActiveJob(jobID)

				if outcome.Status == transcoder.TranscodeStatusCompleted {
					// Check if transcoded output is larger than original
					if outcome.OutputSize >= file.Size {
						logger.WithFields(
							observability.Field{Key: "source", Value: file.AbsolutePath},
							observability.Field{Key: "original_size", Value: file.Size},
							observability.Field{Key: "transcoded_size", Value: outcome.OutputSize},
						).Warn("transcoded file is larger than original: preserving original, output deleted to avoid wasting disk space")

						if queueID > 0 {
							_ = db.Complete(ctx, queueID, 0)
						}
						if outcome.OutputPath != "" {
							_ = os.Remove(outcome.OutputPath)
						}

						var vmafScore float64
						if outcome.Verification != nil {
							vmafScore = outcome.Verification.VMAFScore
						}

						_ = db.RecordProcessedMedia(ctx, &persistence.ProcessedMediaRecord{
							SourcePath:   file.AbsolutePath,
							FileHash:     file.PartialHash,
							FileSize:     file.Size,
							OutputSize:   file.Size,
							Status:       "skipped_larger",
							VMAFScore:    vmafScore,
							DurationMs:   outcome.Duration.Milliseconds(),
							ErrorMessage: fmt.Sprintf("output (%s) larger than original (%s)", formatBytes(outcome.OutputSize), formatBytes(file.Size)),
						})

						broker.Broadcast("file_skipped_larger", "warning", fmt.Sprintf("File originale preservato per %s: output più grande dell'originale (%s vs %s)", filepath.Base(file.AbsolutePath), formatBytes(outcome.OutputSize), formatBytes(file.Size)), map[string]interface{}{
							"source":          file.AbsolutePath,
							"original_size":   file.Size,
							"transcoded_size": outcome.OutputSize,
						})
						continue
					}

					var vmafScore float64
					if outcome.Verification != nil {
						vmafScore = outcome.Verification.VMAFScore
					}

					finalOutputPath := outcome.OutputPath
					finalOutputSize := outcome.OutputSize
					replacedOriginal := false
					if cfg.Transcoder.ReplaceExistingFile && outcome.OutputPath != "" && outcome.OutputPath != file.AbsolutePath {
						_ = os.Remove(file.AbsolutePath)
						if err := os.Rename(outcome.OutputPath, file.AbsolutePath); err == nil {
							finalOutputPath = file.AbsolutePath
							replacedOriginal = true
							logger.WithFields(
								observability.Field{Key: "original", Value: file.AbsolutePath},
								observability.Field{Key: "new_file", Value: finalOutputPath},
							).Info("original file replaced with optimized version")
						} else {
							if cpErr := copyFile(outcome.OutputPath, file.AbsolutePath); cpErr == nil {
								_ = os.Remove(outcome.OutputPath)
								finalOutputPath = file.AbsolutePath
								replacedOriginal = true
								logger.WithFields(
									observability.Field{Key: "original", Value: file.AbsolutePath},
									observability.Field{Key: "new_file", Value: finalOutputPath},
								).Info("original file replaced with optimized version via copy")
							} else {
								logger.WithFields(
									observability.Field{Key: "error", Value: err},
									observability.Field{Key: "copy_error", Value: cpErr},
								).Error("failed to replace original file with optimized version")
							}
						}
					}

					if finalOutputSize <= 0 {
						if fi, statErr := os.Stat(finalOutputPath); statErr == nil {
							finalOutputSize = fi.Size()
						}
					}

					logger.WithFields(
						observability.Field{Key: "output", Value: finalOutputPath},
						observability.Field{Key: "duration", Value: outcome.Duration.String()},
						observability.Field{Key: "vmaf_score", Value: vmafScore},
						observability.Field{Key: "replaced_original", Value: replacedOriginal},
						observability.Field{Key: "original_size", Value: file.Size},
						observability.Field{Key: "output_size", Value: finalOutputSize},
					).Info("transcoding completed successfully")

					if queueID > 0 {
						_ = db.Complete(ctx, queueID, 0)
					}

					// Persist single completed record in SQLite with hash and output_size
					activeHash := file.PartialHash
					origHash := ""
					if replacedOriginal {
						if newHash, hashErr := filesystem.ComputePartialHash(file.AbsolutePath); hashErr == nil && newHash != "" {
							origHash = file.PartialHash
							activeHash = newHash
						}
					}

					_ = db.RecordProcessedMedia(ctx, &persistence.ProcessedMediaRecord{
						SourcePath:       file.AbsolutePath,
						FileHash:         activeHash,
						OriginalHash:     origHash,
						FileSize:         file.Size,
						OutputSize:       finalOutputSize,
						Status:           "completed",
						OutputPath:       finalOutputPath,
						VMAFScore:        vmafScore,
						DurationMs:       outcome.Duration.Milliseconds(),
						ReplacedOriginal: replacedOriginal,
					})

					msg := fmt.Sprintf("Transcodifica completata con successo: %s (VMAF: %.2f)", filepath.Base(file.AbsolutePath), vmafScore)
					if replacedOriginal {
						msg = fmt.Sprintf("Transcodifica completata e file originale sostituito: %s (VMAF: %.2f)", filepath.Base(file.AbsolutePath), vmafScore)
					}

					broker.Broadcast("transcode_completed", "success", msg, map[string]interface{}{
						"source":            file.AbsolutePath,
						"output":            finalOutputPath,
						"vmaf_score":        vmafScore,
						"duration":          outcome.Duration.String(),
						"replaced_original": replacedOriginal,
					})
				} else if outcome.Status == transcoder.TranscodeStatusQualityFailed {
					var vmafScore float64
					if outcome.Verification != nil {
						vmafScore = outcome.Verification.VMAFScore
					}

					logger.WithFields(
						observability.Field{Key: "status", Value: outcome.Status},
						observability.Field{Key: "vmaf_score", Value: vmafScore},
						observability.Field{Key: "error", Value: outcome.Error},
					).Warn("transcoding rejected due to excessive quality loss: output deleted, original preserved")

					if queueID > 0 {
						_ = db.Fail(ctx, queueID, outcome.Error)
					}

					// Ensure output file is deleted if created
					if outputPath != "" {
						_ = os.Remove(outputPath)
					}

					// Persist skipped_quality status with hash so it is NEVER re-evaluated or re-transcoded again!
					_ = db.RecordProcessedMedia(ctx, &persistence.ProcessedMediaRecord{
						SourcePath:   file.AbsolutePath,
						FileHash:     file.PartialHash,
						FileSize:     file.Size,
						OutputSize:   file.Size,
						Status:       "skipped_quality",
						VMAFScore:    vmafScore,
						DurationMs:   outcome.Duration.Milliseconds(),
						ErrorMessage: outcome.Error,
					})

					broker.Broadcast("quality_rejected", "warning", fmt.Sprintf("Qualità insufficiente per %s (VMAF %.2f): originale preservato e output cancellato", filepath.Base(file.AbsolutePath), vmafScore), map[string]interface{}{
						"source":     file.AbsolutePath,
						"vmaf_score": vmafScore,
						"reason":     outcome.Error,
					})
				} else {
					logger.WithFields(
						observability.Field{Key: "status", Value: outcome.Status},
						observability.Field{Key: "error", Value: outcome.Error},
					).Error("transcoding failed")

					if queueID > 0 {
						_ = db.Fail(ctx, queueID, outcome.Error)
					}

					broker.Broadcast("transcode_failed", "error", fmt.Sprintf("Transcodifica fallita per %s: %s", filepath.Base(file.AbsolutePath), outcome.Error), map[string]interface{}{
						"source": file.AbsolutePath,
						"error":  outcome.Error,
					})
				}
			} else {
				// Decision is not transcode (e.g. copy, ignore)
				if queueID > 0 {
					_ = db.Complete(ctx, queueID, 0)
				}
				// Record as ignored so on subsequent runs we don't re-probe
				_ = db.RecordProcessedMedia(ctx, &persistence.ProcessedMediaRecord{
					SourcePath:   file.AbsolutePath,
					FileHash:     file.PartialHash,
					FileSize:     file.Size,
					OutputSize:   file.Size,
					Status:       "ignored",
					ErrorMessage: fmt.Sprintf("rule decision: %s", decision.Action),
				})
			}
		}
	}

	// Run initial scan and periodic ticker
	go func() {
		processScan()
		ticker := time.NewTicker(10 * time.Second)
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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}


