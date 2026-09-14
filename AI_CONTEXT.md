# AI_CONTEXT.md — Architectural & Operational Knowledge Base

> **Target Audience:** Future AI coding assistants & developers.  
> **Purpose:** Provide an immediate, high-density technical summary of MediaCruncher to avoid re-reading the entire codebase and docs.

---

## 1. Executive Summary & Purpose

**MediaCruncher** is a headless, autonomous media processing daemon written in pure Go (Go 1.25.5, Zero-CGO).  
Its job is to:
1. Scan local or network filesystem directories (`filesystem`).
2. Deduplicate files via persistent SQLite hash registry to survive restarts without re-encoding (`persistence`).
3. Analyze media files via `ffprobe` and match them against rule sets (`evaluation`).
4. Queue tasks by priority, manage batching, rate-limiting, and retries (`concurrency`).
5. Execute hardware- or software-accelerated video/audio transcoding with fast sampled VMAF metric verification and corruption checks (`transcoder`).
6. Persist processing state, queue entries, and historical decisions in SQLite (`persistence`).
7. Serve a real-time web dashboard with SSE event streaming and interactive metrics (`web`).
8. Dispatch events and status alerts to external webhooks and notification channels (`notification`).
9. Expose Prometheus metrics, structured logs, and health status (`observability`).

---

## 2. Tech Stack & Environment Constraints

- **Language:** Go 1.25.5 (pure Go, zero-CGO).
- **SQLite Engine:** `modernc.org/sqlite` (pure Go port; no CGO compiler needed).
- **External CLI Dependencies:** `ffmpeg`, `ffprobe`, and `vmaf` binaries must be in `$PATH`.
- **Operating Systems:** Linux (amd64, arm64), macOS (amd64, arm64), Windows (amd64).

---

## 3. Core Architecture & Module Flow

```mermaid
flowchart TD
    FS[filesystem: Scan & Dedup] --> DB[(persistence: SQLite)]
    FS --> Eval[evaluation: ffprobe + Rules]
    Eval --> DB
    Eval --> CC[concurrency: Priority TaskQueue]
    CC --> WP[concurrency: WorkerPools]
    WP --> TC[transcoder: ffmpeg + Sampled VMAF + Staging]
    TC --> DB
    TC --> Notif[notification: Adapters]
    TC --> Web[web: SSE Dashboard & API]
    Obs[observability: Metrics & Health] -.-> CC & TC & FS & DB & Web
```

### Module Responsibilities

1. **`cmd/main.go`**:
   - Single application entrypoint orchestrating all engines.
   - Discovers config via `$MC_CONFIG_DIR`, executable directory, or CWD.
   - Manages graceful shutdown via SIGINT/SIGTERM.
   - Scans scopes on startup and periodically via timer or manual trigger from web dashboard.
   - Implements persistent deduplication checking hashes before enqueuing to avoid reprocessing already completed/skipped items.
   - Automatically falls back to original files if the transcoded file is larger than the original (`skipped_larger`).
   - Serves the real-time SSE web server.

2. **`internal/config`**:
   - Master configuration loaded from `mediacruncher.json` or `MC_*` environment variables.
   - `DefaultConfig()` is the single source of truth for fallback values.
   - Parses human-readable durations (`2h`, `30s`) and hardware acceleration settings.
   - Supports web dashboard settings (`web.enabled`, `web.port`, `web.addr`).
   - Supports VMAF sampling parameters (`vmaf_sampling`, `vmaf_sample_segments`, `vmaf_sample_duration_sec`).

3. **`internal/filesystem`**:
   - Traverses directories with `filepath.WalkDir`.
   - Validates allowed path scopes (path traversal protection).
   - Deduplication through fast partial file hashing (head/mid/tail chunks) and size indexing.

4. **`internal/evaluation`**:
   - Runs `ffprobe` externally to obtain JSON metadata (codecs, bitrates, resolutions, streams).
   - Evaluates media against configured user rules (e.g. `codec == "h264" && bitrate > 8000k`).
   - Determines actions: `transcode`, `copy`, `ignore`, `quarantine`.

5. **`internal/concurrency`**:
   - Thread-safe priority task queue (`TaskQueue`) with channels and mutexes.
   - Multi-type worker pools (`analysis`, `transcode`, `verification`).
   - Exponential backoff retry manager (`RetryManager`) and batch splitter (`BatchSplitter`).

