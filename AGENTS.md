# AGENTS.md

> **AGENTS.md** is a living document for coding agents. It complements README.md (for humans) with build steps, test commands, code conventions, and architecture context that agents need to work effectively on this project.
>
> The closest AGENTS.md to an edited file wins. Nested AGENTS.md in subdirectories override the root for that subtree.

---

## Setup Commands

### Prerequisites

```bash
# Go 1.22+
go version

# FFmpeg 6.0+ and FFprobe (required for media analysis and transcoding)
ffmpeg -version
ffprobe -version

# Docker & Docker Compose (for containerized deployment)
docker --version
docker compose version
```

### Install Dependencies

```bash
# Download Go modules (all deps are stdlib + 2 third-party packages)
go mod download
```

### Environment Variables (Optional Overrides)

| Variable | Purpose | Default |
|----------|---------|---------|
| `MEDIACRUNCHER_DB_PATH` | SQLite database path | `~/.mediacruncher/mediacruncher.db` |
| `MEDIACRUNCHER_WORKERS` | Worker pool count | `4` |
| `MEDIACRUNCHER_GPU_LIMIT` | Max concurrent GPU encodes | `2` |
| `MEDIACRUNCHER_LOG_LEVEL` | Verbosity: debug, info, warn, error | `info` |
| `MEDIACRUNCHER_METRICS_PORT` | HTTP metrics port | `9090` |
| `MEDIACRUNCHER_HWACCEL` | Hardware acceleration mode | `auto` |
| `MEDIACRUNCHER_SCAN_INTERVAL` | Filesystem scan interval | `20s` |

---

## Build & Test Commands

### Build Binary (Static, CGO-Disabled)

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=$(git describe --tags --always --dirty)" -o bin/mediacruncher ./cmd/mediacruncher
```

### Run Tests

```bash
# All tests (unit + integration)
go test -v ./...

# Scheduling tests (only package with dedicated unit tests — 19/19 PASS)
go test -v ./internal/scheduling/...

# Race detection
go test -race -v ./...
```

### Docker Build & Deploy

```bash
# Build image
docker build -t mediacruncher:test

# Run (binds to Tailnet IP — use 0.0.0.0 for all interfaces)
docker run -d \
  --name mediacruncher \
  -p 100.64.0.16:9090:9090 \
  -v $(pwd)/config:/etc/mediacruncher:ro \
  -v $(pwd)/data:/var/lib/mediacruncher \
  -v $(pwd)/media:/media:ro \
  mediacruncher:test

# Stop & remove
docker stop mediacruncher && docker rm mediacruncher
```

### Docker Compose

```bash
docker compose up -d
docker compose logs -f
docker compose down
```

---

## Code Style & Conventions

### Go Style

- **Package names**: lowercase, single word, no underscores (`persistence`, not `persistence_engine`)
- **Exported names**: `UpperCamelCase` for types and functions; `lowerCamelCase` for unexported
- **Error handling**: always check errors, wrap with `fmt.Errorf("context: %w", err)`, never ignore
- **Context propagation**: `ctx context.Context` is always the first parameter of blocking/cancellable operations
- **Channel naming**: send channels use the same name as receive channels (Go convention — e.g. `doneCh`)
- **No global state** except:
  - `globalMetrics` in `observability` (singleton, lock-free via atomics)
  - `currentAPIKey` in `server` (mutable, package-level)
- **Embedding**: `//go:embed web/*` for static assets — never load from disk at runtime

### Architecture Patterns

- **Dependency Injection**: `server.NewServer()` takes minimal params; all deps attached via setter methods after construction. This avoids cyclic imports and keeps constructors testable.
- **Thread Safety**: Each package documents its concurrency model in its package doc comment. Default: readers concurrent-safe, writers serialised.
- **Error Recovery**: Workers recover from crashes via lease expiration (3 min). Retries use exponential backoff with jitter.
- **Process Isolation**: All external processes (ffmpeg, ffprobe, VMAF) run in isolated process groups (POSIX) or Job Objects (Windows) — zero orphans on crash.

