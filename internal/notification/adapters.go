package notification

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"mediacruncher/internal/config"
)

type NotificationPayload struct {
	EventID       string                 `json:"event_id"`
	EventType     string                 `json:"event_type"` // job_completed, job_failed, batch_summary, etc.
	Title         string                 `json:"title"`
	Message       string                 `json:"message"`
	Timestamp     time.Time              `json:"timestamp"`
	TotalItems    int                    `json:"total_items,omitempty"`
	SavedBytes    int64                  `json:"saved_bytes,omitempty"`
	AverageVMAF   float64                `json:"average_vmaf,omitempty"`
	Metadata      map[string]any         `json:"metadata,omitempty"`
}

type Adapter interface {
	Name() string
	Type() string
	SupportsEvent(eventType string) bool
	Send(ctx context.Context, payload NotificationPayload) error
}

// DiscordAdapter posts rich embeds to Discord webhooks.
type DiscordAdapter struct {
	name       string
	webhookURL string
	events     map[string]bool
	client     *http.Client
}

func NewDiscordAdapter(cfg config.ChannelConfig, client *http.Client) *DiscordAdapter {
	events := make(map[string]bool)
	for _, e := range cfg.Events {
		events[strings.ToLower(e)] = true
	}
	return &DiscordAdapter{
		name:       cfg.Type,
		webhookURL: cfg.Target,
		events:     events,
		client:     client,
	}
}

func (d *DiscordAdapter) Name() string { return d.name }
func (d *DiscordAdapter) Type() string { return "discord" }
func (d *DiscordAdapter) SupportsEvent(evt string) bool {
	if len(d.events) == 0 {
		return true
	}
	return d.events[strings.ToLower(evt)]
}

func (d *DiscordAdapter) Send(ctx context.Context, p NotificationPayload) error {
	color := 0x2ecc71 // Green
	if strings.Contains(p.EventType, "failed") {
		color = 0xe74c3c // Red
	} else if strings.Contains(p.EventType, "skipped") {
		color = 0xf1c40f // Yellow
	}

	embed := map[string]any{
		"title":       p.Title,
		"description": p.Message,
		"color":       color,
		"timestamp":   p.Timestamp.Format(time.RFC3339),
		"footer": map[string]string{
			"text": "MediaCruncher Transcoder",
		},
	}

	var fields []map[string]any
	if p.SavedBytes > 0 {
		fields = append(fields, map[string]any{
			"name":   "Saved Space",
			"value":  formatBytes(p.SavedBytes),
			"inline": true,
		})
	}
	if p.AverageVMAF > 0 {
		fields = append(fields, map[string]any{
			"name":   "Avg VMAF",
			"value":  fmt.Sprintf("%.2f", p.AverageVMAF),
			"inline": true,
		})
	}
	if len(fields) > 0 {
		embed["fields"] = fields
	}

	body := map[string]any{
		"embeds": []map[string]any{embed},
	}

	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webhookURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discord webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// TelegramAdapter delivers notifications to a Telegram chat.
type TelegramAdapter struct {
	token  string
	chatID string
	events map[string]bool
	client *http.Client
}

func NewTelegramAdapter(cfg config.ChannelConfig, client *http.Client) *TelegramAdapter {
	events := make(map[string]bool)
	for _, e := range cfg.Events {
		events[strings.ToLower(e)] = true
	}
	return &TelegramAdapter{
		token:  cfg.Token,
		chatID: cfg.Target,
		events: events,
		client: client,
	}
}

func (t *TelegramAdapter) Name() string { return "telegram" }
func (t *TelegramAdapter) Type() string { return "telegram" }
func (t *TelegramAdapter) SupportsEvent(evt string) bool {
	if len(t.events) == 0 {
		return true
	}
	return t.events[strings.ToLower(evt)]
}

func (t *TelegramAdapter) Send(ctx context.Context, p NotificationPayload) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token)
	text := fmt.Sprintf("*%s*\n%s", escapeTelegram(p.Title), escapeTelegram(p.Message))
	if p.SavedBytes > 0 {
		text += fmt.Sprintf("\nSaved: %s", formatBytes(p.SavedBytes))
	}
	if p.AverageVMAF > 0 {
		text += fmt.Sprintf(" | VMAF: %.2f", p.AverageVMAF)
	}

	body := map[string]any{
		"chat_id":    t.chatID,
		"text":       text,
		"parse_mode": "Markdown",
	}

	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram API returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// SlackAdapter delivers notifications to Slack via incoming webhook.
type SlackAdapter struct {
	webhookURL string
	events     map[string]bool
	client     *http.Client
}

func NewSlackAdapter(cfg config.ChannelConfig, client *http.Client) *SlackAdapter {
	events := make(map[string]bool)
	for _, e := range cfg.Events {
		events[strings.ToLower(e)] = true
	}
	return &SlackAdapter{
		webhookURL: cfg.Target,
		events:     events,
		client:     client,
	}
}

func (s *SlackAdapter) Name() string { return "slack" }
func (s *SlackAdapter) Type() string { return "slack" }
func (s *SlackAdapter) SupportsEvent(evt string) bool {
	if len(s.events) == 0 {
		return true
	}
	return s.events[strings.ToLower(evt)]
}

