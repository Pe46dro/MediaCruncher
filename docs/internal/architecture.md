# MediaCruncher Architectural Specification

This document provides a production-grade, implementation-ready architectural specification for MediaCruncher, a high-performance, cross-platform Go media library transcoding and auditing application. All descriptions use plain natural language with no code, no pseudo-code, and no language-specific syntax.

---

## Module A: Persistence Engine

### Primary Responsibility

Provide durable, transactional storage for the application's queue entries, job metadata, system settings, and audit logs. The persistence engine serves as the single source of truth for all operational state, enabling recovery across restarts and supporting concurrent read-write access from the concurrency engine. It must operate without any C bindings or CGO dependencies, relying exclusively on a pure-Go database driver to maintain build portability across platforms.

### Core Data Structures

The persistence layer manages four logical tables, each representing a distinct domain entity:

- Queue Entries: Records pending transcoding jobs with fields for source file path, current processing state, assigned worker identifier, priority level, retry count, creation timestamp, and scheduled execution time. Each entry carries a unique surrogate key and supports indexed lookups by state, priority, and worker assignment.

- Job Metadata: Stores the full analysis results produced by the evaluation pipeline for each media file. This includes extracted video and audio codec information, resolution, frame rate, bit rate, duration, language tracks, subtitle streams, and the decision rendered by the rule-matching engine. Metadata entries are linked to their corresponding queue entry via a foreign key relationship.

- System Settings: Holds configurable application parameters that persist across restarts. This includes transcoding presets, quality thresholds, worker pool sizing, rate limit configurations, notification channel settings, and scanned path registries. Settings are keyed by a unique identifier and carry a version stamp to support optimistic concurrency control.

- Audit Logs: Append-only records of significant operational events: job submissions, state transitions, transcoding completions, quality failures, notification deliveries, and system lifecycle events. Logs carry a monotonically increasing sequence number, timestamp, event type, severity level, and a structured payload containing contextual details.

### Key Interface Contracts

The persistence engine exposes a connection-oriented interface with the following operational boundaries:

- Connection Lifecycle: Opens a database connection from a provided file path, executes a controlled schema migration if needed, and validates the connection through a lightweight ping operation. Closing the connection drains pending transactions, releases the database file lock, and returns a cleanup error if any operation was left incomplete.

- Transaction Boundary: All write operations execute within explicitly scoped transactions that follow an all-or-nothing guarantee. The interface provides atomic begin-commit and rollback semantics, with automatic rollback triggered by unhandled error recovery within the transaction scope.

- Queue Operations: Enqueue a new job with state initialization and timestamp setting. Claim the highest-priority available job for a given worker, atomically transitioning it from pending to processing state while recording the worker identifier. Complete or fail a job by transitioning it to the target state with metadata linkage. Requeue a job with incremented retry count and backoff delay when transient failures occur.

- Metadata Operations: Upsert job metadata using the queue entry identifier as the foreign key, creating a new record or updating an existing one. Retrieve full metadata for a given job, or bulk-query metadata across a set of job identifiers for reporting purposes.

- Settings Operations: Load all system settings into a configuration map on startup. Update individual settings by key, with optimistic concurrency enforced through a version column that rejects stale writes. Reload settings on demand to support hot-reload capability.

- Audit Operations: Append a single audit log entry or bulk-insert multiple entries within a single transaction. Purge old entries beyond a configurable retention period, retaining a minimum number of recent entries for debugging purposes. Query entries by event type, severity, time range, or job identifier.

### State Flow and Lifecycle

The persistence engine lifecycle follows these phases:

1. Initialization: On application startup, the engine opens the database connection, runs schema migrations if the version is behind, and validates that all required tables and indexes exist. The migration system maintains a version history table and applies incremental changes in order, each migration being a self-contained transaction.

2. Runtime: During normal operation, the engine serves concurrent read requests through shared read connections and exclusive write requests through a single write connection pool. Queue claim operations are serialized through a write lock to prevent duplicate worker assignments. Metadata writes are batched where possible to reduce transaction overhead.

3. Shutdown: During graceful shutdown, the engine receives a drain notification that prevents new enqueue operations. Pending transactions complete, the connection pool is drained, and the database connection is closed cleanly. The application waits for the shutdown signal before invoking cleanup.

