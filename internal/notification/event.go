package notification

import (
	"fmt"
	"time"
)

// EventType represents the type of notification event.
type EventType string

const (
	EventTypeJobCompleted     EventType = "job_completed"
	EventTypeJobFailed        EventType = "job_failed"
	EventTypeScanStarted      EventType = "scan_started"
	EventTypeScanCompleted    EventType = "scan_completed"
	EventTypeQualityExceeded  EventType = "quality_exceeded"
	EventTypeSystemError      EventType = "system_error"
	EventTypeTranscodeStarted EventType = "transcode_started"
	EventTypeTranscodeFailed  EventType = "transcode_failed"
)

// Severity represents the severity level of an event.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// Event is the atomic unit of notification.
type Event struct {
	ID         string                 `json:"id"`
	Type       EventType              `json:"type"`
	Severity   Severity               `json:"severity"`
	Timestamp  time.Time              `json:"timestamp"`
	Context    map[string]interface{} `json:"context"`
	Channel    string                 `json:"channel,omitempty"`
	Immediate  bool                   `json:"immediate"`
	RetryCount int                    `json:"retry_count"`
}

// NewEvent creates a new notification event.
func NewEvent(eventType EventType, severity Severity, context map[string]interface{}) *Event {
	return &Event{
		ID:         fmt.Sprintf("evt-%d-%s", time.Now().UnixNano(), eventType),
		Type:       eventType,
		Severity:   severity,
		Timestamp:  time.Now(),
		Context:    context,
		Immediate:  severity == SeverityCritical || severity == SeverityError,
		RetryCount: 0,
	}
}

// WithChannel sets the target channel for the event.
func (e *Event) WithChannel(channel string) *Event {
	e.Channel = channel
	return e
}

// WithImmediate sets whether the event should be delivered immediately.
func (e *Event) WithImmediate(immediate bool) *Event {
	e.Immediate = immediate
	return e
}

// Clone creates a copy of the event.
func (e *Event) Clone() *Event {
	ctx := make(map[string]interface{}, len(e.Context))
	for k, v := range e.Context {
		ctx[k] = v
	}
	return &Event{
		ID:         e.ID,
		Type:       e.Type,
		Severity:   e.Severity,
		Timestamp:  e.Timestamp,
		Context:    ctx,
		Channel:    e.Channel,
		Immediate:  e.Immediate,
		RetryCount: e.RetryCount,
	}
}

// String returns a string representation of the event.
func (e *Event) String() string {
	return fmt.Sprintf("event{type=%s, severity=%s, id=%s}", e.Type, e.Severity, e.ID)
}

// IsHighSeverity returns true if the event is error or critical severity.
func (e *Event) IsHighSeverity() bool {
	return e.Severity == SeverityError || e.Severity == SeverityCritical
}

// IsImmediate returns true if the event should bypass batching.
func (e *Event) IsImmediate() bool {
	return e.Immediate
}
