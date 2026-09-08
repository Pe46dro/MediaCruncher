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

---

## Module E: Concurrency Engine

### Primary Responsibility

Orchestrate the concurrent processing of transcoding jobs across a configurable pool of workers, implementing priority-based task routing, backpressure-aware concurrency control, graceful worker lifecycle management, and retry logic with exponential backoff. The concurrency engine is the operational heart of the system, transforming the ordered queue of pending work into parallel execution while respecting resource limits, enforcing cancellation through context propagation, and transitioning to a drain phase during shutdown.

### Core Data Structures

- Task Queue: An in-memory priority queue that holds pending work items ordered by priority level and submission time. The queue supports atomic claim operations that remove the highest-priority available item for a specific worker, preventing duplicate assignments. The queue carries a configurable maximum depth that triggers backpressure when full.

- Worker Pool: A collection of active and idle workers, each associated with a processing context that carries cancellation signals for lifecycle management. The pool tracks worker state (idle, processing, draining, stopped), the current job assignment, and processing statistics per worker.

- Routing Table: A mapping of evaluation outcomes to worker dispatch strategies. After a file is evaluated, the routing table determines whether the work item should be dispatched to a transcoder worker, sent for notification processing, or marked complete. The routing table is configured by the evaluation pipeline's decision outcomes.

- Retry Record: Metadata attached to queue entries that have failed processing, tracking the retry count, next scheduled retry time based on exponential backoff, the failure reason, and whether the maximum retry limit has been reached.

### Key Interface Contracts

- Task Submission: Accepts work items from the ingestion pipeline (file paths for evaluation) and the filesystem engine (completed scan items). Items are enqueued with a priority level and a job type classifier (evaluate, transcode, notify). The submission interface returns a status indicating acceptance or rejection due to queue capacity limits, triggering backpressure upstream.

- Priority Claim: Workers claim the highest-priority available item for their job type. The claim operation is atomic: only one worker can claim a specific item, and the item transitions from pending to processing state with the worker identifier recorded. Items are claimed in priority order within each job type, with lower-priority items served only when all higher-priority items are in progress or queued.

- Worker Dispatch: Routes a claimed item to the appropriate processing stage based on its job type. Evaluation items are dispatched to the evaluation pipeline module. Transcode items are dispatched to the transcoder module. Notification items are dispatched to the notification module. The dispatch operation creates a cancellation context for the worker and returns a result channel for collecting the outcome.

- Result Collection: Collects the outcome from dispatched workers, including success, failure with error detail, or timeout. On success, the result is processed according to the routing table (e.g., a successful evaluation may enqueue a transcode job). On failure, the retry logic determines whether to requeue the item with an exponential backoff delay or mark it as permanently failed.

- Backpressure Enforcement: When the task queue reaches its configured capacity, new task submissions are blocked until space becomes available through worker completion. The backpressure signal propagates upstream: ingestion slows when evaluation queues fill, evaluation slows when transcode queues fill, creating a cascading flow-control mechanism.

- Worker Lifecycle: Initializes the configured number of workers, each running an independent processing loop that claims items, dispatches work, collects results, and repeats. During graceful shutdown, the engine transitions to a drain phase: no new items are accepted, existing workers complete their current assignments, and the engine waits for all workers to finish before reporting completion.

### State Flow and Lifecycle

The concurrency engine lifecycle follows these phases:

1. Initialization: The engine creates the task queue with configured priority levels and capacity limits, initializes the worker pool with the specified worker count, and establishes the routing table based on evaluation outcomes and transcode decisions. Each worker is launched with its own cancellation context.

2. Processing: Workers continuously claim items from the task queue and dispatch them to the appropriate processing modules. Results are collected, routed, and either enqueued as follow-up work (evaluation results trigger transcode jobs) or marked as complete. The engine monitors queue depths and worker utilization, publishing metrics at regular intervals.

3. Retry Handling: Failed jobs enter the retry record system. The first retry is scheduled after a base backoff interval. Each subsequent failure doubles the backoff interval up to a configured maximum. If the retry count reaches the maximum, the job is marked as permanently failed and flagged for review in the persistence engine.

4. Drain Phase: During graceful shutdown, the engine stops accepting new task submissions, signals all workers to stop after their current assignment completes, and waits for all workers to finish. In-progress jobs complete normally; new items are rejected. The engine waits up to a configured drain timeout before forcibly stopping remaining workers.

