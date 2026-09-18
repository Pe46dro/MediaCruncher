package persistence

import (
	"time"
)

type JobState string

const (
	StatePending           JobState = "pending"
	StateLeased            JobState = "leased"
	StateEvaluating        JobState = "evaluating"
	StateTranscoding       JobState = "transcoding"
	StateProcessing        JobState = "processing"
	StateCompleted         JobState = "completed"
	StateSkipped           JobState = "skipped"
	StateFailed            JobState = "failed"
	StateQualityFailed     JobState = "quality_failed"
	StateReviewRequired    JobState = "review_required"
	StatePermanentlyFailed JobState = "permanently_failed"
)

type QueueEntry struct {
	ID             int64      `json:"id"`
	FilePath       string     `json:"file_path"`
	State          JobState   `json:"state"`
	WorkerID       string     `json:"worker_id,omitempty"`
	Priority       int        `json:"priority"`
	RetryCount     int        `json:"retry_count"`
	CreatedAt      time.Time  `json:"created_at"`
	LeasedAt       *time.Time `json:"leased_at,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	ScheduledAt    time.Time  `json:"scheduled_at"`
	ErrorMessage   string     `json:"error_message,omitempty"`
}

type JobMetadata struct {
	QueueID        int64     `json:"queue_id"`
	VideoCodec     string    `json:"video_codec"`
	Resolution     string    `json:"resolution"`
	Bitrate        int64     `json:"bitrate"`
	Duration       float64   `json:"duration"`
	AudioTracks    string    `json:"audio_tracks"`
	Subtitles      string    `json:"subtitles"`
	DecisionAction string    `json:"decision_action"`
	Preset         string    `json:"preset"`
	StreamMapJSON  string    `json:"stream_map_json"`
	NormalizedJSON string    `json:"normalized_json"`
	CreatedAt      time.Time `json:"created_at"`
}

type AuditLog struct {
	SeqID       int64     `json:"seq_id"`
	Timestamp   time.Time `json:"timestamp"`
	EventType   string    `json:"event_type"`
	Severity    string    `json:"severity"`
	PayloadJSON string    `json:"payload_json"`
}

type DeliveryRecord struct {
	ID        int64     `json:"id"`
	EventID   string    `json:"event_id"`
	Channel   string    `json:"channel"`
	Status    string    `json:"status"`
	Response  string    `json:"response"`
	Timestamp time.Time `json:"timestamp"`
}

type DeadLetterEntry struct {
	ID         int64     `json:"id"`
	EventJSON  string    `json:"event_json"`
	RetryCount int       `json:"retry_count"`
	LastError  string    `json:"last_error"`
	CreatedAt  time.Time `json:"created_at"`
}
