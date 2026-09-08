package adapters

import (
	"fmt"
	"net/http"
	"time"

	"mediacruncher/internal/notification"
)

// SlackAdapter delivers notifications to Slack via Webhooks.
type SlackAdapter struct {
	*notification.HTTPAdapter
	webhookURL string
}

// SlackBlock represents a Slack message block.
type SlackBlock struct {
	Type     string            `json:"type"`
	Text     *SlackText        `json:"text,omitempty"`
	Fields   []SlackFieldBlock `json:"fields,omitempty"`
}

// SlackText represents text in a Slack block.
type SlackText struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji bool   `json:"emoji,omitempty"`
}

// SlackFieldBlock represents a field block in Slack.
type SlackFieldBlock struct {
	Type  string `json:"type"`
	Text  SlackText `json:"text"`
	Short bool    `json:"short,omitempty"`
}

// SlackAttachment represents a Slack attachment.
type SlackAttachment struct {
	Color   string           `json:"color"`
	Title   string           `json:"title"`
	Text    string           `json:"text"`
	Fields  []SlackFieldBlock `json:"fields,omitempty"`
	Footer  string           `json:"footer,omitempty"`
	Ts      int64            `json:"ts,omitempty"`
}

// SlackMessage represents a Slack webhook message.
type SlackMessage struct {
	Channel     string           `json:"channel,omitempty"`
	Text        string           `json:"text"`
	Attachments []SlackAttachment `json:"attachments,omitempty"`
}

// NewSlackAdapter creates a new Slack adapter.
func NewSlackAdapter(config notification.AdapterConfig) (*SlackAdapter, error) {
	base := notification.NewHTTPAdapter("slack", config)
	if err := base.Validate(); err != nil {
		return nil, err
	}

	return &SlackAdapter{
		HTTPAdapter: base,
		webhookURL:  config.URL,
	}, nil
}

// Send delivers a message to Slack.
func (a *SlackAdapter) Send(msg *notification.Message) (*notification.DeliveryResult, error) {
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

	attachment := SlackAttachment{
		Color: msg.Color,
		Title: msg.Title,
		Text:  msg.Content,
	}

	for _, f := range msg.Fields {
		attachment.Fields = append(attachment.Fields, SlackFieldBlock{
			Type:  "mrkdwn",
			Text:  SlackText{Type: "mrkdwn", Text: fmt.Sprintf("*%s*: %v", f.Title, f.Value)},
			Short: f.Short,
		})
	}

	if msg.Footer != "" {
		attachment.Footer = msg.Footer
		attachment.Ts = msg.Timestamp.Unix()
	}

	slackMsg := &SlackMessage{
		Attachments: []SlackAttachment{attachment},
	}

	resp, _, err := a.DoPOST(a.webhookURL, slackMsg)
	if err != nil {
		return notification.BuildDeliveryResult(false, "", 0, err, start), nil
	}

	if resp.StatusCode != http.StatusOK {
		return notification.BuildDeliveryResult(false, "", resp.StatusCode, nil, start), nil
	}

	return notification.BuildDeliveryResult(true, fmt.Sprintf("sk-%s", msg.Title), resp.StatusCode, nil, start), nil
}

// SendBatch delivers a batch of messages to Slack.
func (a *SlackAdapter) SendBatch(messages []*notification.Message) ([]*notification.DeliveryResult, error) {
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

// Validate checks the Slack adapter configuration.
func (a *SlackAdapter) Validate() error {
	if a.webhookURL == "" {
		return fmt.Errorf("slack webhook URL is required")
	}
	return nil
}