5. Shutdown: After all workers have completed or been stopped, the engine records final statistics, flushes any pending audit log entries, and reports the drain completion status to the shutdown coordinator.

### Error Handling and Recovery Strategy

- Queue capacity overflow: When the task queue is full and new items cannot be enqueued, the upstream module (filesystem engine, evaluation pipeline, or external submission) is signalled to slow down. The blocked submission retries after a short interval. If the queue remains full for an extended period, the engine logs a warning about sustained backpressure.

- Worker crashes: If a worker process terminates unexpectedly, the in-flight job is requeued with incremented retry count. The worker pool automatically replaces the crashed worker with a new instance. The job is re-evaluated from the beginning of its processing stage.

- Dispatch failures: If the target processing module (evaluation pipeline or transcoder) cannot accept a dispatch (e.g., the module is unhealthy or the context is cancelled), the job is requeued with a brief delay. Repeated dispatch failures to the same module cause the engine to mark that module as degraded and log a warning.

- Timeout handling: Each dispatched job has a configurable timeout. If the job exceeds its timeout, the worker's cancellation context is triggered, the in-flight operation should abort, and the result is treated as a timeout failure. The job enters the retry cycle with exponential backoff.

### Integration Points

- Module A (Persistence Engine): Reads pending queue entries on startup after recovery, claims jobs by updating queue entry state to processing, and writes completion or failure state transitions.

- Module B (Filesystem Engine): Receives scan result items through the task submission interface when the filesystem engine pushes completed scan items into the processing pipeline.

- Module C (Evaluation Pipeline): Receives evaluation task dispatches from the task queue. The concurrency engine collects evaluation results and routes them based on the routing table.

- Module D (Transcoder): Receives transcode task dispatches. The concurrency engine collects transcoding results and reports completion status.

- Module F (Notification Engine): Receives notification task dispatches for events that require outbound communication.

- Cross-Cutting (Configuration Management): Reads worker pool size, queue capacity, retry policies, backoff settings, and drain timeout from centralized configuration.

- Cross-Cutting (Graceful Shutdown): Receives the shutdown signal and orchestrates the drain sequence. Reports drain completion to the shutdown coordinator.

- Cross-Cutting (Observability): Publishes queue depth, worker utilization, task completion rates, retry counts, and backpressure duration metrics.

### Trade-offs and Design Justification

An in-memory task queue was chosen over a persistent queue to minimize latency and avoid the complexity of distributed task management for a single-process daemon. The zero-CGO constraint is satisfied by implementing the priority queue with a pure-Go heap data structure. The worker pool model with context-based cancellation provides fine-grained control over worker lifecycle and shutdown behavior without requiring external process management. The exponential backoff retry policy prevents thundering herd scenarios where many retried jobs flood the system simultaneously. Backpressure is enforced at the queue submission level rather than through rate limiting, creating a natural flow-control mechanism that adapts to actual system load rather than a fixed rate. The routing table approach decouples the concurrency engine from the specific processing pipeline, allowing the system to add new processing stages without modifying the core orchestration logic.

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

- Adapter Registration: Registers a channel adapter by type (Telegram, Discord, Slack, Gotify, SMTP) with its configuration parameters. Each adapter validates its configuration at registration time and reports registration success or failure. Registered adapters are stored in a thread-safe adapter registry that the delivery router consults to select the appropriate adapter.

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

- Artifact Caching: GitHub Actions caching is used to cache Go module dependencies between workflow runs, significantly reducing build times for successive runs on the same or related branches. The cache key is derived from the lock file hash to ensure cache validity.

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

GitLab CI/CD was designed as a parallel alternative to GitHub Actions rather than a replacement, providing platform choice without duplicating all CI/CD logic. The pipeline stage structure mirrors GitHub Actions' workflow job structure, maintaining conceptual consistency across both implementations. GitLab-specific features (runner groups, CI/CD variables, approval gates, packages registry) are leveraged where they provide unique value compared to the GitHub implementation. The approval gate provides a human release checkpoint that is more flexible than GitHub's release draft workflow, allowing conditional approval requirements based on project configuration. Package registry integration provides versioned artifact storage within the GitLab project, eliminating the need for external artifact hosting.