4. Recovery: On startup after an unclean shutdown, the engine identifies any queue entries left in the processing state and transitions them back to pending, incrementing their retry count. The audit log provides a complete history for diagnosing what caused the interruption.

### Error Handling and Recovery Strategy

The persistence engine employs a tiered error handling approach:

- Connection-level failures (database file locked, disk full, file not found) are treated as fatal errors that trigger immediate application shutdown, since no queued work can be processed without persistent state.

- Transaction-level failures (constraint violations, deadlock detection, serialization conflicts) are retried with exponential backoff up to a configurable limit. Deadlock scenarios are detected automatically by the database engine and retried on a different transaction path. After exhausting retries, the failing operation is logged to the audit trail with full error context, and the queue entry is reset to pending state.

- Migration failures are treated as fatal, since the schema must be at the expected version for correct operation. The engine logs the migration version discrepancy and aborts startup.

- Write-ahead logging is enabled to ensure durability on unclean shutdowns. Every write transaction is committed with synchronous flush to guarantee that completed operations are persisted before the commit response is returned.

### Integration Points

- Module E (Concurrency Engine): The primary consumer of queue operations. The concurrency engine claims jobs for workers, updates completion state, and reads retry configuration. Communication occurs through the persistence interface with no direct dependency on the database driver.

- Module C (Evaluation Pipeline): Writes job metadata after analysis completes. The evaluation pipeline may read existing metadata for caching purposes.

- Module F (Notification Engine): Reads audit log entries to generate delivery notifications for specific event types. The notification engine queries the audit log by event type and severity without locking.

- Cross-Cutting (Configuration Management): Loads system settings on startup and during hot-reload cycles. Settings control queue priority ordering, retention periods, and worker pool sizes.

- Cross-Cutting (Observability): Publishes queue depth and transaction metrics. Audit log queries feed the reporting subsystem.

### Trade-offs and Design Justification

SQLite was selected as the persistence engine over alternatives such as PostgreSQL or embedded key-value stores because the application operates as a single-process daemon with no need for multi-node concurrency or external database hosting. The choice delivers a self-contained deployment with zero infrastructure dependencies. The zero-CGO constraint is satisfied by using a pure-Go SQLite driver, which trades some raw write throughput for cross-platform build simplicity. For the expected workload of hundreds to low thousands of files per library scan, the throughput difference is imperceptible. The trade-off of SQLite's single-writer limitation is acceptable because the concurrency engine serializes all write operations through the claimed-worker model, eliminating write contention by design. Write-ahead logging is enabled for durability at the cost of a modest performance penalty on high-frequency small writes, which is mitigated by batching metadata updates within single transactions.

---

## Module B: Filesystem Engine

### Primary Responsibility

Discover, catalog, and prepare media files from user-configured scanned paths for processing by the evaluation pipeline. The filesystem engine performs recursive directory traversal, filters files by type and accessibility, detects and deduplicates existing files, and pushes the resulting file list into the processing queue through a backpressure-aware ingestion pipeline. It must handle permission errors, symbolic links, deeply nested structures, and I/O timeouts without aborting the entire scan.

### Core Data Structures

The filesystem engine maintains three primary abstractions:

- Scan Scope: Represents a user-configured root path along with inclusion and exclusion rules. Each scope carries a list of allowed file extensions, glob-based exclusion patterns, a maximum directory depth limit, a symbolic link policy (follow, skip, or dereference), and a timeout duration for I/O operations. Scopes are validated before traversal begins, and invalid scopes are reported as warnings without halting the scan.

- File Record: The output unit of the filesystem engine, containing the absolute file path, file size in bytes, last modification time, cryptographic hash of the first and last megabytes for deduplication, media type classification based on extension and MIME sniffing, and a list of warnings encountered during discovery (permission denied, unreadable, unsupported format). File records are the contract between the filesystem engine and the evaluation pipeline.

- Ingestion Buffer: An in-memory bounded queue that collects file records as they are discovered and feeds them to the processing queue at a controlled rate. The buffer has a configurable capacity, applies deduplication against an in-memory hash index, and blocks producers when full to enforce backpressure.

### Key Interface Contracts

The filesystem engine exposes a scanning interface with these operational boundaries:

