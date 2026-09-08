package notification

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// AdapterConfig holds the configuration for a channel adapter.
type AdapterConfig struct {
	Type        string `json:"type"`
	URL         string `json:"url"`
	Token       string `json:"token"`
	ChannelID   string `json:"channel_id"`
	Username    string `json:"username"`
	SMTPHost    string `json:"smtp_host"`
	SMTPPort    int    `json:"smtp_port"`
	SMTPUser    string `json:"smtp_user"`
	SMTPPass    string `json:"smtp_pass"`
	ToAddresses []string `json:"to_addresses"`
	Enabled     bool   `json:"enabled"`
	Timeout     string `json:"timeout"`
}

// Adapter is the interface that all channel adapters must implement.
type Adapter interface {
	// Send delivers a message to the channel.
	Send(msg *Message) (*DeliveryResult, error)
	// SendBatch delivers a batch of messages to the channel.
	SendBatch(messages []*Message) ([]*DeliveryResult, error)
	// Validate checks the adapter configuration.
	Validate() error
	// GetName returns the adapter name.
	GetName() string
	// GetType returns the adapter type.
	GetType() string
}

// Message represents a notification message to be delivered.
type Message struct {
	Title       string                 `json:"title"`
	Content     string                 `json:"content"`
	Fields      []MessageField         `json:"fields,omitempty"`
	Color       string                 `json:"color,omitempty"`
	Footer      string                 `json:"footer,omitempty"`
	Timestamp   time.Time              `json:"timestamp"`
	Channel     string                 `json:"channel,omitempty"`
	Event       *Event                 `json:"event,omitempty"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
}

// MessageField is a key-value field in a message.
type MessageField struct {
	Title string      `json:"title"`
	Value interface{} `json:"value"`
	Short bool        `json:"short,omitempty"`
}

// DeliveryResult holds the outcome of a delivery attempt.
type DeliveryResult struct {
	Success     bool    `json:"success"`
	MessageID   string  `json:"message_id,omitempty"`
	StatusCode  int     `json:"status_code,omitempty"`
	Error       string  `json:"error,omitempty"`
	Duration    time.Duration `json:"duration"`
	Timestamp   time.Time `json:"timestamp"`
	Body        string  `json:"body,omitempty"`
}

// HTTPAdapter is a base adapter for HTTP-based notification platforms.
type HTTPAdapter struct {
	name      string
	config    AdapterConfig
	client    *http.Client
	enabled   bool
}

// NewHTTPAdapter creates a new HTTP-based adapter.
func NewHTTPAdapter(name string, config AdapterConfig) *HTTPAdapter {
	timeout := 10 * time.Second
	if config.Timeout != "" {
		if d, err := time.ParseDuration(config.Timeout); err == nil {
			timeout = d
		}
	}
	return &HTTPAdapter{
		name: name,
		config: config,
		client: &http.Client{
			Timeout: timeout,
		},
		enabled: config.Enabled,
	}
}

// Validate checks the adapter configuration.
func (a *HTTPAdapter) Validate() error {
	if a.config.Type == "" {
		return fmt.Errorf("adapter type is empty")
	}
	if a.config.URL == "" && a.name != "smtp" {
		return fmt.Errorf("adapter URL is empty")
	}
	if !a.enabled {
		return nil
	}
	return nil
}

// GetName returns the adapter name.
func (a *HTTPAdapter) GetName() string {
	return a.name
}

// GetType returns the adapter type.
func (a *HTTPAdapter) GetType() string {
	return a.config.Type
}

// GetConfig returns the adapter configuration.
func (a *HTTPAdapter) GetConfig() AdapterConfig {
	return a.config
}

// isEnabled returns whether the adapter is enabled.
func (a *HTTPAdapter) IsEnabled() bool {
	return a.enabled
}

// validateURL checks that the URL is safe to use for outbound HTTP requests.
// It rejects non-HTTPS schemes and URLs pointing to private, loopback, or link-local IPs.
func validateURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("only https URLs are allowed, got %s", parsed.Scheme)
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return fmt.Errorf("URL points to restricted IP: %s", host)
		}
	}
	return nil
}

// DoPOST sends a POST request and returns the response body.
func (a *HTTPAdapter) DoPOST(rawURL string, body interface{}) (*http.Response, []byte, error) {
	if err := validateURL(rawURL); err != nil {
		return nil, nil, fmt.Errorf("URL validation failed: %w", err)
	}

	var bodyBuf bytes.Buffer
	enc := json.NewEncoder(&bodyBuf)
	if err := enc.Encode(body); err != nil {
		return nil, nil, fmt.Errorf("encode request body: %w", err)
	}

	req, err := http.NewRequest("POST", rawURL, &bodyBuf)
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := json.Marshal(map[string]interface{}{
		"status_code": resp.StatusCode,
	})
	if err != nil {
		respBody = []byte(fmt.Sprintf(`{"status_code":%d}`, resp.StatusCode))
	}

	return resp, respBody, nil
}

// BuildDeliveryResult creates a DeliveryResult from a delivery attempt.
func BuildDeliveryResult(success bool, messageID string, statusCode int, err error, start time.Time) *DeliveryResult {
	result := &DeliveryResult{
		Success:   success,
		MessageID: messageID,
		StatusCode: statusCode,
		Duration:  time.Since(start),
		Timestamp: time.Now(),
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// BuildMessage creates a Message from an Event.
func BuildMessage(evt *Event) *Message {
	return &Message{
		Title:     buildEventTitle(evt),
		Content:   buildEventContent(evt),
		Color:     buildEventColor(evt),
		Timestamp: evt.Timestamp,
		Event:     evt,
		Metadata:  evt.Context,
	}
}

// WithMetadata adds metadata to the message.
func (m *Message) WithMetadata(key string, value interface{}) *Message {
	if m.Metadata == nil {
		m.Metadata = make(map[string]interface{})
	}
	m.Metadata[key] = value
	return m
}

// WithChannel sets the target channel for the message.
func (m *Message) WithChannel(channel string) *Message {
	m.Channel = channel
	return m
}

// buildEventTitle creates a title for the event message.
func buildEventTitle(evt *Event) string {
	switch evt.Type {
	case EventTypeJobCompleted:
		return "Job Completed"
	case EventTypeJobFailed:
		return "Job Failed"
	case EventTypeScanStarted:
		return "Scan Started"
	case EventTypeScanCompleted:
		return "Scan Completed"
	case EventTypeQualityExceeded:
		return "Quality Threshold Exceeded"
	case EventTypeSystemError:
		return "System Error"
	case EventTypeTranscodeStarted:
		return "Transcode Started"
	case EventTypeTranscodeFailed:
		return "Transcode Failed"
	default:
		return "Notification"
	}
}

// buildEventContent creates content for the event message.
func buildEventContent(evt *Event) string {
	ctx := evt.Context
	if ctx == nil {
		ctx = make(map[string]interface{})
	}

	source := ctx["source"]
	if source == nil {
		source = "unknown"
	}

	switch evt.Type {
	case EventTypeJobCompleted:
		return fmt.Sprintf("Job completed successfully: %v", source)
	case EventTypeJobFailed:
		reason := ctx["reason"]
		if reason == nil {
			reason = "unknown error"
		}
		return fmt.Sprintf("Job failed: %v - Reason: %v", source, reason)
	case EventTypeScanStarted:
		return fmt.Sprintf("Scan started on: %v", source)
	case EventTypeScanCompleted:
		total := ctx["total_files"]
		return fmt.Sprintf("Scan completed: %v files processed on %v", total, source)
	case EventTypeQualityExceeded:
		score := ctx["vmaf_score"]
		threshold := ctx["threshold"]
		return fmt.Sprintf("Quality below threshold: score=%v, threshold=%v", score, threshold)
	case EventTypeSystemError:
		msg := ctx["error"]
		if msg == nil {
			msg = "unknown system error"
		}
		return fmt.Sprintf("System error: %v", msg)
	case EventTypeTranscodeStarted:
		return fmt.Sprintf("Transcode started: %v", source)
	case EventTypeTranscodeFailed:
		reason := ctx["reason"]
		if reason == nil {
			reason = "unknown error"
		}
		return fmt.Sprintf("Transcode failed: %v - Reason: %v", source, reason)
	default:
		return fmt.Sprintf("Event: %v", evt.Type)
	}
}

// buildEventColor creates a color string for the event message.
func buildEventColor(evt *Event) string {
	switch evt.Severity {
	case SeverityInfo:
		return "#36a64f"
	case SeverityWarning:
		return "#ffcc00"
	case SeverityError:
		return "#ff6600"
	case SeverityCritical:
		return "#ff0000"
	default:
		return "#36a64f"
	}
}