func (s *SlackAdapter) Send(ctx context.Context, p NotificationPayload) error {
	body := map[string]any{
		"text": fmt.Sprintf("*%s*\n%s", p.Title, p.Message),
	}
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// GenericWebhookAdapter dispatches JSON with HMAC-SHA256 signature verification.
type GenericWebhookAdapter struct {
	targetURL string
	secretKey string
	events    map[string]bool
	client    *http.Client
}

func NewGenericWebhookAdapter(cfg config.ChannelConfig, client *http.Client) *GenericWebhookAdapter {
	events := make(map[string]bool)
	for _, e := range cfg.Events {
		events[strings.ToLower(e)] = true
	}
	return &GenericWebhookAdapter{
		targetURL: cfg.Target,
		secretKey: cfg.Token,
		events:    events,
		client:    client,
	}
}

func (g *GenericWebhookAdapter) Name() string { return "webhook" }
func (g *GenericWebhookAdapter) Type() string { return "webhook" }
func (g *GenericWebhookAdapter) SupportsEvent(evt string) bool {
	if len(g.events) == 0 {
		return true
	}
	return g.events[strings.ToLower(evt)]
}

func (g *GenericWebhookAdapter) Send(ctx context.Context, p NotificationPayload) error {
	jsonBytes, err := json.Marshal(p)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.targetURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "MediaCruncher-Notifier/1.0")

	// Apply HMAC-SHA256 signature if secret key is present
	if g.secretKey != "" {
		mac := hmac.New(sha256.New, []byte(g.secretKey))
		mac.Write(jsonBytes)
		sig := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-Signature-SHA256", "sha256="+sig)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("generic webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// GotifyAdapter dispatches messages to Gotify push server.
type GotifyAdapter struct {
	baseURL string
	token   string
	events  map[string]bool
	client  *http.Client
}

func NewGotifyAdapter(cfg config.ChannelConfig, client *http.Client) *GotifyAdapter {
	events := make(map[string]bool)
	for _, e := range cfg.Events {
		events[strings.ToLower(e)] = true
	}
	return &GotifyAdapter{
		baseURL: strings.TrimRight(cfg.Target, "/"),
		token:   cfg.Token,
		events:  events,
		client:  client,
	}
}

func (gt *GotifyAdapter) Name() string { return "gotify" }
func (gt *GotifyAdapter) Type() string { return "gotify" }
func (gt *GotifyAdapter) SupportsEvent(evt string) bool {
	if len(gt.events) == 0 {
		return true
	}
	return gt.events[strings.ToLower(evt)]
}

func (gt *GotifyAdapter) Send(ctx context.Context, p NotificationPayload) error {
	url := fmt.Sprintf("%s/message", gt.baseURL)
	priority := 5
	if strings.Contains(p.EventType, "failed") {
		priority = 8
	}

	body := map[string]any{
		"title":    p.Title,
		"message":  p.Message,
		"priority": priority,
	}

	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", gt.token)

	resp, err := gt.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gotify server returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// SMTPAdapter sends notification emails via SMTP.
type SMTPAdapter struct {
	targetEmail string
	host        string
	port        string
	username    string
	password    string
	from        string
	events      map[string]bool
}

func NewSMTPAdapter(cfg config.ChannelConfig) *SMTPAdapter {
	events := make(map[string]bool)
	for _, e := range cfg.Events {
		events[strings.ToLower(e)] = true
	}
	host := cfg.Options["host"]
	port := cfg.Options["port"]
	if port == "" {
		port = "587"
	}
	from := cfg.Options["from"]
	if from == "" {
		from = "mediacruncher@localhost"
	}

	return &SMTPAdapter{
		targetEmail: cfg.Target,
		host:        host,
		port:        port,
		username:    cfg.Options["username"],
		password:    cfg.Token,
		from:        from,
		events:      events,
	}
}

func (s *SMTPAdapter) Name() string { return "smtp" }
func (s *SMTPAdapter) Type() string { return "smtp" }
func (s *SMTPAdapter) SupportsEvent(evt string) bool {
	if len(s.events) == 0 {
		return true
	}
	return s.events[strings.ToLower(evt)]
}

func (s *SMTPAdapter) Send(ctx context.Context, p NotificationPayload) error {
	if s.host == "" {
		return fmt.Errorf("smtp host not configured")
	}

	subject := fmt.Sprintf("Subject: [MediaCruncher] %s\r\n", p.Title)
	mime := "MIME-version: 1.0;\r\nContent-Type: text/html; charset=\"UTF-8\";\r\n\r\n"
	body := fmt.Sprintf("<html><body><h2>%s</h2><p>%s</p><p><strong>Saved:</strong> %s</p><p><strong>VMAF:</strong> %.2f</p></body></html>",
		p.Title, p.Message, formatBytes(p.SavedBytes), p.AverageVMAF)

	msg := []byte(subject + mime + body)
	addr := fmt.Sprintf("%s:%s", s.host, s.port)

	var auth smtp.Auth
	if s.username != "" && s.password != "" {
		auth = smtp.PlainAuth("", s.username, s.password, s.host)
	}

	return smtp.SendMail(addr, auth, s.from, []string{s.targetEmail}, msg)
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func escapeTelegram(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"`", "\\`",
	)
	return replacer.Replace(s)
}