- Scan Scope Validation: Validates each configured scan scope by verifying path existence, readability, and sane configuration parameters. Returns a list of validation warnings that are included in the scan report but do not prevent the scan from proceeding for valid scopes.

- Recursive Traversal: Walks each valid scan scope recursively, applying inclusion filters by file extension and exclusion filters by glob pattern. The traversal honors the symbolic link policy and respects depth limits. Each discovered file is classified by media type and a File Record is constructed. Concurrent subdirectory traversal is used to avoid I/O idle time, with a worker limit to prevent resource exhaustion.

- Deduplication: Before emitting each File Record, the engine checks the in-memory hash index. If a file with an identical first-and-last-megabyte hash already exists, a deduplication warning is attached to the record and the file is either skipped or flagged for review based on the deduplication policy configuration. Full cryptographic hash comparison is deferred to the evaluation pipeline to avoid unnecessary I/O on large files.

- Backpressure-Aware Ingestion: Pushes File Records into the ingestion buffer. When the buffer is full, the discovery process blocks until space is available, creating a natural backpressure signal that slows file discovery to match the downstream processing capacity. When the processing queue rejects an item due to capacity limits, the buffer accumulates, and discovery eventually pauses. An optional overflow policy determines whether blocked items are dropped, buffered with an expanded capacity, or cause the scan to halt entirely.

- Error Handling During Traversal: When a permission denied or access denied error is encountered during directory traversal, the engine logs the error, records it in the scan report, and continues with sibling directories. Retry attempts are made once after a brief delay before recording the error as permanent. Timeout errors on file stat operations cause the file to be skipped with a warning.

### State Flow and Lifecycle

The filesystem engine lifecycle follows these phases:

1. Configuration: Accepts a list of scan scopes with validation rules. Validates each scope and builds an internal scope graph, resolving overlapping and nested paths to avoid duplicate scans.

2. Discovery: Launches concurrent subdirectory traversal workers for each scope. Each worker independently traverses its assigned directory subtree, applies filters, checks deduplication, and pushes File Records to the ingestion buffer. Workers respect the configured concurrency limit and emit progress updates at regular intervals.

3. Aggregation: As workers complete, the engine aggregates the discovered File Records from the ingestion buffer. A final deduplication pass against a complete hash index eliminates duplicates that were missed during streaming discovery due to files being discovered by different workers in parallel.

4. Report: Generates a scan report containing total files discovered, files accepted, files skipped by type filter, files flagged as duplicates, directories skipped due to permission errors, and total scan duration. The report is published to the observability subsystem for monitoring.

5. Cleanup: Releases all worker processes, closes file handles, and drains the ingestion buffer. If the engine was interrupted during scanning, any remaining items in the buffer are flushed before cleanup.

### Error Handling and Recovery Strategy

The filesystem engine employs a resilient error handling approach:

- Permission errors during directory traversal are logged and recorded but do not abort the scan. The scan report includes a per-directory breakdown of permission failures.

- I/O timeout errors cause individual files or directories to be skipped with warnings. The engine does not retry timeout errors, as they typically indicate hardware or network issues that are unlikely to resolve during a single scan.

- Disk full errors during metadata collection cause the scan to pause briefly and retry once. If the second attempt also fails, the engine logs a fatal scan error and stops processing that scope, continuing with remaining scopes.

- The engine supports resumable scans through a scan checkpoint mechanism that records the last successfully processed path in each scope. On subsequent scans, the engine skips already-processed paths, with a configurable freshness policy that allows re-scanning of previously processed directories if their modification time has changed.

### Integration Points

- Module A (Persistence Engine): Pushes processing jobs derived from accepted File Records into the persistence queue. The ingestion contract between the two modules defines the shape and semantics of each queued item.

- Module C (Evaluation Pipeline): Provides the File Records that the evaluation pipeline analyzes. The evaluation pipeline may request additional metadata from the filesystem engine for files where initial analysis produced inconclusive results.

- Module E (Concurrency Engine): Receives the ingestion buffer's processed output as the source of work items. The concurrency engine reads from the ingestion buffer to dispatch work to workers.

- Cross-Cutting (Configuration Management): Reads scan scope configurations, file extension lists, depth limits, symlink policies, and timeout values from the centralized settings store.

- Cross-Cutting (Observability): Reports scan progress metrics, file discovery rates, error counts by type, and deduplication statistics.