6. **`internal/transcoder`**:
   - Orchestrates `ffmpeg` CLI executions.
   - **Hardware Acceleration Discovery:** Actively probes hardware encoders (`ffmpeg -hide_banner -f lavfi -i testsrc -c:v <encoder> ...`) with dummy runs to verify real driver support (e.g., NVENC CUDA, Intel QSV, VAAPI, VideoToolbox) instead of purely checking static codec strings.
   - **Sampled VMAF Verification (`vmaf.go`):**
     - Fast multi-segment VMAF evaluation: for long videos, probes duration and evaluates $N$ segments (default 3 segments of 15s) using fast seek (`-ss` and `-t`), dropping computation time from 15-30 minutes to seconds.
     - Fallback to real SSIM calculation (`ssim` filter) if `libvmaf` is not built into the FFmpeg binary.
   - **Staging workflow:** transcode to temp directory -> compute VMAF score vs original -> verify integrity -> atomic rename / replacement.
   - **Automatic fallback:** Hardware acceleration -> Software (`libx265`, `libx264`, `libsvtav1`) upon encoding failure or quality threshold failure.

7. **`internal/persistence`**:
   - Pure Go SQLite database (`modernc.org/sqlite`) with WAL journal mode.
   - Single-writer architecture: `MaxOpenConns = 1`, WAL journal mode, immediate transaction locking.
   - Manages queue states: `pending`, `processing`, `completed`, `failed`, `skipped_quality`, `skipped_larger`, `ignored`.
   - `GetPendingDepth()` counts strictly `pending` items so queue depth resets to 0 when completed.
   - `GetProcessedStats()` accurately computes space saved (`orig - output` for completed files) and aggregates skipped original preservation stats.
   - `ListProcessedMedia()` aligns with stats, allowing the `skipped_quality` status filter to retrieve both `skipped_quality` and `skipped_larger` records.
   - `RecoverProcessing()` resets orphaned `processing` items back to `pending` on restart.

8. **`internal/web`**:
   - Embedded single-page web application (`internal/web/static/` via `go:embed`).
   - Server-Sent Events (SSE) broker (`/events`) streaming real-time queue states, active jobs, logs, and progress.
   - REST API endpoints:
     - `/api/status`: System hardware profile, runtime stats, disk savings, and queue depths.
     - `/api/jobs`: Currently active transcoding workers and progress.
     - `/api/media`: Paginated SQLite records with filter pills (`all`, `completed`, `skipped_quality`, `ignored`, `failed`).
     - `/api/scan`: Trigger manual re-scan.

9. **`internal/notification`**:
   - Multi-channel notification pipeline with dead-letter queue (`DLQ`).
   - Supported adapters under `adapters/`: Discord, Slack, Telegram, Gotify, SMTP (STARTTLS / SMTPS).

10. **`internal/observability`**:
    - Structured JSON/Console logger (`NewStdLogger`).
    - Prometheus metrics registry (`NewRegistry`).
    - Health checker (`NewHealthChecker`).
    - Version injection via ldflags: `BuildVersion`, `BuildCommit`, `BuildTime`.

---

## 4. Key Conventions & Rules

- **Zero-CGO:** Never introduce CGO-dependent libraries. Must compile with `CGO_ENABLED=0`.
- **Constructors:** All internal modules follow `New(cfg Config) *Engine`.
- **No Global Mutable State:** Observability build vars (`version.go`) are the only exported package-level variables.
- **Error Wrapping:** Always wrap errors with contextual detail (`fmt.Errorf("context: %w", err)`).
- **Tests with Race Detector:** Ensure all code passes `go test -v -race -count=1 ./...` and `go vet ./...`.

---

## 5. CI/CD & Build Gotchas

- **Multi-OS Shells:** CI scripts using PowerShell syntax (`$buildDir`, `Test-Path`, etc.) **must** declare `shell: pwsh` on GitHub Actions; otherwise, Linux and macOS runners default to `/bin/bash` and fail with exit code 127 (`=: command not found`).
- **GitLab CI:** Standard pipeline defined in `.gitlab-ci.yml` (validate -> build -> test -> verify -> docker-build -> sign -> release).
- **GitHub CI:** Workflows located in `.github/workflows/` (`ci.yml`, `release.yml`, and `docker.yml`). Multi-arch container images (`linux/amd64`, `linux/arm64`) are built and published to GitHub Container Registry (`ghcr.io/pe46dro/mediacruncher`) on tags and `main` branch pushes.
