package adapters

import (
	"fmt"
	"net/http"
	"time"

	"mediacruncher/internal/notification"
)

// DiscordAdapter delivers notifications to Discord via Webhooks.
type DiscordAdapter struct {
	*notification.HTTPAdapter
	webhookURL string
}

// DiscordEmbed represents a Discord embed field.
type DiscordEmbed struct {
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Color       int               `json:"color"`
	Fields      []DiscordField    `json:"fields,omitempty"`
	Footer      *DiscordFooter    `json:"footer,omitempty"`
	Timestamp   string            `json:"timestamp,omitempty"`
}

// DiscordField represents a field in a Discord embed.
type DiscordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Short  bool   `json:"short,omitempty"`
}

// DiscordFooter represents the footer of a Discord embed.
type DiscordFooter struct {
	Text string `json:"text"`
}

// DiscordWebhook represents a Discord webhook message.
type DiscordWebhook struct {
	Content   string         `json:"content,omitempty"`
	Embeds    []DiscordEmbed `json:"embeds,omitempty"`
	Username  string         `json:"username,omitempty"`
}

// NewDiscordAdapter creates a new Discord adapter.
func NewDiscordAdapter(config notification.AdapterConfig) (*DiscordAdapter, error) {
	base := notification.NewHTTPAdapter("discord", config)
	if err := base.Validate(); err != nil {
		return nil, err
	}

	return &DiscordAdapter{
		HTTPAdapter: base,
		webhookURL:  config.URL,
	}, nil
}

// Send delivers a message to Discord.
func (a *DiscordAdapter) Send(msg *notification.Message) (*notification.DeliveryResult, error) {
	if !a.IsEnabled() {
		return &notification.DeliveryResult{
			Success:   false,
			Error:     "adapter disabled",
			Timestamp: time.Now(),
		}, nil
	}

	if err := a.Validate(); err != nil {
		return nil, err
	}

	start := time.Now()

	colorInt := 0
	switch msg.Color {
	case "#36a64f":
		colorInt = 0x36A64F
	case "#ffcc00":
		colorInt = 0xFFCC00
	case "#ff6600":
		colorInt = 0xFF6600
	case "#ff0000":
		colorInt = 0xFF0000
	}

	embed := DiscordEmbed{
		Title:       msg.Title,
		Description: msg.Content,
		Color:       colorInt,
		Timestamp:   msg.Timestamp.UTC().Format(time.RFC3339),
	}

	if len(msg.Fields) > 0 {
		for _, f := range msg.Fields {
			embed.Fields = append(embed.Fields, DiscordField{
				Name:   f.Title,
				Value:  fmt.Sprintf("%v", f.Value),
				Short:  f.Short,
			})
		}
	}

	if msg.Footer != "" {
		embed.Footer = &DiscordFooter{Text: msg.Footer}
	}

	webhook := &DiscordWebhook{
		Embeds: []DiscordEmbed{embed},
	}

	resp, _, err := a.DoPOST(a.webhookURL, webhook)
	if err != nil {
		return notification.BuildDeliveryResult(false, "", 0, err, start), nil
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return notification.BuildDeliveryResult(false, "", resp.StatusCode, nil, start), nil
	}

	return notification.BuildDeliveryResult(true, fmt.Sprintf("dc-%s", msg.Title), resp.StatusCode, nil, start), nil
}

// SendBatch delivers a batch of messages to Discord.
func (a *DiscordAdapter) SendBatch(messages []*notification.Message) ([]*notification.DeliveryResult, error) {
	results := make([]*notification.DeliveryResult, len(messages))
	for i, msg := range messages {
		result, err := a.Send(msg)
		if err != nil {
			results[i] = notification.BuildDeliveryResult(false, "", 0, err, time.Now())
		} else {
			results[i] = result
		}
	}
	return results, nil
}

// Validate checks the Discord adapter configuration.
func (a *DiscordAdapter) Validate() error {
	if a.webhookURL == "" {
		return fmt.Errorf("discord webhook URL is required")
	}
	return nil
}