### Trade-offs and Design Justification

Concurrent subdirectory traversal was chosen over sequential traversal because modern storage systems benefit from parallel I/O operations, and the overhead of goroutine management in Go is minimal compared to I/O wait times. The worker limit prevents resource exhaustion on systems with thousands of small directories. The in-memory deduplication index trades memory for speed: storing hash entries for millions of files may require several hundred megabytes of RAM, but eliminates the need for database-assisted deduplication during discovery. Full cryptographic hashing is deferred to the evaluation pipeline to avoid reading large files during discovery, which would dramatically slow down the scan. The backpressure-aware ingestion buffer decouples discovery speed from processing speed, allowing the engine to gracefully handle scenarios where the transcoding pipeline is slower than file discovery without dropping work or requiring complex buffering layers.

---

## Module C: Evaluation Pipeline

### Primary Responsibility

Analyze media files using ffprobe metadata extraction, normalize the extracted data into a consistent internal representation, apply the user-configured rule-matching engine to determine the appropriate action for each file, and persist the decision and supporting metadata to the persistence engine. The evaluation pipeline is the decision-making core of the system, transforming raw file information into actionable processing directives.

### Core Data Structures

- Analysis Result: The primary output of the ffprobe analysis stage, containing extracted video codec, audio codec, resolution, frame rate, bit rate, duration, color space, bit depth, container format, number of streams, stream languages, subtitle presence, and any anomaly flags detected during analysis.

- Normalized Metadata: The rule-engine-compatible representation of the analysis result, with all values converted to standard units, standardized codec name mapping (including codec alias resolution), boolean flags for HDR presence and high bit depth, and computed derived fields such as estimated output size at target bitrate.

- Rule Set: A collection of user-defined rules, each with a name, an ordered list of match conditions on normalized metadata fields, an action directive (transcode, stream copy, skip, flag for review), an optional output preset, and an optional priority score for conflict resolution when multiple rules match a file.

- Decision Record: The final output of the evaluation pipeline for each file, containing the matched rule identifier, the chosen action, the output preset (if applicable), any flags or warnings generated during evaluation, and the full normalized metadata as supporting evidence for audit purposes.

### Key Interface Contracts

- Analysis Pipeline: Accepts a file path, invokes ffprobe with appropriate arguments to extract comprehensive metadata, parses the structured output, and constructs an Analysis Result. The pipeline handles ffprobe exit codes, captures stderr output for error diagnostics, and enforces a configurable timeout on the ffprobe invocation. Timeouts result in a flag on the Analysis Result rather than a pipeline failure, allowing the evaluation to proceed with partial data.

- Normalization: Transforms an Analysis Result into Normalized Metadata by applying unit conversions, codec name standardization, and derived field computation. Normalization is deterministic and idempotent: running it on the same Analysis Result always produces the same Normalized Metadata.

- Rule Matching: Evaluates a file's Normalized Metadata against the full rule set in priority order. Each rule's conditions are evaluated in sequence, and the first matching rule determines the decision. If multiple rules match at the same priority level, the rule with the most specific condition match count wins. If no rules match, the default rule (configured at rule set level) applies.

- Decision Persistence: Writes the Decision Record to the persistence engine, linking it to the corresponding queue entry. The write includes the full normalized metadata as audit evidence, even when the decision is to skip the file.

### State Flow and Lifecycle

The evaluation pipeline lifecycle for each file follows these phases:

1. Invocation: The pipeline receives a file path from the concurrency engine. It creates a cancellation context that can be aborted by the concurrency engine if the worker is draining.

2. Analysis: The pipeline invokes ffprobe with a timeout and captures structured output. The raw output is parsed into an Analysis Result. If parsing fails, a partial analysis result is constructed from any successfully parsed fields, with a parsing error flag set.

3. Normalization: The Analysis Result is normalized into standard form. Codec aliases are resolved, units are standardized, and derived fields are computed.

4. Evaluation: The normalized metadata is matched against the rule set. The matching engine evaluates conditions in order and produces a Decision Record.

5. Persistence: The Decision Record is written to the persistence engine within the same transaction that updates the queue entry state to the decision outcome.

6. Completion: The pipeline signals completion to the concurrency engine with the decision outcome, file processing duration, and any errors encountered during analysis.

### Error Handling and Recovery Strategy

