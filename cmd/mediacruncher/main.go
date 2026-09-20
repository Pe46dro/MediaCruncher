package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mediacruncher/internal/concurrency"
	"mediacruncher/internal/config"
	"mediacruncher/internal/evaluation"
	"mediacruncher/internal/filesystem"
	"mediacruncher/internal/notification"
	"mediacruncher/internal/observability"
	"mediacruncher/internal/persistence"
	"mediacruncher/internal/server"
	"mediacruncher/internal/shutdown"
	"mediacruncher/internal/transcoder"
)

var Version = "1.0.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "daemon":
		runDaemon(os.Args[2:])
	case "scan":
		runScan(os.Args[2:])
	case "eval":
		runEval(os.Args[2:])
	case "transcode":
		runTranscode(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "version":
		runVersion()
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`MediaCruncher - Automated Distributed Media Transcoder (v%s)

Usage:
  mediacruncher <command> [arguments]

Commands:
  daemon      Start continuous transcoding worker daemon with API & scheduler
  scan        Run one-shot filesystem scan to discover and queue media files
  eval        Probe and evaluate media file against decision rules and stream plans
  transcode   Run one-shot transcode on a file with verification and promotion
  status      Display current queue depths, audit records, and worker metrics
  version     Display application version and hardware acceleration capabilities

Run 'mediacruncher <command> -h' for more details on each command.
`, Version)
}

func runVersion() {
	fmt.Printf("MediaCruncher version %s (zero-CGO, pure Go)\n", Version)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hw := transcoder.DetectHardwareCapabilities(ctx)
	fmt.Println("\nHardware Acceleration Capabilities:")
	fmt.Printf("  NVIDIA NVENC:       %v\n", hw.HasNVENC)
	fmt.Printf("  Intel QuickSync:    %v\n", hw.HasQuickSync)
	fmt.Printf("  AMD AMF:            %v\n", hw.HasAMF)
	fmt.Printf("  Linux VAAPI:        %v\n", hw.HasVAAPI)

	fmt.Println("\nAvailable Video Encoders:")
	for enc := range hw.Encoders {
		fmt.Printf("  - %s\n", enc)
	}
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to YAML configuration file")
	_ = fs.Parse(reorderFlags(args))

	cfgMgr, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}
	cfg := cfgMgr.Get()

	// 1. Observability
	observability.InitLogger(cfg.Observability.LogLevel, cfg.Observability.LogJSON)
	slog.Info("Starting MediaCruncher Daemon", "version", Version)

	// 2. Persistence
	db, err := persistence.NewEngine(cfg.Database.Path, cfg.Database.BusyTimeout)
	if err != nil {
		slog.Error("Failed to initialize database", "err", err)
		os.Exit(1)
	}

	// 3. Evaluation & Transcoding Engines
	progressTracker := transcoder.NewProgressTracker()
	evalPipeline := evaluation.NewPipeline(cfg.Evaluation)
	tc := transcoder.NewTranscoder(cfg.Transcoder, func(prog transcoder.TranscodeProgress) {
		var qID int64
		fmt.Sscanf(prog.JobID, "job-%d", &qID)
		if qID > 0 {
			progressTracker.UpdateTranscode(qID, prog)
		}
		slog.Debug("Transcode progress",
			"job_id", prog.JobID,
			"pct", fmt.Sprintf("%.1f%%", prog.Percentage),
			"fps", prog.FPS,
			"speed", fmt.Sprintf("%.2fx", prog.Speed),
			"time", fmt.Sprintf("%.1fs/%.1fs", prog.CurrentSec, prog.TotalSec),
		)
	})
	tc.SetVMAFProgressCallback(func(jobID string, current, total int) {
		var qID int64
		fmt.Sscanf(jobID, "job-%d", &qID)
		if qID > 0 {
			progressTracker.UpdateVMAF(qID, current, total)
		}
	})

	// 4. Notification Engine
	notifEngine := notification.NewEngine(cfg.Notification, db)
	notifCtx, notifCancel := context.WithCancel(context.Background())
	notifEngine.Start(notifCtx)

	// 5. Worker Pool
	workerPool := concurrency.NewWorkerPool(
		cfg.Concurrency,
		db,
		evalPipeline,
		tc,
		func(event string, payload any) {
			notifEngine.Dispatch(event, payload)
		},
	)
	workerPool.SetProgressTracker(progressTracker)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	workerPool.Start(workerCtx)

	// 6. Filesystem Scanner Loop with Bounded Ingestion Buffer
	buf := filesystem.NewIngestionBuffer(cfg.Filesystem.IngestionCapacity)
	scanner := filesystem.NewScanner(cfg.Filesystem.Scopes, buf)

	go func() {
		for rec := range buf.Records() {
			if rec.IsDuplicate {
				continue
			}
			priority := 50
			_, err := db.Enqueue(rec.Path, priority)
			if err == nil {
				observability.GetMetrics().JobsSubmittedTotal.Add(1)
				workerPool.TriggerPrefetch()
			}
		}
	}()

	scannerCtx, scannerCancel := context.WithCancel(context.Background())
	go func() {
		slog.Info("Running initial filesystem scan")
		report, err := scanner.ScanScopes(scannerCtx)
		if err != nil {
			slog.Warn("Filesystem scan error", "err", err)
		} else {
			slog.Info("Filesystem scan completed", "accepted", report.Accepted, "duplicates", report.Duplicates)
		}
	}()

	// 7. Web UI & REST API Dashboard Server
	webServer := server.NewServer(
		cfg.Observability.MetricsPort,
		cfgMgr,
		db,
		observability.GetMetrics(),
		func() {
			slog.Info("Triggering manual filesystem scan via Web UI")
			scanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			_, _ = scanner.ScanScopes(scanCtx)
		},
	)
	webServer.SetProgressTracker(progressTracker)
	webServer.Start()

	// 8. Graceful Shutdown Coordinator
	coord := shutdown.NewCoordinator(cfg.Concurrency.DrainTimeout)

	coord.OnDrainWorkers(func(ctx context.Context) error {
		scannerCancel()
		workerPool.DrainAndStop(cfg.Concurrency.DrainTimeout)
		workerCancel()
		return nil
	})

	coord.OnPersistState(func(ctx context.Context) error {
		notifEngine.Stop()
		notifCancel()
		return nil
	})

	coord.OnTeardown(func(ctx context.Context) error {
		_ = webServer.Shutdown(ctx)
		return db.Close()
	})

	coord.OnFinalReporting(func(ctx context.Context) error {
		slog.Info("Graceful shutdown complete")
		return nil
	})

	// Wait for OS signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	slog.Info("MediaCruncher daemon running. Press Ctrl+C to terminate.")
	<-sigCh

	slog.Info("Shutdown signal received; executing 5-phase graceful teardown")
	coord.Execute()
}

func runScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to YAML configuration file")
	_ = fs.Parse(reorderFlags(args))

	cfgMgr, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}
	cfg := cfgMgr.Get()

	db, err := persistence.NewEngine(cfg.Database.Path, cfg.Database.BusyTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Println("Starting filesystem scan across configured scopes...")
	buf := filesystem.NewIngestionBuffer(cfg.Filesystem.IngestionCapacity)
	scanner := filesystem.NewScanner(cfg.Filesystem.Scopes, buf)

	discoveredCount := 0
	doneCh := make(chan struct{})
	go func() {
		for rec := range buf.Records() {
			if rec.IsDuplicate {
				continue
			}
			qID, err := db.Enqueue(rec.Path, 50)
			if err == nil && qID > 0 {
				discoveredCount++
				fmt.Printf(" [QUEUED] %s (%d bytes)\n", rec.Path, rec.Size)
			}
		}
		close(doneCh)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	summary, err := scanner.ScanScopes(ctx)
	buf.Close()
	<-doneCh

	if err != nil {
		fmt.Fprintf(os.Stderr, "Scan error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nScan Complete:\n")
	fmt.Printf("  Total files inspected: %d\n", summary.TotalDiscovered)
	fmt.Printf("  Accepted:              %d\n", summary.Accepted)
	fmt.Printf("  New files queued:      %d\n", discoveredCount)
	fmt.Printf("  Duplicates filtered:   %d\n", summary.Duplicates)
	fmt.Printf("  Errors encountered:    %d\n", summary.Errors)
	fmt.Printf("  Duration:              %s\n", summary.Duration)
}

func runEval(args []string) {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to YAML configuration file")
	_ = fs.Parse(reorderFlags(args))

	targetFile := fs.Arg(0)
	if targetFile == "" {
		fmt.Fprintf(os.Stderr, "Error: media file path is required.\nUsage: mediacruncher eval <path-to-file> [-config <config.yaml>]\n")
		os.Exit(1)
	}

	cfgMgr, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	cfg := cfgMgr.Get()

	pipeline := evaluation.NewPipeline(cfg.Evaluation)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	fmt.Printf("Analyzing media file: %s\n", targetFile)
	rec, err := pipeline.EvaluateFile(ctx, targetFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Evaluation failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n--- Normalized Metadata ---\n")
	fmt.Printf("  Container:     %s\n", rec.Metadata.Container)
	fmt.Printf("  Duration:      %.2fs\n", rec.Metadata.Duration)
	fmt.Printf("  Video Codec:   %s\n", rec.Metadata.VideoCodec)
	fmt.Printf("  Resolution:    %dx%d (%s)\n", rec.Metadata.Width, rec.Metadata.Height, rec.Metadata.ResolutionTag)
	fmt.Printf("  Bitrate:       %d bps\n", rec.Metadata.TotalBitrate)
	fmt.Printf("  HDR:           %v (%s)\n", rec.Metadata.IsHDR, rec.Metadata.HDRFormat)
	fmt.Printf("  Audio Tracks:  %d\n", len(rec.Metadata.AudioTracks))
	for _, a := range rec.Metadata.AudioTracks {
		fmt.Printf("    - Stream #0:%d: %s (%s, %s, %s)\n", a.Index, a.Codec, a.Layout, a.Language, a.Title)
	}
	fmt.Printf("  Subtitles:     %d\n", len(rec.Metadata.SubtitleTracks))
	for _, s := range rec.Metadata.SubtitleTracks {
		fmt.Printf("    - Stream #0:%d: %s (%s, %s)\n", s.Index, s.Codec, s.Language, s.Title)
	}

	fmt.Printf("\n--- Evaluation Decision ---\n")
	fmt.Printf("  Matched Rule:  %s\n", rec.Decision.MatchedRule)
	fmt.Printf("  Action:        %s\n", rec.Decision.Action)
	fmt.Printf("  Preset:        %s\n", rec.Decision.Preset)
	fmt.Printf("  Audio Action:  %s\n", rec.Decision.AudioAction)

	fmt.Printf("\n--- Synthesized FFmpeg Stream Plan ---\n")
	fmt.Printf("  Plan Summary:  %s\n", rec.StreamPlan.Summary)
	fmt.Printf("  Map Args:      %v\n", rec.StreamPlan.MapArgs)
	fmt.Printf("  Audio Args:    %v\n", rec.StreamPlan.AudioCodecArgs)
	fmt.Printf("  Subtitle Args: %v\n", rec.StreamPlan.SubtitleArgs)
}

func runTranscode(args []string) {
	fs := flag.NewFlagSet("transcode", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to YAML configuration file")
	presetFlag := fs.String("preset", "", "Preset override (e.g. balanced-hevc, efficient-av1)")
	destFlag := fs.String("dest", "", "Destination output path (default: in-place replacement)")
	vmafFlag := fs.Bool("vmaf", true, "Enable VMAF quality verification")
	_ = fs.Parse(reorderFlags(args))

	sourceFile := fs.Arg(0)
	if sourceFile == "" {
		fmt.Fprintf(os.Stderr, "Error: media file path is required.\nUsage: mediacruncher transcode <path-to-file> [-dest <output.mp4>] [-preset <name>]\n")
		os.Exit(1)
	}

	cfgMgr, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	cfg := cfgMgr.Get()
	if !*vmafFlag {
		cfg.Transcoder.VMAFEnabled = false
	}

	pipeline := evaluation.NewPipeline(cfg.Evaluation)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("Evaluating %s...\n", sourceFile)
	rec, err := pipeline.EvaluateFile(ctx, sourceFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Evaluation failed: %v\n", err)
		os.Exit(1)
	}

	presetName := rec.Decision.Preset
	if *presetFlag != "" {
		presetName = *presetFlag
	}

	tc := transcoder.NewTranscoder(cfg.Transcoder, func(prog transcoder.TranscodeProgress) {
		fmt.Printf("\rTranscoding: %.1f%% | %d frames | %.1f fps | %.2fx speed | %s",
			prog.Percentage, prog.Frames, prog.FPS, prog.Speed, prog.Bitrate)
	})

	preset := tc.SelectPreset(presetName)
	destPath := sourceFile
	if *destFlag != "" {
		destPath = *destFlag
	}

	job := &transcoder.TranscodeJob{
		ID:         fmt.Sprintf("cli-%d", time.Now().UnixNano()),
		SourcePath: sourceFile,
		DestPath:   destPath,
		Plan:       rec.StreamPlan,
		Preset:     preset,
		Duration:   rec.Metadata.Duration,
		SourceSize: rec.Metadata.FileSize,
	}

	fmt.Printf("Beginning transcode with preset '%s' (codec: %s, CRF: %d)...\n",
		preset.Name, preset.VideoCodec, preset.QualityCRF)

	result, err := tc.Execute(ctx, job)
	fmt.Println() // newline after progress carriage returns
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nTranscode failed: %v\n", err)
		os.Exit(1)
	}

	if result.SkippedSizeGrowth {
		fmt.Printf("\n[SKIPPED] %s\nOriginal Size: %s, New Size: %s\n",
			result.VerificationReason, formatBytes(result.OriginalSize), formatBytes(result.NewSize))
		return
	}

	fmt.Printf("\nTranscode Successful!\n")
	fmt.Printf("  Output:            %s\n", result.DestPath)
	fmt.Printf("  Encoder Used:      %s (hardware: %v)\n", result.EncoderUsed, result.IsHardware)
	fmt.Printf("  Original Size:     %s\n", formatBytes(result.OriginalSize))
	fmt.Printf("  New Size:          %s\n", formatBytes(result.NewSize))
	fmt.Printf("  Space Saved:       %s (%.1f%% reduction)\n",
		formatBytes(result.SavedBytes), (1.0-result.CompressionRatio)*100.0)
	fmt.Printf("  Duration:          %.2fs\n", result.DurationSec)
	if result.VMAFScore > 0 {
		fmt.Printf("  Average VMAF:      %.2f\n", result.VMAFScore)
	}
	if result.SSIMScore > 0 {
		fmt.Printf("  SSIM Score:        %.4f\n", result.SSIMScore)
	}
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to YAML configuration file")
	_ = fs.Parse(reorderFlags(args))

	cfgMgr, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	cfg := cfgMgr.Get()

	db, err := persistence.NewEngine(cfg.Database.Path, cfg.Database.BusyTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	pending, leased, completed, failed, err := db.GetQueueCounts()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to retrieve queue counts: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("=== MediaCruncher System Status ===\n")
	fmt.Printf("Database: %s\n\n", cfg.Database.Path)
	fmt.Printf("Queue Depths:\n")
	fmt.Printf("  Pending:   %d\n", pending)
	fmt.Printf("  Leased:    %d\n", leased)
	fmt.Printf("  Completed: %d\n", completed)
	fmt.Printf("  Failed:    %d\n", failed)

	fmt.Printf("\nRecent Audit Logs (Last 10):\n")
	logs, err := db.GetRecentAuditLogs(10)
	if err == nil {
		for _, l := range logs {
			fmt.Printf("  [%s] [%s] %s: %s\n",
				l.Timestamp.Format("2006-01-02 15:04:05"),
				l.Severity,
				l.EventType,
				l.PayloadJSON,
			)
		}
	}
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
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func reorderFlags(args []string) []string {
	var flags []string
	var positionals []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				if args[i] == "-vmaf" && args[i+1] != "true" && args[i+1] != "false" {
					continue
				}
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			positionals = append(positionals, args[i])
		}
	}
	return append(flags, positionals...)
}
