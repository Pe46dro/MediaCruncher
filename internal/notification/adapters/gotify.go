package adapters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"mediacruncher/internal/notification"
)

// GotifyAdapter delivers notifications to Gotify server.
type GotifyAdapter struct {
	*notification.HTTPAdapter
	appToken string
	apiURL   string
}

// GotifyMessage represents a Gotify notification.
type GotifyMessage struct {
	Title    string            `json:"title"`
	Message  string            `json:"message"`
	Priority int               `json:"priority"`
	Extras   map[string]interface{} `json:"extras,omitempty"`
}

// GotifyResponse represents a Gotify API response.
type GotifyResponse struct {
	ID     int    `json:"id"`
	AppID  int    `json:"appId"`
	UserID int    `json:"userId"`
	Title  string `json:"title"`
	Message string `json:"message"`
	Time   string `json:"time"`
	Priority int  `json:"priority"`
}

// NewGotifyAdapter creates a new Gotify adapter.
func NewGotifyAdapter(config notification.AdapterConfig) (*GotifyAdapter, error) {
	base := notification.NewHTTPAdapter("gotify", config)
	if err := base.Validate(); err != nil {
		return nil, err
	}

	return &GotifyAdapter{
		HTTPAdapter: base,
		appToken:  config.Token,
		apiURL:    config.URL,
	}, nil
}

// Send delivers a message to Gotify.
func (a *GotifyAdapter) Send(msg *notification.Message) (*notification.DeliveryResult, error) {
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

	priority := 5
	switch msg.Event.Severity {
	case notification.SeverityInfo:
		priority = 3
	case notification.SeverityWarning:
		priority = 5
	case notification.SeverityError:
		priority = 7
	case notification.SeverityCritical:
		priority = 8
	}

	gotifyMsg := &GotifyMessage{
		Title:    msg.Title,
		Message:  msg.Content,
		Priority: priority,
		Extras: map[string]interface{}{
			"client::display": map[string]string{
				"contentType": "text/markdown",
			},
		},
	}

	bodyBuf, err := json.Marshal(gotifyMsg)
	if err != nil {
		return notification.BuildDeliveryResult(false, "", 0, err, start), nil
	}

	req, err := http.NewRequest("POST", a.apiURL+"/application/message", bytes.NewReader(bodyBuf))
	if err != nil {
		return notification.BuildDeliveryResult(false, "", 0, err, start), nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", a.appToken)

	resp, err := a.GetClient().Do(req)
	if err != nil {
		return notification.BuildDeliveryResult(false, "", 0, err, start), nil
	}
	defer resp.Body.Close()

	respBody := make([]byte, 0)
	if resp.Body != nil {
		respBody, _ = json.Marshal(map[string]interface{}{
			"status_code": resp.StatusCode,
		})
	}

	if resp.StatusCode != http.StatusOK {
		return notification.BuildDeliveryResult(false, "", resp.StatusCode, nil, start), nil
	}

	var gotifyResp GotifyResponse
	// Simple parse for message ID
	msgID := fmt.Sprintf("gt-%d", resp.StatusCode)
	if len(respBody) > 0 {
		_ = gotifyResp
		msgID = fmt.Sprintf("gt-message")
	}

	return notification.BuildDeliveryResult(true, msgID, resp.StatusCode, nil, start), nil
}

// SendBatch delivers a batch of messages to Gotify.
func (a *GotifyAdapter) SendBatch(messages []*notification.Message) ([]*notification.DeliveryResult, error) {
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

// Validate checks the Gotify adapter configuration.
func (a *GotifyAdapter) Validate() error {
	if a.apiURL == "" {
		return fmt.Errorf("gotify API URL is required")
	}
	if a.appToken == "" {
		return fmt.Errorf("gotify app token is required")
	}
	return nil
}