- ffprobe invocation failures (binary not found, permission denied, timeout) are treated as analysis errors, not fatal errors. The file is flagged for manual review and the queue entry is updated to a review-required state.

- Parsing failures on ffprobe output are logged with the raw output for debugging, and a best-effort partial result is used for rule matching. Rules that depend on unavailable fields are skipped during matching.

- Rule matching failures (malformed rule set, missing conditions) cause the default rule to apply with a warning logged to the audit trail. Malformed rules are quarantined from active evaluation until corrected.

- Persistence write failures cause the file to be requeued with a retry, allowing the evaluation to be redone. Since the normalization stage is deterministic and idempotent, retried evaluations with unchanged inputs produce identical decision records, ensuring audit trail consistency. The rule set is cached in memory between evaluations to avoid repeated loading.

### Integration Points

- Module A (Persistence Engine): Writes Decision Records and reads rule set configuration from system settings. Reads the queue entry to update processing state.

- Module D (Transcoder): Receives the Decision Record and output preset to determine transcoding parameters. If the decision is to transcode, the transcoder reads the preset and source file information.

- Module E (Concurrency Engine): Receives evaluation results to determine next steps. Evaluation outcome drives the concurrency engine's job routing decisions.

- Cross-Cutting (Configuration Management): Reads rule sets, codec alias mappings, and default evaluation settings from the centralized configuration.

- Cross-Cutting (Observability): Publishes evaluation duration, match counts by rule, and decision distribution metrics.

### Trade-offs and Design Justification

The decoupling of analysis, normalization, and rule matching into separate stages provides clarity and testability. ffprobe is invoked as an external process rather than through library bindings to avoid CGO dependencies and to isolate potential crashes or memory issues in the external tool from the Go runtime. The in-memory rule set cache avoids repeated file I/O on rule set updates, which typically occur infrequently. Deferring transcoding decisions to the concurrency engine (rather than having the evaluation pipeline dispatch transcoding directly) provides a clean separation between analysis and execution, allowing the concurrency engine to apply backpressure and priority-based scheduling on top of evaluation outcomes.

---

## Module D: Transcoder

### Primary Responsibility

Execute media transcoding operations based on decisions from the evaluation pipeline, utilizing hardware-accelerated encoding when available, performing quality verification through VMAF scoring, managing temporary files and staging directories, and enforcing quality thresholds before committing transcoded output. The transcoder is the heaviest resource consumer in the system and must handle codec negotiation, GPU resource contention, corruption detection, and safe file operations.

### Core Data Structures

- Hardware Capability Profile: A snapshot of available hardware encoding resources, including GPU device enumeration, supported codec-acceleration mappings (which codecs are hardware-accelerated on which devices), device thermal and utilization status, and memory availability. The profile is refreshed periodically or on-demand when encoding failures suggest hardware issues.

- Encoding Preset: A configuration of encoding parameters derived from the evaluation pipeline's output preset, including target codec, quality level, resolution constraints, bitrate targets, preset speed, and audio parameters. Presets are validated against the available hardware capabilities before encoding begins.

- Transcode Job: The execution unit for the transcoder, containing the source file path, output path, encoding preset, hardware acceleration preference, VMAF verification parameters, and job lifecycle state.

- Verification Result: The output of VMAF quality verification, containing the computed VMAF score, per-segment scores if segmented verification is used, and a pass-fail determination against the configured quality threshold.

### Key Interface Contracts

- Hardware Discovery: Queries the system for available GPU devices and their encoding capabilities. Returns a capability map that lists supported codecs per device, including h.264, h.265, and AV1. The discovery service respects a configured fallback order: preferred device first, then secondary devices, then software encoding as a last resort.

- Codec Negotiation: Given an encoding preset and the hardware capability map, selects the actual codec and acceleration method. If hardware encoding is requested but unavailable for the target codec, the transcoder falls back to software encoding with a downgrade log entry.

- Encoding Execution: Launches the encoding process with the selected parameters, streams to a staging directory to prevent partial output from being mistaken for final output, monitors process completion, and returns an encoding result with duration, output file size, and exit code.

- VMAF Verification: After successful encoding, runs VMAF quality verification comparing the original and transcoded files. The verification process accepts the original file path, transcoded file path, and a quality threshold value. It returns a verification result with the computed score and a pass-fail determination.