### Commit Messages

Follow conventional commits:

```
feat: add VMAF quality verification
fix: resolve schedule entry race condition
refactor: extract context propagation from worker pool
docs: add package-level documentation for internal/*
```

### Test Coverage Expectations

- Only `internal/scheduling/` has dedicated unit tests (19 tests, all PASS).
- Other packages are validated via end-to-end tests in the Docker container.
- When adding new logic, prefer adding tests to the owning package. If testing is impractical, document the limitation in the code.

---

## Architecture Overview

### Module Map

| Package | File | Responsibility |
|---------|------|----------------|
| `main` | `cmd/mediacruncher/main.go` | CLI entry point — 6 subcommands: daemon, scan, eval, transcode, status, version |
| `config` | `internal/config/config.go` | YAML + env configuration, hot-reload, path normalization |
| `persistence` | `internal/persistence/db.go` | SQLite persistence — dual-connection (1 writer, 10 readers), WAL mode, actor pattern |
| `filesystem` | `internal/filesystem/scanner.go` | Recursive directory scanning, two-tier deduplication, ingestion buffer |
| `dedupe` | `internal/dedupe/index.go` | Two-tier dedup: size check (zero I/O) → boundary SHA-256 hash (2MB per file) |
| `evaluation` | `internal/evaluation/pipeline.go` | 4-stage pipeline: probe(ffprobe) → normalize → rule-match → synthesize FFmpeg stream plan |
| `transcoder` | `internal/transcoder/transcoder.go` | FFmpeg orchestration, hardware acceleration, VMAF/SSIM verification, size-growth safety |
| `concurrency` | `internal/concurrency/worker.go` | Worker pool, CPU/GPU semaphores, lease management, retry with backoff |
| `prefetch` | `internal/concurrency/prefetch.go` | DB→worker buffer: polled every 1s, batch size 10, 30-min lease expiry |
| `scheduling` | `internal/scheduling/engine.go` | Time-based job scheduler: 2 goroutines (processLoop, tickLoop), window triggers |
| `notification` | `internal/notification/engine.go` | Event dispatch to 6 channels: discord, telegram, slack, webhook, gotify, smtp |
| `observability` | `internal/observability/metrics.go` | Singleton metrics (Prometheus + JSON), module health tracking |
| `observability` | `internal/observability/logger.go` | Structured logging, optional file rotation (10MB, 7 backups) |
| `server` | `internal/server/server.go` | HTTP server + SPA dashboard — 4 middleware stack, 17 REST endpoints |
| `shutdown` | `internal/shutdown/coordinator.go` | 4-phase graceful shutdown: drain → persist → teardown → report + force-kill watchdog |
| `proc` | `internal/proc/supervisor.go` | Child process supervision: Job Objects (Windows), process groups (POSIX) |

### Data Flow

```
[Filesystem Scanner] ──discovery──→ [Ingestion Buffer] ──enqueue──→ [SQLite Queue]
                                                                          │
                                                          [Prefetcher] ←──┘
                                                           │
                                                       [Worker Pool]
                                                       /     │     \
                                        [Evaluation]  [Transcoder]  [Notification]
                                           │              │              │
                                         [Rule Match] [VMAF Verify] [Channel Adapters]
                                           │              │              │
                                           └───────promote──────────────→ [Destination File]
```

### Thread Safety Summary

| Component | Concurrency Model | Notes |
|-----------|------------------|-------|
| `config.Manager` | `sync.RWMutex` | Readers concurrent, writers exclusive. Hot-reload via listeners. |
| `observability.Metrics` | `atomic.Uint64/Int64` + `sync.RWMutex` | Counters lock-free. Gauges (vmafSum, healthMap) use mutex. |
| `dedupe.Index` | `sync.RWMutex` | Map access protected. Shared singleton across scanners. |
| `persistence` | Single-writer actor + read pool | Write channel serialised; read pool concurrent (WAL mode). |
| `scheduling.Engine` | `sync.Mutex` | All public methods lock. Channels buffered (cap 10) prevent leaks. |
| `server` | `net/http` managed | Handlers concurrent-safe via immutable reads + atomic metrics. |
| `shutdown.Coordinator` | `sync.Mutex` + `sync.WaitGroup` | Idempotent Execute(). Phases run handlers concurrently. |

