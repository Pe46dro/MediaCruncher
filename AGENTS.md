# MediaCruncher — Agent Instructions

## Core Commands

| Action | Command |
|---|---|
| Run tests | `go test -v -race -count=1 ./...` |
| Vet | `go vet ./...` |
| Build (local) | `go build -o mediacruncher ./cmd/main.go` |
| Build with version | `go build -ldflags="-X mediacruncher/internal/observability.BuildVersion=1.0.0 -X mediacruncher/internal/observability.BuildCommit=$(git rev-parse --short HEAD) -X mediacruncher/internal/observability.BuildTime=$(date +%Y%m%d-%H%M%S)" -o mediacruncher ./cmd/main.go` |

**No test files exist.** `go test ./...` passes with "no test files" on every package. Write new tests alongside any new code.

## Module Structure

```
cmd/main.go                         ← single entrypoint
internal/
  config/                           ← Config struct, JSON file + MC_* env var loading
  concurrency/                      ← worker pool, priority queue, batch splitter, retry
  evaluation/                       ← ffprobe analysis, rule matching, decision engine
  filesystem/                       ← directory scanning, dedup, ingestion pipeline
  notification/                     ← pluggable channels (adapters/ subpackage)
  observability/                    ← std logger, metrics registry, health checker, version vars
  persistence/                      ← sqlite engine (modernc.org/sqlite, zero-CGO), WAL mode
  transcoder/                       ← ffmpeg/VMAF orchestration, hardware accel, staging
```

## Build & Environment

- **Go 1.25.5** (declare in go.mod if adding dependencies)
- **Zero-CGO**: all deps are pure Go, including SQLite. `CGO_ENABLED=0` should work.
- `ffmpeg`, `ffprobe`, and `vmaf` must be on PATH at runtime. The transcoder and evaluation modules invoke them as external processes — no library bindings.
- Build ldflags set three vars in `internal/observability`: `BuildVersion`, `BuildCommit`, `BuildTime`.
- Artifacts land in `dist/`.

## Config

- File: `mediacruncher.json` (JSON, loaded into `Config` struct via `json.Unmarshal`)
- Env prefix: `MC_` (e.g. `MC_WORKER_COUNT=8`, `MC_DATABASE_PATH=/var/lib/mediacruncher.db`)
- Config dir search order: `$MC_CONFIG_DIR` env → dir of executable (if `mediacruncher.json` exists there) → current working directory
- See `internal/config/config.go` for all keys and defaults. `DefaultConfig()` is the single source of truth for defaults.

## Conventions

- No exported package-level state except build version vars in `observability/version.go`.
- `observability.NewStdLogger(level, module)` is the standard logger pattern; callers can pass `nil` and each package falls back to a default.
- Engine constructors follow `New(cfg Config) *Engine` pattern consistently across all modules.
- Config structs have `Parsed*` accessor methods that resolve durations and apply defaults (e.g. `ParsedConcurrency()`, `ParsedTranscoder()`).
- Module interfaces are defined by struct methods, not Go `interface{}` types — tight coupling within `internal/` is intentional.

## Architecture Notes

- **persistence** uses `modernc.org/sqlite` with WAL mode, shared-cache, immediate txlock, and `MaxOpenConns=1`. This is a single-process daemon — writes are serialized by design.
- **persistence/engine.go:183** `RecoverProcessing()` resets `processing`-state queue entries back to `pending` on startup after unclean shutdown.
- **transcoder/engine.go:67** `Transcode()` runs encoding → VMAF verification → corruption detection → commit, with automatic hardware-to-software fallback on one retry for encoding failures and quality failures.
- **filesystem/engine.go:52** `Scan()` uses `filepath.WalkDir` with scope validation, dedup via partial hashing, and a final dedup pass after discovery.
- **notification/adapters/** contains platform implementations: `discord.go`, `slack.go`, `telegram.go`, `smtp.go`, `gotify.go`.

## CI

- GitLab CI at `.gitlab-ci.yml`. Stages: validate → build → test → verify → sign → release.
- Cross-platform builds: Linux (amd64, arm64), Windows (amd64), macOS (amd64, arm64).
- `sign` and `release` stages only run on tags.
- Tests run on Linux and Windows runners: `go test -v -race -count=1 ./...` + `go vet ./...`.

## Docs

- Full architectural spec: `docs/internal/architecture.md` (6 modules, detailed interface contracts)
- Additional planning docs in `docs/internal/` — goal-contract, goal-implement-architecture, goal-loop-arch-spec.