- File Management: Manages temporary staging files throughout the transcoding lifecycle with idempotent operations: creating a staging directory when needed, overwriting existing staged files with the same job identifier, and cleaning up on failure. On success, moves the staged output to the final output location. On failure, cleans up all temporary files. If a failure occurs during the move operation, the staged file is preserved for inspection and the job is marked for manual review.

- Corruption Detection: Validates the transcoded output file by attempting to open it with ffprobe and verify stream integrity. If the file is corrupted or unreadable, the job fails and the transcoder attempts to re-encode with software encoding as a recovery attempt.

### State Flow and Lifecycle

The transcoder lifecycle for each job follows these phases:

1. Preparation: The transcoder receives a transcode job from the concurrency engine with a cancellation context that can be aborted if the worker is draining. It validates the source file exists and is readable, checks the target output location for conflicts, and allocates a staging directory for the encoding output.

2. Hardware Selection: The transcoder consults the hardware capability map and negotiates the codec and acceleration method based on the encoding preset. If hardware encoding is selected, the transcoder verifies that the GPU is available and not in a thermal throttled state.

3. Encoding: The transcoder launches the encoding process with the configured parameters, monitoring for process errors. Output is written to the staging directory. If encoding fails, the transcoder attempts software encoding as a fallback if the failure was hardware-related.

4. Verification: After successful encoding, VMAF verification is run. If the score meets the quality threshold, the transcoder proceeds. If the score is below the threshold, the transcoder attempts a re-encode with adjusted parameters (higher quality, lower speed preset) once. If the second attempt also fails verification, the job is marked as quality-failed and flagged for review.

5. Commit or Cleanup: On successful verification, the staged output is moved to the final output location. On failure, all staging files are cleaned up, and the job state is updated in the persistence engine to reflect the failure outcome.

### Error Handling and Recovery Strategy

- Hardware encoding failures that indicate a device-level issue (GPU reset, driver crash, out of video memory) trigger a device health check. If the device passes the health check, the transcoder retries with software encoding. If the device fails, the device is marked as unavailable in the capability map and all pending jobs using that device are reassigned.

- VMAF verification failures are not immediately fatal. A single retry with adjusted parameters is attempted. Only after the retry fails is the job marked as quality-failed.

- File system errors during staging file creation or output move operations cause the job to fail with a detailed error. Staging files are preserved for a configurable period before cleanup to allow forensic analysis.

- Transcoding timeout errors (job exceeds the configured maximum duration) cause the process to be terminated and the job to be requeued. The timeout threshold is scaled based on source file duration to avoid premature termination of long encodes.

### Integration Points

- Module A (Persistence Engine): Updates job state on completion (success, quality-failed, error). Reads encoding presets from system settings.

- Module C (Evaluation Pipeline): Receives the Decision Record and output preset to determine encoding parameters.

- Module E (Concurrency Engine): Receives transcode job dispatches and reports completion status. The concurrency engine manages the job lifecycle boundaries.

- Cross-Cutting (Configuration Management): Reads hardware acceleration preferences, VMAF threshold values, encoding presets, and maximum encoding duration.

- Cross-Cutting (Observability): Publishes encoding duration, quality scores, hardware utilization, and success-failure rates.

### Trade-offs and Design Justification

Staging files are used to prevent partial output from contaminating the output directory, which adds disk I/O overhead (write to staging, then move to output) but eliminates race conditions where another process reads a partially written output file. The VMAF verification is performed on the full file rather than a subset to provide the most accurate quality assessment, which increases encoding pipeline duration but ensures quality compliance. Hardware encoding is preferred for supported codecs but falls back to software encoding to maintain functionality on systems with limited GPU capabilities. The single-retry policy for quality failures balances thoroughness against pipeline throughput: a single retry catches transient hardware issues without creating long retry loops. Temporary file cleanup is deferred in failure cases to enable forensic analysis, with a configurable retention period that is periodically cleaned by a background maintenance operation. All media processing operations in the transcoder — including ffmpeg encoding, VMAF quality verification, and ffprobe-based corruption detection — are implemented as external process invocations rather than library bindings, maintaining the zero-CGO constraint by design and isolating crashes or memory issues in the media tools from the application runtime at the cost of inter-process communication overhead and process spawn latency.
