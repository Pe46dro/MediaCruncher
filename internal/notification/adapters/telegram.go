package adapters

import (
	"fmt"
	"net/http"
	"time"

	"mediacruncher/internal/notification"
)

// TelegramAdapter delivers notifications to Telegram via Bot API.
type TelegramAdapter struct {
	*notification.HTTPAdapter
	token string
}

// TelegramMessage represents a Telegram Bot API message.
type TelegramMessage struct {
	ChatID      string           `json:"chat_id"`
	Text        string           `json:"text"`
	ParseMode   string           `json:"parse_mode,omitempty"`
	ReplyMarkup *TelegramMarkup  `json:"reply_markup,omitempty"`
}

// TelegramMarkup represents inline keyboard markup.
type TelegramMarkup struct {
	InlineKeyboard [][]TelegramButton `json:"inline_keyboard"`
}

// TelegramButton represents an inline keyboard button.
type TelegramButton struct {
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
}

// NewTelegramAdapter creates a new Telegram adapter.
func NewTelegramAdapter(config notification.AdapterConfig) (*TelegramAdapter, error) {
	if config.Token == "" || config.ChannelID == "" {
		return nil, fmt.Errorf("telegram token and channel_id are required")
	}

	base := notification.NewHTTPAdapter("telegram", config)
	if err := base.Validate(); err != nil {
		return nil, err
	}

	adapter := &TelegramAdapter{
		HTTPAdapter: base,
		token:       config.Token,
	}

	return adapter, nil
}

// Send delivers a message to Telegram.
func (a *TelegramAdapter) Send(msg *notification.Message) (*notification.DeliveryResult, error) {
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
	chatID := a.GetConfig().ChannelID
	if chatID == "" {
		chatID = "none"
	}

	tgMsg := &TelegramMessage{
		ChatID:    chatID,
		Text:      fmt.Sprintf("%s\n\n%s", msg.Title, msg.Content),
		ParseMode: "HTML",
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", a.token)
	resp, _, err := a.DoPOST(url, tgMsg)
	if err != nil {
		return notification.BuildDeliveryResult(false, "", 0, err, start), nil
	}

	if resp.StatusCode != http.StatusOK {
		return notification.BuildDeliveryResult(false, "", resp.StatusCode, nil, start), nil
	}

	return notification.BuildDeliveryResult(true, fmt.Sprintf("tg-%s", chatID), resp.StatusCode, nil, start), nil
}

// SendBatch delivers a batch of messages to Telegram.
func (a *TelegramAdapter) SendBatch(messages []*notification.Message) ([]*notification.DeliveryResult, error) {
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

// Validate checks the Telegram adapter configuration.
func (a *TelegramAdapter) Validate() error {
	if a.token == "" {
		return fmt.Errorf("telegram token is required")
	}
	if a.GetConfig().ChannelID == "" {
		return fmt.Errorf("telegram channel_id is required")
	}
	return nil
}