---

## Key Constraints & Gotchas

### CGO
- **Never enable CGO** for production builds. All dependencies are pure Go.
- Build always with `CGO_ENABLED=0`.
- The SQLite driver (`modernc.org/sqlite`) is pure Go — no C bindings.

### FFmpeg Process Isolation
- External processes (ffmpeg, ffprobe, VMAF) must **never** be orphaned.
- All process invocations go through `internal/proc/supervisor`.
- On POSIX: `Setpgid: true` → kill negative process group ID.
- On Windows: `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`.
- Pipes drained in non-blocking goroutines with bounded buffers.

### Hardware Acceleration
- Hardware detection runs once at `transcoder.NewTranscoder()` construction time.
- GPU encoding requires acquiring a permit from the GPU semaphore **before** dispatch.
- GPU session exhaustion → fallback to CPU software encoding (automatic).
- Consumer GPUs (NVENC on RTX 30xx/40xx) have hard session limits — never use unbounded GPU workers.

### VMAF Quality Verification
- VMAF runs as a **second-pass** FFmpeg command (after transcode completes).
- Stratified segment sampling: 3 segments at 15%, 50%, 85% timestamps.
- Default threshold: 93.0. Below threshold → quality_failed (single retry with adjusted CRF).
- Full-file VMAF is opt-in (for archival presets only).

### SkipIfLarger Safety
- If transcoded output is larger than source, the encode is **aborted** and original preserved.
- This prevents accidental upscales or misconfigured presets from wasting space.
- Controlled by `transcoder.skip_if_larger` in config (default: true).

### Cache Control
- Static assets (favicon, icons) have `Cache-Control: public, max-age=86400` (24h).
- API responses have no cache headers (useful for real-time status polling).

---

## Deployment Notes

### Docker Image
- Multi-stage build: Go builder → Debian runtime (linuxserver/ffmpeg base).
- Binary compiled with `-trimpath -ldflags="-s -w"` → ~11MB static binary.
- No root user required in container (runs as non-privileged user).

### Volume Mounts
| Host Path | Container Path | Purpose |
|-----------|---------------|---------|
| `config/` | `/etc/mediacruncher/config.yaml:ro` | Read-only configuration |
| `data/` | `/var/lib/mediacruncher` | SQLite database + audit state |
| `staging/` | `/tmp/mediacruncher/staging` | Fast staging area for active encodes |
| `media/` | `/media:ro` | Media library to scan |

### Graceful Shutdown
- SIGINT / SIGTERM → coordinator.Execute()
- 4 phases: drain workers → persist state → teardown connections → final reporting
- Force-kill watchdog: os.Exit(1) if shutdown hangs beyond `force_kill_timeout` (default: 30s)
- Container `docker stop` default is 10s — ensure `drain_timeout` + `force_kill_timeout` ≤ 10s for clean exits.

---

## Known Limitations

1. **API authentication is optional** — `observability.api_key` must be set in config.yaml to enable. When empty, all endpoints are public.
2. **Only `scheduling/` has dedicated unit tests** — 19 tests, all PASS. Other packages validated via container end-to-end testing.
3. **Logger file rotation is not enabled by default** — `FileRotatingWriter` exists but `logFile=""` in main. Enable by setting a path in config.
4. **Hot-reload listeners are unregistered** — `config.Manager` supports `OnReload()` but daemon doesn't register any listeners for dynamic actions.
5. **No health endpoint for worker pool** — `/healthz` reports module health but not live worker count. Use `/metrics.json` instead.

---

## Contributing to This File

This file is living documentation. Update it when:
- Adding new build/test commands
- Changing code conventions
- Modifying architecture patterns
- Adding new constraints or gotchas
- Fixing incorrect assumptions

Keep it concise — agents parse this file. Every section should help an agent work on the codebase.
