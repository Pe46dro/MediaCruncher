# MediaCruncher Architectural Specification

This document provides a production-grade, implementation-ready architectural specification for MediaCruncher, a high-performance, cross-platform Go media library transcoding and auditing application. All descriptions use plain natural language with no code, no pseudo-code, and no language-specific syntax.

---

## Module A: Persistence Engine

### Primary Responsibility

Provide durable, transactional storage for the application's queue entries, job metadata, system settings, and audit logs. The persistence engine serves as the single authoritative source of truth for all operational state and task lifecycle stages, enabling deterministic recovery across restarts and supporting concurrent read access alongside serialized write mutations. In-memory queue structures in downstream modules function strictly as bounded prefetch buffers of this persistent state. The engine must operate without any C bindings or CGO dependencies, relying exclusively on a native pure Go database driver to maintain build portability across platforms.

### Core Data Structures

The persistence layer manages four logical tables, each representing a distinct domain entity, operating under a mandatory engine configuration profile:

- Database Configuration Profile: The engine enforces write-ahead logging and concurrent reader semantics via explicit pragmas applied at connection initialization: WAL journal mode (`PRAGMA journal_mode = WAL`), synchronized normal flush (`PRAGMA synchronous = NORMAL`), extended busy handler timeout (`PRAGMA busy_timeout = 5000`), and strict foreign key validation (`PRAGMA foreign_keys = ON`).

- Queue Entries: The authoritative persistent record of all transcoding and evaluation jobs with fields for source file path, current processing state (pending, leased, processing, completed, quality-failed, review-required, permanently-failed), assigned worker identifier, priority level, retry count, creation timestamp, lease expiration timestamp, and scheduled execution time. Each entry carries a unique surrogate key and supports indexed lookups by state, priority, and worker assignment.

- Job Metadata: Stores the full analysis results produced by the evaluation pipeline for each media file. This includes extracted video and audio codec information, resolution, frame rate, bit rate, duration, multi-track audio descriptors, subtitle streams, and the decision rendered by the rule-matching engine. Metadata entries are linked to their corresponding queue entry via a foreign key relationship.

- System Settings: Holds configurable application parameters that persist across restarts. This includes transcoding presets, quality thresholds, worker pool sizing, rate limit configurations, notification channel settings, and scanned path registries. Settings are keyed by a unique identifier and carry a version stamp to support optimistic concurrency control.

- Audit Logs: Append-only records of significant operational events: job submissions, state transitions, transcoding completions, quality failures, notification deliveries, and system lifecycle events. Logs carry a monotonically increasing sequence number, timestamp, event type, severity level, and a structured payload containing contextual details.

### Key Interface Contracts

The persistence engine exposes an actor-mediated interface separating concurrent reads from serialized writes:

- Single-Writer Actor Architecture: To completely eliminate write contention, lock contention errors (`SQLITE_BUSY`), and deadlock retries across concurrent workers, all write operations (enqueuing, state transitions, metadata upserts, audit appends, settings changes) are submitted to a dedicated, bounded write-request channel. A single background write coordinator goroutine drains this channel and executes all write transactions sequentially using a dedicated, exclusive write connection. Operations return synchronous completion channels or futures to caller workers.

- Concurrent Read Pool: Concurrent workers and reporting queries execute read operations (such as queue prefetching, metadata queries, and settings loads) concurrently through a shared pool of read-only database connections, fully utilizing SQLite WAL mode's concurrent reader capabilities without blocking the write coordinator.

- Connection Lifecycle: Opens the database connections from a provided file path, executes the configuration pragmas, applies controlled schema migrations in ordered transaction blocks, and validates connection health through a ping operation. Closing drains the write channel, flushes remaining transactions, releases file locks, and closes both read and write pools.

- Queue Operations: Enqueue new jobs into persistent storage. Lease the highest-priority batch of pending jobs for prefetching by the concurrency engine, atomically transitioning them to leased/processing state with recorded worker identifiers and lease timeouts. Update job states on completion, quality failure, or fatal error with foreign-key metadata linkage. Requeue jobs with incremented retry count and exponential backoff timestamps when transient failures occur.

- Metadata Operations: Upsert job metadata using the queue entry identifier as the foreign key, creating a new record or updating an existing one. Retrieve full metadata for a given job, or bulk-query metadata across a set of job identifiers for reporting purposes.

- Settings Operations: Load all system settings into an immutable configuration snapshot on startup. Update individual settings by key through the write actor, with optimistic concurrency enforced through a version column that rejects stale writes. Reload settings on demand to support hot-reload capability.

- Audit Operations: Append single or batched audit log entries via the write actor. Purge entries beyond a configurable retention window, retaining a minimum baseline for forensic analysis. Query entries by event type, severity, time range, or job identifier over the read pool.

### State Flow and Lifecycle

The persistence engine lifecycle follows these phases:

1. Initialization: On application startup, the engine initializes the database file, applies WAL and integrity pragmas, runs pending migrations sequentially within self-contained transactions, verifies table and index integrity, and starts the write coordinator actor loop.

2. Runtime: The engine serves concurrent read requests via the read connection pool and routes all mutations through the single write coordinator. Queue prefetch operations retrieve available pending jobs in priority order. When workers complete tasks, completion messages pass through the write channel, ensuring serialized state updates with zero database lock contention. Metadata writes are coalesced into batch transactions where possible.

3. Shutdown: During graceful shutdown, the engine rejects new enqueue submissions, processes and drains remaining writes in the write actor queue, commits all pending write transactions synchronously, closes the read pool, and closes the write connection cleanly.

4. Recovery: On startup after an unclean shutdown, the engine executes crash recovery: it scans queue entries left in leased or processing states where the lease timestamp has expired, transitions them back to pending, increments their retry count, and logs the recovery event to the audit trail.

### Error Handling and Recovery Strategy

The persistence engine employs a tiered error handling approach:

- Connection-level failures (database file inaccessible, disk full, permission denied) are treated as fatal errors that trigger an orderly application shutdown, as work cannot safely proceed without durable state.

- Write actor failure handling: Transient transaction errors handled by the single-writer actor (e.g. momentary disk latency) are retried internally with exponential backoff before surfacing an error to the calling worker. Because only one goroutine ever writes to the database, `SQLITE_BUSY` contention between application threads is eliminated by design.

- Migration failures are fatal; startup halts immediately if the database schema cannot be brought to the target version cleanly.

- Write-ahead logging with synchronous normal flush guarantees that all committed write transactions survive application crashes, with full recovery enabled on restart.

### Integration Points

- Module E (Concurrency Engine): The primary consumer of queue operations. The concurrency engine prefetches work items from the persistence engine and submits job status transitions through the single-writer actor channel.

- Module C (Evaluation Pipeline): Writes decision records and job metadata through the write coordinator actor.

- Module D (Transcoder): Submits transcoding completion, quality verification scores, and failure status updates to the write actor.

- Module F (Notification Engine): Queries the audit log via read connections to format notifications and submits delivery records and dead-letter queue entries to the write actor.

- Cross-Cutting (Configuration Management): Loads system settings on startup and writes hot-reload adjustments through the write actor.

- Cross-Cutting (Observability): Publishes queue depth, transaction throughput, and write channel buffer utilization metrics.

### Trade-offs and Design Justification

SQLite was selected over external databases (PostgreSQL, MySQL) because MediaCruncher operates as a self-contained, single-host daemon with zero infrastructure dependencies. The zero-CGO requirement is satisfied by a pure Go SQLite driver, ensuring effortless cross-compilation across all target platforms. While pure Go drivers trade a small degree of raw CPU execution speed compared to C SQLite, write performance is dominated by disk I/O, where WAL mode and transaction batching provide more than sufficient throughput for media library workloads (tens of thousands of operations). The Single-Writer Actor pattern resolves SQLite's fundamental single-writer constraint by architecturally preventing concurrent write attempts at the Go application level, entirely removing database-level locking conflicts, lock starvation, and retry storms. Durable WAL mode with crash recovery provides robust data protection across unclean shutdowns with minimal latency overhead.

---

## Module B: Filesystem Engine

### Primary Responsibility

Discover, catalog, and prepare media files from user-configured scanned paths for processing by the evaluation pipeline. The filesystem engine performs recursive directory traversal, filters files by type and accessibility, detects and deduplicates existing files using a two-tier composite signature, and pushes the resulting file list into the processing queue through a backpressure-aware ingestion pipeline. It handles permission errors, symbolic links, deeply nested structures, extended-length cross-platform paths, and I/O timeouts without aborting the entire scan.

### Core Data Structures

The filesystem engine maintains three primary abstractions:

- Scan Scope: Represents a user-configured root path along with inclusion and exclusion rules. Each scope carries a list of allowed file extensions, glob-based exclusion patterns, a maximum directory depth limit, a symbolic link policy (follow, skip, or dereference), cross-platform path normalization rules (handling Windows UNC paths and extended-length prefixes), and a timeout duration for I/O operations. Scopes are validated before traversal begins, and invalid scopes are reported as warnings without halting the scan.

- File Record: The output unit of the filesystem engine, containing the normalized absolute file path, exact file size in bytes, last modification time, a composite deduplication signature consisting of exact byte size and cryptographic SHA-256 hashes of the first and last megabytes, media type classification based on extension and MIME sniffing, and a list of warnings encountered during discovery (permission denied, unreadable, unsupported format). File records are the contract between the filesystem engine and the evaluation pipeline.

- Ingestion Buffer: An in-memory bounded queue that collects file records as they are discovered and feeds them to the processing queue at a controlled rate. The buffer has a configurable capacity, applies deduplication against a compact in-memory hash index that stores raw fixed-size byte arrays rather than heap-allocated string representations to minimize garbage collection overhead, and blocks producers when full to enforce backpressure.

### Key Interface Contracts

The filesystem engine exposes a scanning interface with these operational boundaries:

- Scan Scope Validation and Path Normalization: Validates each configured scan scope by verifying path existence, readability, and configuration sanity. Normalizes file paths across operating systems: converts path separators to uniform standards, resolves symbolic links per policy, and applies Windows extended-length prefixes (`\\?\`) for paths exceeding the 260-character limitation, as well as handling Universal Naming Convention (UNC) paths for network-attached storage.

- Recursive Traversal: Walks each valid scan scope recursively, applying inclusion filters by file extension and exclusion filters by glob pattern. The traversal honors the symbolic link policy and respects depth limits. Each discovered file is classified by media type and a File Record is constructed. Concurrent subdirectory traversal is used to avoid I/O idle time, with a worker limit to prevent resource exhaustion.

- Two-Tier Deduplication: Before emitting each File Record, the engine checks the in-memory index using a two-tier evaluation strategy:
  1. Size-First Filtering: Exact file size in bytes is compared against known files. If the byte size does not match any known file, the file cannot be a duplicate; no partial hashing I/O is performed.
  2. Partial Hash Verification: Only when an exact byte-size match occurs does the engine read and compute SHA-256 hashes over the first and last megabytes. Comparing both size and boundary hashes eliminates false positives caused by shared container headers (such as standardized MKV/MP4 metadata or encoder padding) across different media files. If an identical composite signature is found, a deduplication warning is attached, and the file is skipped or flagged per policy. Full cryptographic hashing of the entire file is deferred to the evaluation pipeline only when collision ambiguity requires forensic verification.

- Backpressure-Aware Ingestion: Pushes File Records into the ingestion buffer. When the buffer is full, the discovery process blocks until space is available, creating a natural backpressure signal that slows file discovery to match the downstream processing capacity. When the processing queue rejects an item due to capacity limits, the buffer accumulates, and discovery eventually pauses. An optional overflow policy determines whether blocked items are dropped, buffered with an expanded capacity, or cause the scan to halt entirely.

- Error Handling During Traversal: When a permission denied or access denied error is encountered during directory traversal, the engine logs the error, records it in the scan report, and continues with sibling directories. Retry attempts are made once after a brief delay before recording the error as permanent. Timeout errors on file stat operations cause the file to be skipped with a warning.

### State Flow and Lifecycle

The filesystem engine lifecycle follows these phases:

1. Configuration: Accepts a list of scan scopes with validation rules. Validates each scope, normalizes paths, and builds an internal scope graph, resolving overlapping and nested paths to avoid duplicate scans.

2. Discovery: Launches concurrent subdirectory traversal workers for each scope. Each worker independently traverses its assigned directory subtree, applies filters, performs two-tier deduplication, and pushes File Records to the ingestion buffer. Workers respect the configured concurrency limit and emit progress updates at regular intervals.

3. Aggregation: As workers complete, the engine aggregates the discovered File Records from the ingestion buffer. A final deduplication pass against a complete composite signature index eliminates duplicates that were missed during streaming discovery due to files being discovered by different workers in parallel.

4. Report: Generates a scan report containing total files discovered, files accepted, files skipped by type filter, files flagged as duplicates, directories skipped due to permission errors, and total scan duration. The report is published to the observability subsystem for monitoring.

5. Cleanup: Releases all worker processes, closes file handles, and drains the ingestion buffer. If the engine was interrupted during scanning, any remaining items in the buffer are flushed before cleanup.

### Error Handling and Recovery Strategy

The filesystem engine employs a resilient error handling approach:

- Permission errors during directory traversal are logged and recorded but do not abort the scan. The scan report includes a per-directory breakdown of permission failures.

- I/O timeout errors cause individual files or directories to be skipped with warnings. The engine does not retry timeout errors, as they typically indicate hardware or network issues that are unlikely to resolve during a single scan.

- Disk full errors during metadata collection cause the scan to pause briefly and retry once. If the second attempt also fails, the engine logs a fatal scan error and stops processing that scope, continuing with remaining scopes.

- The engine supports resumable scans through a scan checkpoint mechanism that records the last successfully processed path in each scope. On subsequent scans, the engine skips already-processed paths, with a configurable freshness policy that allows re-scanning of previously processed directories if their modification time has changed.

### Integration Points

- Module A (Persistence Engine): Pushes processing jobs derived from accepted File Records into the persistence queue via the single-writer actor channel. The ingestion contract between the two modules defines the shape and semantics of each queued item.

- Module C (Evaluation Pipeline): Provides the File Records that the evaluation pipeline analyzes. The evaluation pipeline may request additional metadata from the filesystem engine for files where initial analysis produced inconclusive results.

- Module E (Concurrency Engine): Receives the ingestion buffer's processed output as the source of work items. The concurrency engine reads from the ingestion buffer to dispatch work to workers.

- Cross-Cutting (Configuration Management): Reads scan scope configurations, file extension lists, depth limits, symlink policies, and timeout values from the centralized settings store.

- Cross-Cutting (Observability): Reports scan progress metrics, file discovery rates, error counts by type, and deduplication statistics.

### Trade-offs and Design Justification

Concurrent subdirectory traversal was chosen over sequential traversal because modern storage systems benefit from parallel I/O operations, and the overhead of concurrent execution-unit management is minimal compared to I/O wait times. The worker limit prevents resource exhaustion on systems with thousands of small directories. The two-tier deduplication strategy (exact size check followed by boundary hashes) eliminates wasteful disk reads for non-colliding files while preventing false-positive deduplication collisions on container headers. Storing deduplication signatures as raw binary arrays rather than string allocations ensures minimal memory footprint and zero Go runtime garbage collection thrashing during scans of millions of files. Deferring full-file cryptographic hashing avoids reading entire multi-gigabyte media files during discovery. The backpressure-aware ingestion buffer decouples discovery speed from downstream processing capacity without dropping items.

---

## Module C: Evaluation Pipeline

### Primary Responsibility

Analyze media files using ffprobe metadata extraction, normalize the extracted data into a consistent internal representation, apply the user-configured rule-matching engine to determine the appropriate action for each file (including multi-stream audio and subtitle preservation directives), and persist the decision and supporting metadata to the persistence engine. The evaluation pipeline is the decision-making core of the system, transforming raw file information into actionable processing and stream-mapping directives.

### Core Data Structures

- Analysis Result: The primary output of the ffprobe analysis stage, containing extracted video codec, color space, bit depth, resolution, frame rate, bit rate, duration, container format, and detailed stream descriptors: all audio streams (codec, channel layout, channel count, sample rate, language tags, default/commentary flags), all subtitle streams (codec format, language tags, forced/SDH flags), and container chapter markers.

- Normalized Metadata: The rule-engine-compatible representation of the analysis result, with all values converted to standard units, standardized codec name mapping (including codec alias resolution), boolean flags for HDR presence (HDR10, Dolby Vision, HLG) and high bit depth, and computed derived fields such as estimated output size at target bitrate.

- Rule Set: A collection of user-defined rules, each with a name, an ordered list of match conditions on normalized metadata fields, an action directive (transcode, stream copy, skip, flag for review), an optional output preset, a stream mapping policy (specifying multi-track audio retention, lossless audio passthrough vs. downmixing, and subtitle handling), and an optional priority score for conflict resolution when multiple rules match a file.

- Decision Record: The final output of the evaluation pipeline for each file, containing the matched rule identifier, the chosen primary action, the output preset (if applicable), an explicit stream mapping specification (specifying per-stream disposition: video transcode parameters, audio stream copying or re-encoding, subtitle track preservation), any flags or warnings generated during evaluation, and the full normalized metadata as supporting evidence for audit purposes.

### Key Interface Contracts

- Analysis Pipeline: Accepts a file path, invokes ffprobe with structured JSON output arguments to extract comprehensive container and stream metadata, parses the structured output, and constructs an Analysis Result. The pipeline handles ffprobe exit codes, captures stderr output for error diagnostics, and enforces a configurable timeout on the ffprobe invocation. Timeouts result in a flag on the Analysis Result rather than a pipeline failure, allowing the evaluation to proceed with partial data.

- Normalization: Transforms an Analysis Result into Normalized Metadata by applying unit conversions, codec name standardization, and derived field computation. Normalization is deterministic and idempotent: running it on the same Analysis Result always produces the same Normalized Metadata.

- Rule Matching and Stream Mapping Synthesis: Evaluates a file's Normalized Metadata against the full rule set in priority order. Each rule's conditions are evaluated in sequence, and the first matching rule determines the primary decision. Simultaneously, the engine synthesizes explicit stream mapping directives (`-map 0:v -map 0:a -map 0:s?` with per-stream disposition) according to configured preservation rules. This ensures high-definition surround sound (e.g., TrueHD, DTS-HD MA, Dolby Atmos) can be passed through via direct stream copy while video is transcoded, and all relevant language and subtitle tracks are retained. If multiple rules match at the same priority level, the rule with the most specific condition match count wins. If no rules match, the default rule applies.

- Decision Persistence: Submits the Decision Record and full metadata payload to Module A via the single-writer actor channel, linking it to the corresponding queue entry. The write includes the full normalized metadata as audit evidence, even when the decision is to skip the file.

### State Flow and Lifecycle

The evaluation pipeline lifecycle for each file follows these phases:

1. Invocation: The pipeline receives a file path from the concurrency engine. It creates a cancellation context that can be aborted by the concurrency engine if the worker is draining.

2. Analysis: The pipeline invokes ffprobe with a timeout and captures structured output. The raw output is parsed into an Analysis Result. If parsing fails, a partial analysis result is constructed from any successfully parsed fields, with a parsing error flag set.

3. Normalization: The Analysis Result is normalized into standard form. Codec aliases are resolved, units are standardized, and derived fields are computed.

4. Evaluation and Mapping Synthesis: The normalized metadata is matched against the rule set. The matching engine evaluates conditions in order, determines the primary action, and synthesizes the per-stream mapping directives.

5. Persistence: The Decision Record is submitted to Module A's write actor channel, updating the queue entry state and storing the metadata within a single transactional write block.

6. Completion: The pipeline signals completion to the concurrency engine with the decision outcome, synthesized stream mapping directives, file processing duration, and any errors encountered during analysis.

### Error Handling and Recovery Strategy

- ffprobe invocation failures (binary not found, permission denied, timeout) are treated as analysis errors, not fatal errors. The file is flagged for manual review and the queue entry is updated to a review-required state.

- Parsing failures on ffprobe output are logged with the raw output for debugging, and a best-effort partial result is used for rule matching. Rules that depend on unavailable fields are skipped during matching.

- Rule matching failures (malformed rule set, missing conditions) cause the default rule to apply with a warning logged to the audit trail. Malformed rules are quarantined from active evaluation until corrected.

- Persistence write failures cause the file to be requeued with a retry, allowing the evaluation to be redone. Since the normalization stage is deterministic and idempotent, retried evaluations with unchanged inputs produce identical decision records, ensuring audit trail consistency. The rule set is cached in memory between evaluations to avoid repeated loading.

### Integration Points

- Module A (Persistence Engine): Writes Decision Records and metadata via the single-writer actor channel. Reads rule set configuration from system settings.

- Module D (Transcoder): Receives the Decision Record, output preset, and explicit stream mapping directives to drive transcoding parameters and stream copy operations.

- Module E (Concurrency Engine): Receives evaluation results to determine next steps. Evaluation outcome drives the concurrency engine's job routing decisions.

- Cross-Cutting (Configuration Management): Reads rule sets, codec alias mappings, and default evaluation settings from the centralized configuration.

- Cross-Cutting (Observability): Publishes evaluation duration, match counts by rule, and decision distribution metrics.

### Trade-offs and Design Justification

The decoupling of analysis, normalization, and rule matching into separate stages provides clarity and testability. ffprobe is invoked as an external process rather than through library bindings to avoid CGO dependencies and to isolate potential crashes or memory issues in the external tool from the application runtime. Synthesizing explicit per-stream mapping directives during evaluation eliminates guesswork in the transcoder, guaranteeing that multi-channel surround tracks and subtitles are preserved rather than inadvertently discarded. The in-memory rule set cache avoids repeated file I/O on rule set updates. Deferring transcoding execution to the concurrency engine provides a clean separation between analysis and execution, allowing priority-based scheduling and resource semaphores to govern heavy transcoding tasks.

---

## Module D: Transcoder

### Primary Responsibility

Execute media transcoding operations based on decisions from the evaluation pipeline, utilizing hardware-accelerated encoding under strict hardware session limits, performing high-throughput quality verification through stratified segment VMAF scoring and fast metric pre-filtering, managing temporary files and staging directories across filesystem boundaries, and enforcing quality thresholds before committing transcoded output. The transcoder is the heaviest resource consumer in the system and handles codec negotiation, GPU session concurrency limits, cross-device atomic moves, corruption detection, and safe in-place replacements.

### Core Data Structures

- Hardware Capability Profile: A snapshot of available hardware encoding resources, including GPU device enumeration, supported codec-acceleration mappings (which codecs are hardware-accelerated on which devices), device thermal and utilization status, concurrent session caps (e.g. NVENC maximum concurrent session limits), and available video RAM. The profile is refreshed periodically or on-demand when encoding failures suggest hardware issues.

- Encoding Preset: A configuration of encoding parameters derived from the evaluation pipeline's output preset and stream mapping directives, including target video codec, quality rate-control parameters (CRF/CQ/bitrate targets), resolution constraints, preset speed, multi-track audio copy or re-encode configurations, and subtitle passthrough options. Presets are validated against available hardware capabilities before encoding begins.

- Transcode Job: The execution unit for the transcoder, containing the source file path, output destination path, encoding preset, explicit stream mapping directives from Module C, hardware acceleration preferences, GPU session permit token, VMAF verification parameters, and job lifecycle state.

- Verification Result: The output of the quality verification stage, containing the fast SSIM/PSNR pre-filter score, computed per-segment VMAF scores from stratified time slices, the composite weighted VMAF score, and a pass-fail determination against configured quality thresholds.

### Key Interface Contracts

- Hardware Discovery and Session Negotiation: Queries the system for available GPU devices and their encoding capabilities (including NVENC, QuickSync, and VAAPI). Given an encoding preset, the transcoder negotiates the target codec and acceleration method. To prevent GPU out-of-memory crashes and driver-level session rejections, hardware encoding requires acquiring a permit from Module E's GPU worker semaphore. If hardware acceleration is unavailable, permits are exhausted, or session initialization fails, the transcoder automatically falls back to software encoding with an informational audit log entry.

- Encoding Execution: Launches the external ffmpeg encoding process with explicit stream mappings (`-map`) and preset parameters. Output is written exclusively to a dedicated staging directory to prevent partial output from being accessed by media libraries or scanner daemons. The transcoder monitors process progress, resource utilization, and return codes.

- Stratified VMAF Quality Verification: To avoid system throughput collapse where full-file VMAF verification can exceed the duration of the transcode itself, the transcoder executes a high-efficiency tiered verification pipeline:
  1. Fast SSIM/PSNR Pre-filter: Fast structural similarity metrics are computed during or immediately following transcode execution. If metric scores indicate severe degradation, the encode is rejected early without additional overhead.
  2. Stratified Segment Sampling: When pre-filtering passes, the transcoder extracts three to five representative 30-second clips sampled across evenly distributed runtime percentiles (e.g., 15%, 50%, and 85% timestamps) from both original and transcoded files. VMAF is calculated across these sample segments, achieving over 98% statistical correlation with full-file scoring while reducing verification time by up to 95%.
  3. Master Archival Override: Full-file VMAF verification is retained as an opt-in configuration exclusively for master archival presets where absolute whole-file scoring is mandated.

- Resilient File Management and Cross-Filesystem Promotion: Manages temporary staging files with atomic guarantees across disparate storage devices:
  1. Intra-Filesystem Move: When staging and destination reside on the same filesystem/volume, the transcoder commits output via atomic operating system rename (`os.Rename`).
  2. Cross-Device Fallback (`EXDEV`): When staging (e.g. fast local NVMe SSD) and destination (e.g. network SMB/NFS share or secondary storage pool) span different mount points, an atomic rename fails with an `EXDEV` error. The transcoder catches this error, executes a buffered streaming copy to a temporary file (`.media_cruncher_tmp`) on the destination volume, validates file size and checksum integrity against the staged file, performs an atomic intra-filesystem rename on the destination to promote the file, and removes the staged artifact.
  3. Safe In-Place Replacement: When replacing an existing source file, the transcoder renames the source to a temporary `.backup` path before promoting the new transcode. The backup is unlinked only after the new file passes corruption checks and promotion succeeds; if promotion fails, the backup is restored immediately.

- Corruption Detection: Validates transcoded media before promotion by opening the file with ffprobe to verify stream headers, packet continuity, and decoding integrity. If corruption is detected, the job is failed and marked for software re-encoding recovery.

### State Flow and Lifecycle

The transcoder lifecycle for each job follows these phases:

1. Preparation: The transcoder receives a transcode job with explicit stream mappings and cancellation context. It validates source file readability, checks destination disk capacity, and allocates an isolated staging workspace.

2. Resource Allocation: If hardware encoding is requested, the transcoder acquires a GPU session permit from the concurrency engine. If permits are unavailable or the GPU is thermal-throttled, it falls back to software encoding.

3. Encoding: Launches the ffmpeg process writing to the staging workspace. Subprocess execution is tied to the parent cancellation context and child process supervisor.

4. Verification: On encode completion, the transcoder releases the GPU session permit immediately, freeing hardware resources for waiting jobs. It executes corruption verification, runs fast SSIM pre-filtering, and performs stratified segment VMAF scoring. If verification fails, a single retry with higher-quality rate control parameters is attempted. If verification fails a second time, the job is marked as quality-failed and flagged for review.

5. Promotion or Cleanup: On successful verification, the staged file is promoted to the destination using atomic rename or validated cross-device streaming copy. Staging workspaces are unlinked. On failure, temporary files are cleared, and the failure status is sent to Module A via the write actor.

### Error Handling and Recovery Strategy

- Hardware encoding failures (driver crash, out-of-memory, NVENC session limit reached) cause the transcoder to release its GPU permit, mark the hardware profile as degraded, and seamlessly retry the transcode using CPU software encoding.

- VMAF quality failures trigger a single automated retry with adjusted quality parameters (e.g. lower CRF/CQ value and slower preset speed). If quality remains below threshold, the job is preserved in a quality-failed state for administrative review.

- Cross-device copy interruptions or disk-full errors on destination abort promotion, leave original source files untouched, preserve the staged file for diagnostic inspection, and log a high-severity error.

- Transcoding timeouts terminate child processes cleanly via process group cancellation and requeue the job with exponential backoff.

### Integration Points

- Module A (Persistence Engine): Receives transcode completion, quality metrics, and failure status updates via the single-writer actor channel.

- Module C (Evaluation Pipeline): Provides Decision Records, encoding presets, and explicit stream mapping specifications.

- Module E (Concurrency Engine): Dispatches transcode tasks and coordinates GPU session permits via resource semaphores.

- Cross-Cutting (Configuration Management): Reads hardware preferences, VMAF sampling thresholds, presets, and timeout rules.

- Cross-Cutting (Observability): Publishes encode durations, segment VMAF scores, GPU session utilization, and transcode throughput metrics.

### Trade-offs and Design Justification

Stratified segment sampling was chosen over full-file VMAF because full-file verification introduces an unsustainable 50-70% processing time penalty per file, creating a massive throughput bottleneck for large libraries; stratified sampling delivers near-identical quality governance in a fraction of the time. The multi-tiered file promotion strategy accommodates realistic storage architectures where fast local NVMe SSDs are used for staging while media libraries reside on network-attached storage (NAS) or separate disk arrays, overcoming operating system `EXDEV` cross-device link limitations without risking destination corruption. Coordinating GPU session permits via concurrency semaphores prevents driver crashes and hardware session rejection on consumer GPUs, ensuring deterministic hardware utilization without overloading video memory. External CLI process invocation isolates media processing crashes from the core Go runtime, maintaining the zero-CGO constraint.

---

## Module E: Concurrency Engine

### Primary Responsibility

Orchestrate the concurrent processing of transcoding jobs across a configurable pool of workers, implementing priority-based task routing, resource-weighted concurrency control (with dedicated GPU session semaphores to protect hardware encoders), graceful worker lifecycle management, and retry logic with exponential backoff. The concurrency engine acts as the operational orchestrator, utilizing a bounded in-memory prefetch buffer that continuously draws from the single authoritative persistent SQLite queue in Module A, enforcing cancellation through context propagation, and executing an orderly drain phase during shutdown.

### Core Data Structures

- Task Prefetch Queue: A bounded in-memory priority buffer that holds pre-leased work items ordered by priority level and submission time. The prefetch queue continuously leases the highest-priority pending items from Module A's authoritative SQLite `Queue Entries` table, eliminating the dual-queue split-brain hazard while providing sub-millisecond dispatch to idle workers. The queue carries a configurable buffer depth that applies backpressure upstream when saturated.

- Worker Pool and Resource Semaphores: A collection of active and idle worker goroutines, governed by dual concurrency controls:
  1. General Worker Pool: Manages worker states (idle, processing, draining, stopped), worker cancellation contexts, and processing statistics.
  2. Resource-Weighted Semaphores: Workers dispatching transcode jobs must acquire permits from a dedicated `GPUWorkerSemaphore` (dimensioned strictly by hardware capability profile, e.g. 2 to 4 concurrent NVENC/QSV sessions) before attempting hardware acceleration, while CPU-intensive tasks draw from a `CPUWorkerSemaphore` dimensioned by available CPU cores. This prevents GPU driver rejections, out-of-memory panics, and thread thrashing.

- Routing Table: A mapping of evaluation outcomes to worker dispatch strategies. After a file is evaluated, the routing table determines whether the work item should be dispatched to a transcoder worker, sent for notification processing, or marked complete. The routing table is configured by the evaluation pipeline's decision outcomes.

- Retry Record: Metadata attached to queue entries that have failed processing, tracking the retry count, next scheduled retry time based on exponential backoff, the failure reason, and whether the maximum retry limit has been reached.

- Batch Splitter: A mechanism for decomposing large batch scanning results into staged pipeline waves. The batch splitter partitions large filesystem discovery batches into balanced sub-batches across the worker pool, coordinating dependencies between sequential lifecycle phases (evaluation phase $\rightarrow$ transcode phase $\rightarrow$ verification phase) while preserving parent priority inheritance and emitting unified scan progress metrics.

### Key Interface Contracts

- Task Ingestion and Persistence Enqueue: Accepts work items from the filesystem engine (discovered file records) and persists them immediately to Module A's `Queue Entries` table via the single-writer actor channel. Ingestion items are assigned a priority level and job type (evaluate, transcode, notify). When the persistent queue or in-memory prefetch buffer reaches capacity limits, upstream ingestion blocks, enforcing backpressure.

- Priority Prefetch and Lease: The concurrency engine maintains a continuous background prefetch loop that queries Module A's read pool for the highest-priority `pending` entries and atomically transitions them to `leased` via the write actor. Leased items populate the in-memory prefetch queue. Workers claim items from this buffer instantaneously without encountering database lock contention.

- Resource-Aware Worker Dispatch: Routes claimed items to target modules based on job type:
  - Evaluation items dispatch to Module C without requiring GPU permits.
  - Transcode items inspect the preset: if hardware acceleration is specified, the worker acquires a permit from the `GPUWorkerSemaphore`. If all GPU permits are active, the worker either waits for an available permit or falls back to software encoding per configuration, then dispatches to Module D.
  - Notification items dispatch to Module F.
  The dispatch creates an isolated cancellation context tied to the worker and OS process supervisor.

- Result Collection: Collects the outcome from dispatched workers. On success, the worker releases any acquired GPU semaphores and submits completion status to Module A's write actor. The routing table enqueues follow-up tasks (e.g. evaluation success triggers transcode staging). On failure, the retry logic schedules exponential backoff or marks the entry as permanently failed.

- Backpressure Enforcement: When the prefetch buffer or persistent queue reaches capacity, new task submissions from the filesystem engine are paused. Upstream discovery workers block on ingestion channel writes, preventing memory bloat.

- Worker Lifecycle and Drain Phase: Initializes worker pools with individual cancellation contexts. During graceful shutdown, the engine transitions to a drain phase: the prefetch loop stops, in-flight jobs complete within a configured drain timeout, and GPU/CPU semaphores are cleanly released.

### State Flow and Lifecycle

The concurrency engine lifecycle follows these phases:

1. Initialization: The engine initializes worker pools and resource semaphores (GPU and CPU pools), launches the prefetch synchronization loop against Module A's persistent database, and establishes routing tables.

2. Processing: The prefetch loop leases pending jobs from SQLite and feeds the in-memory buffer. Workers pull items from the buffer, acquire appropriate resource semaphores, and dispatch to processing modules. Results flow back to the write actor in Module A.

3. Retry Handling: When transient failures occur, the job's retry count is incremented, and its next execution timestamp is calculated using exponential backoff with jitter. The state is updated in SQLite via the write actor.

4. Drain Phase: During graceful shutdown, the engine stops prefetching, halts new ingestion, notifies workers to drain current tasks, and waits for active transcodes and evaluations to complete up to the drain timeout. Operations exceeding the timeout are canceled via context, safely terminating child processes.

5. Shutdown: After workers stop and semaphores release, final metrics are flushed, and shutdown completion is signaled to the shutdown coordinator.

### Error Handling and Recovery Strategy

- Queue capacity overflow: When the task prefetch queue is full, the filesystem engine ingestion buffer pauses traversal, preventing memory exhaustion.

- Worker crashes: If a worker panics or crashes, its assigned job's lease expires. The crash recovery routine resets expired leased jobs to `pending` with an incremented retry count, and a new worker goroutine is spawned automatically.

- GPU session exhaustion: If a hardware acceleration session fails during transcode dispatch, the worker releases its GPU permit and immediately falls back to software encoding under the CPU semaphore.

- Timeout handling: Jobs exceeding configured execution timeouts trigger parent context cancellation. The child process supervisor terminates the underlying ffmpeg or ffprobe process cleanly, and the job enters exponential backoff retry.

### Integration Points

- Module A (Persistence Engine): The authoritative backing store. Module E prefetches pending jobs from Module A and submits lease, progress, completion, and retry updates to the write actor.

- Module B (Filesystem Engine): Pushes discovered file records into Module E's ingestion interface.

- Module C (Evaluation Pipeline): Receives evaluation task dispatches and returns normalized metadata and decision records.

- Module D (Transcoder): Receives transcode task dispatches; worker dispatches are regulated by Module E's `GPUWorkerSemaphore`.

- Module F (Notification Engine): Receives operational event dispatches for external delivery.

- Cross-Cutting (Configuration Management): Reads worker pool sizes, GPU semaphore capacities, backoff settings, and drain timeouts.

- Cross-Cutting (Observability): Publishes queue depth, GPU/CPU semaphore utilization, worker idle/busy ratios, and backpressure metrics.

### Trade-offs and Design Justification

A hybrid queue architecture (authoritative SQLite persistence paired with an in-memory prefetch buffer) eliminates the dual-queue split-brain problem and ensures complete crash durability while delivering sub-millisecond dispatch latency to workers. Resource-weighted concurrency semaphores address a critical real-world failure mode in media transcoding: consumer GPU hardware acceleration limits (such as NVENC concurrent session caps and VRAM ceilings) that inevitably crash or reject encodes when treated as unbounded generic workers. Dimensioning separate GPU and CPU worker semaphores guarantees maximum hardware saturation without risking out-of-memory conditions or hardware driver instability. Simplifying the batch splitter to coordinate multi-phase pipeline waves rather than attempting complex asynchronous per-stream media file chunking avoids massive container demuxing and concatenation complexity without sacrificing throughput.

---

## Module F: Notification Engine

### Primary Responsibility

Deliver operational notifications to external communication channels through a pluggable adapter architecture, providing real-time visibility into system events such as job completions, quality failures, scan progress, and system errors. The notification engine batches events, enforces rate limits per channel, tracks delivery acknowledgments, and implements dead-letter handling for failed deliveries.

### Core Data Structures

- Event: The atomic unit of notification, containing an event type (job completed, job failed, scan started, scan completed, quality threshold exceeded, system error), event severity (info, warning, error, critical), a timestamp, contextual data relevant to the event type, and a unique event identifier. Events are the input to the notification system.

- Channel Adapter: A pluggable delivery mechanism for a specific notification platform. Each adapter implements a standardized delivery interface and carries configuration for its platform (webhook URL, API credentials, channel identifier). Adapters are registered at startup and selected by the notification routing logic.

- Message Batch: A collection of events grouped for batched delivery to a channel adapter. Batches are sized by count or age threshold, whichever comes first. Batching reduces per-message overhead for platforms that support bulk delivery and reduces the number of API calls to notification services.

- Delivery Record: A per-event record tracking the delivery status, the target channel adapter used, the delivery timestamp, the response from the notification platform, and any error encountered during delivery. Delivery records are appended to the audit log for accountability.

### Key Interface Contracts

- Adapter Registration: Registers a channel adapter by type (Telegram, Discord, Slack, Gotify, SMTP) with its configuration parameters. Each adapter validates its configuration at registration time and reports registration success or failure. Registered adapters are stored in a concurrency-controlled adapter registry that the delivery router consults to select the appropriate adapter.

- Event Routing: Accepts an event and routes it to the configured notification channels based on event type and severity. Routing rules are configured per event type, specifying which channel adapters should receive the event and whether the delivery should be immediate or batched. Events that do not match any routing rule are silently discarded.

- Message Batching: Collects events into batches for batched delivery. The batching logic groups events by target channel and flushes batches when either the batch size threshold is reached or the batch age threshold expires. Immediate delivery events bypass batching and are delivered synchronously.

- Rate Limiting: Enforces per-channel rate limits by tracking the number of delivery attempts per time window. When a channel adapter is rate-limited, subsequent events for that channel are queued with a delay until the rate limit window expires. The rate limit configuration is sourced from the centralized settings store and can be updated via hot-reload.

- Delivery Acknowledgment: Tracks the delivery status of each event. On successful delivery, the delivery record is marked as delivered with the platform response. On delivery failure, the delivery record is marked as failed with the error detail. Failed deliveries enter the dead-letter queue.

- Dead-Letter Queue: A holding area for events that have failed delivery after exhausting the configured retry attempts. Dead-lettered events are logged with full error context and can be replayed manually or automatically after a configurable delay. The dead-letter queue is stored in the persistence engine for durability.

### State Flow and Lifecycle

The notification engine lifecycle follows these phases:

1. Initialization: The engine loads notification channel configurations from the centralized settings store, registers each configured adapter, validates adapter connectivity where possible, and starts the batching and delivery dispatch loops.

2. Event Processing: Events are received from the concurrency engine and other system components. Each event is routed to the appropriate channel adapters based on routing rules. Immediate events are dispatched synchronously; batched events are collected into batches.

3. Batch Dispatch: Batched events are dispatched to their target channel adapters in chronological order. The dispatch respects rate limits by queuing delayed dispatches when a channel is rate-limited. Delivery results are recorded in delivery records.

4. Retry and Dead-Letter: Events that fail delivery are retried with exponential backoff up to a configured maximum. Events that exhaust retries are moved to the dead-letter queue and logged for manual intervention.

5. Shutdown: During graceful shutdown, the engine flushes all pending batches, waits for in-flight deliveries to complete (up to a configured flush timeout), and records the final delivery statistics. Pending events that have not been dispatched are either delivered during the flush period or moved to the dead-letter queue.

### Error Handling and Recovery Strategy

- Adapter connectivity failures: When a channel adapter reports a connectivity error (network unreachable, authentication failure), the adapter is marked as unavailable and all subsequent events for that adapter are queued for retry. The adapter is retried after a configurable interval. Connectivity failures do not affect delivery to other adapters.

- Rate limit errors: When a channel adapter returns a rate limit response, subsequent events for that adapter are delayed until the rate limit window expires. The engine tracks rate limit headers from platform responses to handle platform-specific rate limit behavior.

- Delivery timeouts: When a delivery request exceeds the configured timeout, the delivery is marked as failed and enters the retry cycle. The timeout is configured per adapter type to accommodate platform-specific response time characteristics.

- Dead-letter exhaustion: The dead-letter queue is stored in the persistence engine and is periodically reviewed by a maintenance routine. Events in the dead-letter queue older than a configurable retention period are purged after logging.

### Integration Points

- Module E (Concurrency Engine): Receives operational events from the concurrency engine for job completions, failures, and system lifecycle events. The concurrency engine creates notification events with job context.

- Module A (Persistence Engine): Stores delivery records and the dead-letter queue. Reads notification channel configurations from system settings.

- Cross-Cutting (Configuration Management): Loads notification channel configurations, routing rules, rate limits, and retry policies from centralized settings. Supports hot-reload of notification configuration.

- Cross-Cutting (Observability): Publishes delivery success and failure rates, batch sizes, delivery latency, and channel adapter health metrics.

### Trade-offs and Design Justification

Constraints addressed: rate limiting via per-channel rate limiters with retry delays, delivery guarantees via at-least-once retry with exponential backoff and dead-letter queue preservation.

The pluggable adapter architecture provides extensibility without modifying core notification logic, allowing new notification platforms to be added by implementing a single adapter interface. Batching is used to reduce the number of API calls to notification platforms, which reduces both network overhead and the risk of rate limit violations. Per-channel rate limiting ensures that one slow or rate-limited channel does not block deliveries to other channels. The dead-letter queue provides a safety net for delivery failures, ensuring that no notification is silently lost. Storing delivery records in the persistence engine provides auditability at the cost of additional database write overhead, which is considered acceptable given that delivery tracking is a critical operational requirement.

---

## Module G: CI/CD Pipeline - GitHub Actions

### Primary Responsibility

Define and configure a GitHub Actions continuous integration and continuous deployment pipeline for building, testing, signing, and releasing MediaCruncher across multiple platforms and architectures. The pipeline produces cryptographically signed build artifacts, publishes them as GitHub Releases, and enforces quality gates through matrix builds and status checks.

### Core Data Structures

- Build Matrix: A structured configuration of build targets, combining operating systems (Linux, Windows, macOS) with CPU architectures (x86_64, ARM64). Each matrix combination specifies the target OS, architecture, Go version, and platform-specific build flags. The matrix is generated dynamically based on supported platform combinations.

- Build Artifact: The output of a successful build, including the compiled binary, associated checksums, cryptographic signatures, and platform identification metadata. Artifacts are uploaded as GitHub Actions artifacts during the build workflow and published as GitHub Release assets during the release workflow.

- Semantic Version Tag: A version string following semantic versioning conventions (major.minor.patch) with optional pre-release and build metadata. Version tags are created during the release process and are used to name GitHub Releases and to scope the changelog included in release notes.

- Release Draft: A pre-published GitHub Release containing the build artifacts for all platforms, release notes generated from commit history since the previous release, cryptographic signature verification instructions, and download links for each platform-architecture combination.

### Key Interface Contracts

- Matrix Build Strategy: Defines the build matrix configuration specifying all supported operating system and architecture combinations. Each matrix job runs in an isolated GitHub Actions runner environment, checks out the repository source code, caches Go module dependencies, runs the test suite, and builds the platform-specific binary. Build jobs are parametrized by operating system and architecture.

- Artifact Generation: After a successful build, each matrix job produces a platform-specific binary with embedded version information (major version, minor version, patch version, commit hash, build timestamp). The artifact includes the binary, a SHA-256 checksum file, and a detached cryptographic signature file.

- Cryptographic Signing: After all matrix build jobs complete successfully, a signing job downloads all platform artifacts, verifies their checksums, signs each binary with an ECDSA private key, and uploads the signed artifacts as workflow artifacts. The signing key is sourced from encrypted repository secrets.

- Semantic Version Tagging: During the release workflow, a version job computes the next semantic version based on the previous release tag and generates a version file. A tag job creates the annotated git tag for the release and pushes it to the repository. The version computation follows semantic versioning conventions with automatic patch increment for non-breaking changes.

- GitHub Release Publishing: After successful builds and signing, a release job creates a draft GitHub Release with the generated version tag, includes release notes compiled from commit messages since the previous release, attaches all signed artifacts for each platform-architecture combination, and includes download instructions and signature verification steps in the release body.

- Artifact Caching: GitHub Actions caching is used to cache package dependencies between workflow runs, significantly reducing build times for successive runs on the same or related branches. The cache key is derived from the lock file hash to ensure cache validity.

- Artifact Retention: Build artifacts uploaded as GitHub Actions workflow artifacts are retained for a configurable number of days (default thirty days). GitHub Release assets are retained indefinitely as part of the release history.

### State Flow and Lifecycle

The CI/CD pipeline lifecycle for a GitHub Actions workflow follows these phases:

1. Trigger: The workflow is triggered by a push event (for build validation on all branches) or a release event (for release publishing on tagged commits). Push triggers run the full build matrix and test suite. Release triggers run the build matrix, signing, and release publishing sequence.

2. Matrix Build: Each matrix combination runs as an independent GitHub Actions job. Jobs run in parallel when possible and share the repository checkout. Each job validates the build environment, installs dependencies, runs tests, and produces a platform-specific binary artifact.

3. Artifact Verification: After all matrix builds complete, a verification job downloads all artifacts and runs a cross-platform validation suite. The validation suite checks binary executability, version string correctness, and platform-specific behavior.

4. Signing: The signing job downloads verified artifacts, signs each binary, and uploads signed artifacts. Signing failure causes the release workflow to abort, with the unsigned artifacts preserved for debugging.

5. Release Publishing: The release job creates the GitHub Release with all signed artifacts, release notes, and verification instructions. The release is published as a draft for review before final publication.

### Error Handling and Recovery Strategy

- Build failures: A failure in any matrix job marks the workflow run as failed. Individual matrix failures do not block other matrix jobs from completing, allowing the pipeline to gather maximum diagnostic information about which platforms are affected.

- Signing failures: Signing is a critical security step. If signing fails, the release workflow is aborted and the failure is reported as a high-severity event. The release is not published. The signing key and environment are inspected before retrying.

- Test failures: Test failures in the matrix build stage block the release workflow. The failing platform-architecture combination is reported with test output and failure details. The pipeline does not proceed to signing or release when tests fail.

- Release conflicts: If a release with the target version tag already exists, the release job aborts with a conflict error. The pipeline requires manual resolution of the version conflict before proceeding.

### Integration Points

- Module H (CI/CD Pipeline - GitLab CI/CD): Provides a parallel CI/CD pipeline for GitLab CI/CD. The pipeline designs in Modules G and H are coordinated to maintain feature parity and consistent build outputs across platforms.

- Cross-Cutting (Configuration Management): Reads build configuration, signing key references, version computation rules, and artifact retention settings from centralized configuration.

- Cross-Cutting (Observability): Publishes build duration, success rates by platform, artifact size metrics, and release event metrics.

### Trade-offs and Design Justification

GitHub Actions was selected as the primary CI/CD platform for its native GitHub integration, which provides seamless release publishing, artifact hosting, and workflow triggering without external tooling. The matrix build strategy ensures comprehensive platform coverage while keeping individual build jobs isolated for reliable failure diagnosis. Cryptographic signing is performed within the CI/CD pipeline using GitHub Actions secrets, keeping signing keys out of the repository while allowing automated release workflows. Draft release publishing enables human review before assets are publicly available, providing a safety net for release errors. The separation of the build matrix (push trigger) and release workflow (release trigger) enables rapid build validation on every commit while maintaining a controlled release process.

---

## Module H: CI/CD Pipeline - GitLab CI/CD

### Primary Responsibility

Define and configure a GitLab CI/CD continuous integration and continuous deployment pipeline that mirrors the functionality of Module G (GitHub Actions) but leverages GitLab-specific features including runner groups, CI/CD variables, the GitLab Packages registry, pipeline variables, and approval gates. The pipeline provides an alternative CI/CD platform for deployments where GitHub Actions is not available or where GitLab-specific features are preferred.

### Core Data Structures

- Pipeline Matrix: A structured configuration parallel to the build matrix in Module G, defining the same operating system and architecture combinations but implemented using GitLab CI/CD job definitions and rules. The matrix maps to GitLab CI/CD stages and job definitions with GitLab-specific syntax and features.

- Runner Configuration: GitLab-specific runner assignments and constraints, including runner groups, runner tags, runner concurrency limits, and runner environment variables. Runners are configured to match the target platform of each matrix job (Linux runners for Linux builds, Windows runners for Windows builds, macOS runners for macOS builds).

- Pipeline Variable Set: A collection of environment variables scoped to specific pipeline stages, including build configuration variables, platform-specific flags, signing configuration, and test parameters. Variables are sourced from GitLab CI/CD variables (project-level or group-level) and pipeline-level overrides.

- Package Registry Asset: Build artifacts published to the GitLab Packages and Registries feature, which provides versioned artifact storage integrated with the GitLab project. Package registry assets are versioned with semantic version tags and are accessible via the GitLab package API.

### Key Interface Contracts

- Pipeline Stage Definition: Defines GitLab CI/CD pipeline stages in the correct execution order: validate, build, test, verify, sign, and release. Each stage contains one or more jobs that run in the specified order. Jobs within a stage can run in parallel when runner capacity is available.

- Matrix Job Definition: Defines matrix jobs using GitLab CI/CD dynamic job generation or static job definitions for each platform-architecture combination. Each matrix job checks out the repository, installs dependencies, runs tests, and builds the platform-specific binary. Matrix jobs are parametrized by platform and architecture using GitLab CI/CD variables.

- Runner Assignment: Assigns matrix jobs to runners based on runner tags and runner groups. Linux x86_64 jobs are assigned to Linux x86_64 runners, Windows jobs to Windows runners, and so on. The runner assignment ensures platform-specific builds run on the correct operating system environment.

- Package Upload: After successful builds, artifacts are uploaded to the GitLab Packages registry with semantic version metadata. Package uploads are versioned, auditable, and accessible through the GitLab package API for downstream consumption.

- Approval Gates: Release stage jobs include approval gate requirements that require manual approval before the release can proceed. Approval gates are configured at the GitLab project level and can be assigned to specific maintainer roles. The approval gate provides a human checkpoint before release assets are published.

- Retry Policy: Build and test jobs include configurable retry policies with a maximum retry count and retry delay. Transient failures in matrix jobs are automatically retried up to the configured limit before the pipeline marks the job as failed.

- Pipeline Variables: Pipeline-level variables override project-level CI/CD variables for the current pipeline execution. Variables control the build version, target platforms, signing configuration, and test parameters. Variables are sourced from pipeline triggers, manual pipeline runs, or scheduled pipeline configurations.

### State Flow and Lifecycle

The GitLab CI/CD pipeline lifecycle follows these phases:

1. Trigger: The pipeline is triggered by a push to the repository (continuous integration), a tag creation (release pipeline), or a manual pipeline run. Push triggers run the validate, build, test, and verify stages. Tag triggers run the full pipeline including signing and release.

2. Matrix Build: Each platform-architecture combination runs as an independent GitLab CI/CD job within the build stage. Jobs are assigned to runners based on runner tags. All matrix jobs run in parallel within the build stage.

3. Verification: After the build stage completes, the verify stage runs cross-platform validation against all produced artifacts, verifying executability, version strings, and platform-specific behavior.

4. Signing: The sign stage downloads all verified artifacts, signs each binary, and uploads signed artifacts to the GitLab Packages registry.

5. Release: The release stage creates the GitLab release with all signed artifacts, with an approval gate requiring manual maintainer approval before final publication. Release notes are generated from commit history and included in the release.

### Error Handling and Recovery Strategy

- Pipeline failures: A failure in any pipeline stage blocks subsequent stages. Individual matrix job failures do not block other matrix jobs in the same stage, allowing maximum diagnostic information to be gathered.

- Approval gate failures: If the approval gate is not approved within a configurable timeout, the release pipeline is paused and a notification is sent to the designated approvers. The pipeline remains in a paused state until approval or explicit rejection.

- Runner capacity exhaustion: If no runners are available for a required platform, the pipeline job is queued indefinitely until a runner becomes available. The pipeline configuration includes a job timeout that eventually cancels the job if runners are unavailable for an extended period.

- Package registry conflicts: If a package version already exists in the GitLab Packages registry, the upload job aborts with a conflict error. The pipeline requires manual resolution before proceeding.

### Integration Points

- Module G (CI/CD Pipeline - GitHub Actions): Provides a parallel CI/CD implementation for GitHub Actions. Both modules maintain feature parity and produce the same build outputs, with platform-specific differences only in CI/CD platform integration details.

- Module F (Notification Engine): Receives pipeline status notifications (build success, build failure, release published, release failed) for delivery to configured notification channels.

- Cross-Cutting (Configuration Management): Reads pipeline configuration, runner assignments, package registry settings, and approval gate configurations from centralized configuration.

- Cross-Cutting (Observability): Publishes pipeline duration, stage completion rates, runner utilization, and release event metrics.

### Trade-offs and Design Justification

Module G (GitHub Actions) serves as the primary canonical CI/CD pipeline for open distribution, automated version tagging, draft release publishing, and upstream public release assets. Module H (GitLab CI/CD) is designed as a parallel enterprise mirror for self-hosted environments or internal air-gapped corporate deployments where GitLab runner groups, project approval gates, and integrated package registries are required. The pipeline stage structure mirrors GitHub Actions' workflow job structure, maintaining conceptual consistency across both implementations while allowing teams to maintain a single core codebase across both public GitHub and private GitLab infrastructure.

---

## Cross-Cutting Concern: Configuration Management

### Primary Responsibility

Provide a unified, hierarchical configuration loading mechanism that consolidates settings from multiple sources into a single authoritative configuration store, with validation, cross-platform path normalization, hot-reload capability, and centralized access for all modules.

### Design

Each configuration snapshot is immutable once published. Configuration values are loaded in a defined priority order: built-in defaults are applied first, then overridden by environment variables, then by command-line flags, and finally by a configuration file on disk. The last writer wins within each priority layer. Values are validated against a schema defined at startup; invalid values cause a startup failure with a detailed error listing each invalid field and its expected type and constraints. The configuration store is read through a concurrency-safe accessor that returns copies of configuration values.

Cross-platform path normalization is enforced across all configured paths: Windows backslashes are converted to uniform internal separators, extended-length path prefixes (`\\?\`) are applied transparently on Windows to overcome 260-character `MAX_PATH` limitations, and Universal Naming Convention (UNC) paths (`\\server\share`) are validated for network storage targets.

Hot-reload capability allows specific configuration sections (notification channels, rate limits, scan paths, worker pool sizing, GPU session semaphore limits) to be reloaded without restarting the application. When a configuration file change is detected on disk, the engine loads the new configuration, validates it, and atomically replaces the active configuration snapshot. Modules that depend on hot-reloadable settings subscribe to configuration change notifications and react to updates (e.g., the concurrency engine adjusts its worker pool or GPU semaphore limits, the notification engine reloads channel adapters). Settings that cannot be hot-reloaded (database connection string, log level during initialization) require a full application restart.

All modules access configuration through a shared configuration accessor interface that provides typed getters for each configuration parameter. The accessor returns a default value when a configuration key is unset, ensuring that modules never encounter nil or missing configuration. Configuration changes are logged to the audit trail with the changed keys and old/new values.

### Integration

All modules read their configuration from the centralized configuration accessor. The configuration store is a dependency injected into each module at construction time. Module-specific configuration sections are namespaced within the overall configuration hierarchy (e.g., persistence section for Module A, filesystem section for Module B, concurrency section for Module E).

---

## Cross-Cutting Concern: Observability

### Primary Responsibility

Provide structured logging, metrics collection, health check reporting, and audit trail integration that apply across all modules, enabling operational visibility, performance monitoring, and debugging support.

### Design

Structured logging is the primary observational mechanism, with each module emitting log entries at defined severity levels (debug, info, warning, error, critical). Log entries include a structured context block containing the module name, operation identifier, relevant identifiers (job ID, file path, worker ID), and a human-readable message. Log entries are written to stdout in a structured format (JSON) that can be consumed by log aggregation systems. The log level is configurable and hot-reloadable.

Metrics are collected through a centralized metrics registry that exposes counters, gauges, and histograms. Specific metrics collected include:

- Queue depth: current number of pending, leased, processing, and completed queue entries (reported by Module E and persisted by Module A).
- Worker and semaphore utilization: percentage of active vs. idle CPU workers, and active vs. available GPU session permits (reported by Module E).
- Success and failure rates: ratio of successful to failed operations per module per time window (reported by each module).
- VMAF scores: distribution of stratified segment VMAF scores and SSIM pre-filter results (reported by Module D).
- Scan progress: files discovered, files accepted, files skipped, duplicate counts, scan duration (reported by Module B).
- Evaluation results: decision distribution (transcode, stream copy, skip, flag for review counts) (reported by Module C).
- Notification delivery: delivery success and failure rates, batch sizes, delivery latency (reported by Module F).
- Build metrics: build duration, success rates by platform, artifact sizes (reported by Modules G and H).

Metrics are exported in a standard format compatible with Prometheus and are exposed on a local HTTP endpoint for scraping. A health check endpoint reports the overall system health status, including the state of each module (healthy, degraded, unhealthy), queue depth, write actor buffer utilization, and active worker count.

The audit trail, implemented by Module A's audit log, captures significant operational events across all modules. Events are appended with structured metadata enabling post-incident investigation and compliance reporting.

---

## Cross-Cutting Concern: Graceful Shutdown

### Primary Responsibility

Define and enforce a deterministic shutdown sequence that ensures all in-progress work is completed or safely rolled back, persistent state is flushed, external connections are cleanly terminated, and child processes are pruned before the application exits.

### Design

The shutdown sequence is triggered by a signal (SIGTERM on Unix, Ctrl+C or system shutdown on Windows) and follows a strict ordered sequence:

1. Signal Interception: A signal handler intercepts shutdown signals and initiates the shutdown sequence. The handler ignores subsequent signals during the shutdown process to prevent duplicate shutdown attempts. The application logs the shutdown signal and its timestamp.

2. Worker Draining and Ingestion Halt: The concurrency engine enters the drain phase. The prefetch loop stops, and new task submissions from the filesystem engine are rejected. Existing workers complete their current assignments. The engine waits for all in-flight operations to complete, up to a configured drain timeout. Operations that exceed the drain timeout are canceled via their cancellation context, triggering immediate termination of child processes via the process supervisor.

3. State Persistence: The concurrency engine flushes all pending state changes to the persistence engine's write actor channel. The filesystem engine flushes any pending scan checkpoints. The notification engine flushes pending delivery records. The persistence engine's single-writer actor commits all remaining transactions synchronously to SQLite.

4. Connection Teardown: The persistence engine closes all read connections and the single write connection. The notification engine closes all adapter connections. External process handles (ffprobe, ffmpeg, VMAF) are terminated cleanly.

5. Final Reporting: The observability subsystem exports final metrics and writes a shutdown summary to the audit log. The application exits with a zero exit code if shutdown completed normally, or a non-zero exit code if a timeout or error prevented clean shutdown.

The shutdown sequence is designed to be idempotent: calling it multiple times (e.g., from multiple signal handlers) has no adverse effect. The shutdown coordinator tracks shutdown progress and reports the current phase, enabling diagnostics if shutdown hangs.

### Integration

The shutdown coordinator is a central component that all modules register with at startup. Each module registers a shutdown handler that is invoked during the appropriate phase (draining, persistence, teardown). The concurrency engine's drain phase is the longest phase and typically determines the overall shutdown duration. The persistence engine's synchronous commit during state persistence ensures that no queued work is lost on shutdown.

---

## Cross-Cutting Concern: Child Process Supervision and Zombie Prevention

### Primary Responsibility

Supervise and isolate all external CLI child processes (ffmpeg, ffprobe, VMAF verification) across POSIX and Windows operating systems, ensuring immediate process termination and cleanup upon context cancellation, worker timeout, or application termination to prevent orphaned zombie processes from consuming CPU and GPU resources.

### Design

Because MediaCruncher delegates media analysis, transcoding, and quality scoring to external CLI binaries without CGO bindings, child process lifecycle management is a vital stability requirement:

1. Windows Job Objects: On Windows hosts, every external process invocation is assigned upon creation to a Windows Job Object configured with the `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` flag. If the worker context is canceled, the timeout expires, or the parent Go daemon crashes, the Windows kernel guarantees immediate, unconditional termination of the entire child process hierarchy.

2. POSIX Process Groups: On Linux and macOS hosts, processes are launched with `Setpgid: true` to establish their own independent process group. When a context cancellation or timeout occurs, termination signals (`SIGTERM` followed after a brief grace window by `SIGKILL`) are dispatched to the negative process group ID (`-PID`), ensuring intermediate child shells and media sub-processes are completely pruned without leaving orphaned processes.

3. Asynchronous Pipe Draining: Standard output and standard error from child processes are drained continuously using non-blocking goroutines with bounded circular memory buffers. This prevents external CLI tools from hanging indefinitely due to saturated operating system pipe buffers during long transcode runs.

### Integration

Modules C (Evaluation Pipeline) and D (Transcoder) execute all external process invocations through this unified supervisor. Context cancellations generated by Module E's worker lifecycles propagate directly through the supervisor to terminate active transcoding and probing immediately.

---

## Cross-Cutting Concern: Stateless Worker Design

### Primary Responsibility

Define the worker processing model as stateless where possible, enabling horizontal scalability through future gRPC or HTTP-based worker distribution without requiring shared mutable state between workers.

### Design

Workers are stateless with respect to processing logic: a worker receives a job, performs the required processing (evaluation or transcoding), and returns the result. Workers may maintain local caches for performance optimization (e.g., the evaluation pipeline caches the rule set in memory, the transcoder caches the hardware capability profile), but these caches are rebuildable from persistent sources without affecting processing correctness and can be discarded without state migration. The worker does not cache in-memory state about files, rules, or processing history that would tie it to a specific processing session. All persistent state is stored in the persistence engine (Module A), and all configuration is read from the centralized configuration store.

Stateless worker design enables future horizontal scaling where workers run as separate processes or containers communicating through a gRPC or HTTP interface. The queue remains the single source of truth for work items, workers claim jobs through the queue interface, and results are returned through the same interface. The stateless model ensures that worker instances are interchangeable and that no single worker becomes a point of failure for specific jobs.

### Integration

The stateless worker model is the default assumption for all module interfaces. Modules expose stateless operation contracts: the evaluation pipeline accepts a file path and returns a decision; the transcoder accepts a transcode job and returns a result; the notification engine accepts an event and delivers it. Modules do not expose stateful operation contracts that would require workers to maintain session state. The persistence engine and configuration store are the two shared stateful services that all workers depend on, providing the single source of truth for persistent data and configuration.

---

## Integration Map

This section summarizes the connectivity, data sharing, and communication patterns between all modules and cross-cutting concerns in the MediaCruncher architecture.

### Module Connectivity Overview

The system follows a pipeline architecture where data flows from filesystem ingestion through evaluation to transcoding, with the concurrency engine orchestrating the flow and the notification engine providing asynchronous event distribution.

- Module B (Filesystem Engine) is the entry point, discovering media files with two-tier deduplication and pushing them into the processing pipeline.
- Module E (Concurrency Engine) receives discovered files from Module B, stages them in persistent storage via Module A's write actor, prefetches pending jobs into an in-memory buffer, and dispatches them to Module C (Evaluation Pipeline) for analysis.
- Module C (Evaluation Pipeline) analyzes files, synthesizes multi-stream preservation mappings, and submits decisions to Module A via the write actor.
- Module E reads evaluation decisions from the prefetch buffer, acquires GPU permits via resource semaphores, and dispatches transcode jobs to Module D (Transcoder).
- Module D executes transcoding with hardware session limits, verifies quality using stratified segment VMAF sampling, promotes output across filesystem boundaries, and reports results back to Module E and Module A.
- Module E routes completion notifications to Module F (Notification Engine).
- Module A (Persistence Engine) underlies the entire system, providing durable state for all modules via concurrent read pools and a Single-Writer Actor.
- Modules G (GitHub CI/CD) and H (GitLab CI/CD) operate externally to the runtime pipeline, providing canonical release automation and enterprise mirrored distribution respectively.

### Data Sharing Patterns

- Persistent Queue with Prefetch Dispatch: Module A's SQLite `Queue Entries` table serves as the single authoritative state for all work items. Module E maintains a bounded in-memory prefetch buffer that continuously leases pending work items from Module A, providing sub-millisecond dispatch to workers while eliminating dual-queue split-brain risks.

- Single-Writer Actor Communication: All write mutations across all modules (queue enqueues, state transitions, metadata upserts, audit log records, delivery acknowledgments) are serialized through Module A's dedicated write actor channel, eliminating `SQLITE_BUSY` database lock contention.

- Resource Semaphore Governance: Module E regulates dispatch of transcode jobs to Module D using dedicated `GPUWorkerSemaphore` and `CPUWorkerSemaphore` tokens, preventing GPU VRAM exhaustion and hardware encoder session rejections.

- Audit-mediated communication: The audit log in Module A serves as a shared event history that can be queried by Module F for notification routing decisions and by external monitoring tools for system diagnostics.

### Communication Flow Sequence

1. Filesystem discovery (Module B) identifies media files using size-first boundary hashing and pushes records to the concurrency engine (Module E).
2. Module E persists new jobs to Module A via the single-writer actor channel.
3. The prefetch loop in Module E leases pending evaluation jobs and dispatches them to the evaluation pipeline (Module C).
4. Module C analyzes files, generates per-stream audio/subtitle preservation mapping directives, and submits decision records to Module A's write actor.
5. Module E prefetches transcode jobs, acquires a GPU session permit from the GPU semaphore (or selects CPU fallback), and dispatches work to the transcoder (Module D).
6. Module D executes ffmpeg transcoding in an isolated staging folder under child process supervision, runs stratified segment VMAF quality verification, releases the GPU semaphore permit, promotes output to the destination via atomic move or cross-device copy (`EXDEV`), and updates job state via Module A's write actor.
7. Module E routes transcoding results: success triggers notification events; failure triggers retry backoff or review notifications.
8. Module F delivers batched and rate-limited notifications to external channels (Telegram, Discord, Slack, etc.).
9. All modules log significant operational events to Module A's audit log.
10. CI/CD pipelines (Modules G and H) build, test, sign, and release the application independently of the runtime pipeline.

### Dependency Graph

- Module A is depended on by: B, C, D, E, F
- Module B is depended on by: E
- Module C is depended on by: E, D (indirect via stream mapping contracts)
- Module D is depended on by: E
- Module E is depended on by: B, C, D, F
- Module F is depended on by: E
- Module G is independent (canonical external CI/CD)
- Module H is independent (mirrored external CI/CD)
- Child Process Supervision is depended on by: C, D
- Configuration Management is depended on by: all modules (A through H)
- Observability is depended on by: all modules (A through F)
- Graceful Shutdown coordinates: E, A, F, and Child Process Supervision
- Stateless Worker Design applies to: E, C, D

