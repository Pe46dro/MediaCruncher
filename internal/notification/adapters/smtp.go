package adapters

import (
	"fmt"
	"net/smtp"
	"strings"
	"time"

	"mediacruncher/internal/notification"
)

// SMTPAdapter delivers notifications via SMTP email.
type SMTPAdapter struct {
	*notification.HTTPAdapter
	host     string
	port     int
	username string
	password string
	toAddrs  []string
}

// NewSMTPAdapter creates a new SMTP adapter.
func NewSMTPAdapter(config notification.AdapterConfig) (*SMTPAdapter, error) {
	base := notification.NewHTTPAdapter("smtp", config)
	if err := base.Validate(); err != nil {
		return nil, err
	}

	port := config.SMTPPort
	if port <= 0 {
		port = 587
	}

	return &SMTPAdapter{
		HTTPAdapter: base,
		host:        config.SMTPHost,
		port:        port,
		username:    config.SMTPUser,
		password:    config.SMTPPass,
		toAddrs:     config.ToAddresses,
	}, nil
}

// Send delivers an email via SMTP.
func (a *SMTPAdapter) Send(msg *notification.Message) (*notification.DeliveryResult, error) {
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

	if len(a.toAddrs) == 0 {
		return notification.BuildDeliveryResult(false, "", 0, fmt.Errorf("no recipient addresses configured"), start), nil
	}

	subject := msg.Title
	body := fmt.Sprintf("Subject: %s\r\n\r\n%s", subject, msg.Content)

	auth := smtp.PlainAuth("", a.username, a.password, a.host)
	addr := fmt.Sprintf("%s:%d", a.host, a.port)
	to := a.toAddrs

	err := smtp.SendMail(addr, auth, a.username, to, []byte(body))
	if err != nil {
		return notification.BuildDeliveryResult(false, "", 0, err, start), nil
	}

	return notification.BuildDeliveryResult(true, fmt.Sprintf("smtp-%s", msg.Title), 250, nil, start), nil
}

// SendBatch delivers a batch of messages via SMTP.
func (a *SMTPAdapter) SendBatch(messages []*notification.Message) ([]*notification.DeliveryResult, error) {
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

// Validate checks the SMTP adapter configuration.
func (a *SMTPAdapter) Validate() error {
	if a.host == "" {
		return fmt.Errorf("smtp host is required")
	}
	if len(a.toAddrs) == 0 {
		return fmt.Errorf("at least one to address is required")
	}
	return nil
}

// BuildEmailBody creates the email body from a message.
func BuildEmailBody(msg *notification.Message) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Subject: %s\r\n", msg.Title))
	sb.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(msg.Content)
	if msg.Footer != "" {
		sb.WriteString("\r\n\r\n")
		sb.WriteString(msg.Footer)
	}
	return sb.String()
}
