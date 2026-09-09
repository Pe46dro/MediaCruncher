# MediaCruncher

High-performance, cross-platform media library transcoding and auditing daemon.

Scans media libraries, analyzes files with ffprobe, applies user-defined rules to decide how to process each file, and transcodes with hardware acceleration, VMAF quality verification, and automatic software fallback — all backed by a self-contained SQLite database.

## Quick Start

### Prerequisites

- Go 1.25+
- [ffmpeg](https://ffmpeg.org), [ffprobe](https://ffmpeg.org), and [VMAF](https://github.com/Netflix/vmaf) on your `PATH`

### Build

```powershell
go build -o mediacruncher ./cmd/main.go
```

Build with version info (GitLab CI style):

```powershell
$Commit = git rev-parse --short HEAD
$Time = Get-Date -Format "yyyyMMdd-HHmmss"
go build -ldflags="-X mediacruncher/internal/observability.BuildVersion=$Commit -X mediacruncher/internal/observability.BuildCommit=$Commit -X mediacruncher/internal/observability.BuildTime=$Time" -o mediacruncher ./cmd/main.go
```

### Configuration

Create `mediacruncher.json` in one of these locations (checked in order):

1. Directory specified by `MC_CONFIG_DIR`
2. Directory alongside the executable (if `mediacruncher.json` exists there)
3. Current working directory

```json
{
  "scan_scopes": [
    {
      "root_path": "C:/Users/me/Movies",
      "included_extensions": ["mp4", "mkv", "avi", "mov"],
      "exclusion_patterns": ["@Recycle/**"],
      "max_depth": 10,
      "symlink_policy": "skip"
    }
  ],
  "persistence": {
    "database_path": "mediacruncher.db",
    "synchronous": "full"
  },
  "evaluation": {
    "default_action": "transcode",
    "probe_timeout": "30s"
  },
  "transcoder": {
    "hardware_acceleration": true,
    "vmaf_threshold": 90.0,
    "preset": "medium",
    "max_encoding_duration": "2h",
    "fallback_to_software": true
  },
  "concurrency": {
    "worker_count": 4,
    "queue_capacity": 1000,
    "max_retries": 3,
    "base_backoff": "1s",
    "max_backoff": "1m",
    "drain_timeout": "5m"
  },
  "notification": {
    "channels": [],
    "rate_limit_per_minute": 60
  },
  "observability": {
    "log_level": "info",
    "metrics_addr": ":9090/metrics"
  }
}
```

Or override any setting via environment variables with the `MC_` prefix:

```powershell
$env:MC_WORKER_COUNT = 8
$env:MC_DATABASE_PATH = "C:/data/mediacruncher.db"
.\mediacruncher.exe
```

Default file extensions when none are configured: `mp4, mkv, avi, mov, wmv, flv, webm, m4v, mpg, mpeg, ts, m2ts`

## Architecture

MediaCruncher is a single-process daemon composed of eight internal packages:

| Package | Responsibility |
|---|---|
| `config` | JSON config + `MC_*` env var loading, `DefaultConfig()` as single source of truth for defaults |
| `persistence` | SQLite (modernc.org/sqlite, zero-CGO), WAL mode, schema migrations, queue management, audit logs |
| `filesystem` | Recursive directory scanning, partial-hash deduplication, backpressure-aware ingestion |
| `evaluation` | ffprobe metadata extraction, normalization, rule matching, decision records |
| `transcoder` | ffmpeg encoding with hardware acceleration, VMAF quality verification, corruption detection, staging |
| `concurrency` | Worker pool, priority queue, batch splitting, retry with exponential backoff |
| `notification` | Pluggable delivery to Telegram, Discord, Slack, SMTP, Gotify with batching and rate limiting |
| `observability` | Structured logging, Prometheus-style metrics registry, health checker, version vars |

## Processing Pipeline

1. **Scan** — `filesystem.Engine.Scan()` walks configured paths, classifies files, deduplicates via partial hashing
2. **Queue** — `concurrency.Engine` enqueues jobs with priority, applies backpressure when the queue is full
3. **Analyze** — `evaluation.Engine.AnalyzeFile()` invokes `ffprobe`, normalizes metadata, matches against rule sets
4. **Transcode** — `transcoder.Engine.Transcode()` runs encoding → VMAF verification → corruption check → commit, with automatic hardware-to-software retry
5. **Notify** — `notification.Engine` batches and routes events to configured channels
6. **Persist** — All queue states, decisions, and audit logs are stored in SQLite with `MaxOpenConns=1`

On startup, `persistence.RecoverProcessing()` resets any jobs left in `processing` state back to `pending` (unclean shutdown recovery).

## CI/CD

GitLab CI at `.gitlab-ci.yml` stages: validate → build → test → verify → sign → release.

Cross-platform builds for Linux (amd64/arm64), Windows (amd64), macOS (amd64/arm64). Sign and release stages only run on tags.

```powershell
# Tests (Linux and Windows runners)
go test -v -race -count=1 ./...
go vet ./...
```

No test files exist yet — the repository passes `go test ./...` with "no test files" on every package.

## Docker

### Pull from GitHub Container Registry

The official multi-architecture (`linux/amd64`, `linux/arm64`) image is published on GitHub Container Registry:

```bash
docker pull ghcr.io/pe46dro/mediacruncher:latest
# or a specific release tag
docker pull ghcr.io/pe46dro/mediacruncher:v0.1.0
```

### Quick Start

Run with a default configuration (creates `mediacruncher.json` from env):

```bash
docker run -d --name mediacruncher \
  -e MC_CONFIG_DIR=/app/config \
  -e MC_DATABASE_PATH=/app/data/mediacruncher.db \
  -v ./config:/app/config:ro \
  -v ./data:/app/data \
  -v ./media:/app/media:ro \
  ghcr.io/pe46dro/mediacruncher:latest
```

Or build locally:

```bash
docker build -t mediacruncher:local .
```

### Docker Compose

Start the full stack with one command:

```bash
docker compose up -d
```

### Volume Layout

| Volume | Mount Path | Purpose |
|---|---|---|
| `./config` | `/app/config` (ro) | Configuration directory — place `mediacruncher.json` here |
| `./data` | `/app/data` | SQLite database and persistent state |
| `./media` | `/app/media` (ro) | Media library scan scopes — adjust as needed |

### Configuration

Set configuration via the mounted `mediacruncher.json` in the `./config` volume, or override with `MC_*` environment variables (prefix each config key). See the [Configuration](#configuration) section for the full schema.

### Notes

- The runtime image includes `ffmpeg`, `ffprobe`, and `libvmaf`. The VMAF filtering capability is built into ffmpeg — a separate `vmaf` CLI binary is not required for transcoding.
- The metrics server listens on `127.0.0.1:9090/metrics` inside the container. Publish port `9090` to scrape metrics externally.

## Links

- Architectural specification: `docs/internal/architecture.md`
- Implementation instructions: `AGENTS.md`
