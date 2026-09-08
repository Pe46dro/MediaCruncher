package adapters

import (
	"crypto/tls"
	"fmt"
	"net"
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

// Send delivers an email via SMTP with explicit TLS (STARTTLS or SMTPS).
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

	addr := fmt.Sprintf("%s:%d", a.host, a.port)
	auth := smtp.PlainAuth("", a.username, a.password, a.host)
	to := a.toAddrs

	var deliverErr error

	if a.port == 465 {
		deliverErr = a.sendSMTPS(addr, auth, to, body)
	} else {
		deliverErr = a.sendSTARTTLS(addr, auth, to, body)
	}

	if deliverErr != nil {
		return notification.BuildDeliveryResult(false, "", 0, deliverErr, start), nil
	}

	return notification.BuildDeliveryResult(true, fmt.Sprintf("smtp-%s", msg.Title), 250, nil, start), nil
}

// sendSMTPS delivers via SMTPS (implicit TLS on port 465).
func (a *SMTPAdapter) sendSMTPS(addr string, auth smtp.Auth, to []string, body string) error {
	tlsConfig := &tls.Config{
		ServerName: a.host,
		MinVersion: tls.VersionTLS12,
	}
	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("SMTPS TLS dial: %w", err)
	}
	client, err := smtp.NewClient(conn, a.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SMTPS smtp client: %w", err)
	}
	if err := client.Auth(auth); err != nil {
		client.Close()
		return fmt.Errorf("SMTPS auth: %w", err)
	}
	if err := client.Mail(a.username); err != nil {
		client.Close()
		return fmt.Errorf("SMTPS mail: %w", err)
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			client.Close()
			return fmt.Errorf("SMTPS rcpt %s: %w", recipient, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		client.Close()
		return fmt.Errorf("SMTPS data: %w", err)
	}
	_, err = w.Write([]byte(body))
	if err != nil {
		client.Close()
		return fmt.Errorf("SMTPS write data: %w", err)
	}
	if err := w.Close(); err != nil {
		client.Close()
		return fmt.Errorf("SMTPS close data: %w", err)
	}
	client.Close()
	return nil
}

// sendSTARTTLS delivers via STARTTLS (port 587 with explicit TLS upgrade).
func (a *SMTPAdapter) sendSTARTTLS(addr string, auth smtp.Auth, to []string, body string) error {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("STARTTLS dial: %w", err)
	}

	client, err := smtp.NewClient(conn, a.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("STARTTLS smtp client: %w", err)
	}

	tlsConfig := &tls.Config{
		ServerName: a.host,
		MinVersion: tls.VersionTLS12,
	}

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(tlsConfig); err != nil {
			client.Close()
			return fmt.Errorf("STARTTLS failed: %w", err)
		}
	} else {
		client.Close()
		return fmt.Errorf("server does not support STARTTLS on port %d", a.port)
	}

	if err := client.Auth(auth); err != nil {
		client.Close()
		return fmt.Errorf("STARTTLS auth: %w", err)
	}
	if err := client.Mail(a.username); err != nil {
		client.Close()
		return fmt.Errorf("STARTTLS mail: %w", err)
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			client.Close()
			return fmt.Errorf("STARTTLS rcpt %s: %w", recipient, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		client.Close()
		return fmt.Errorf("STARTTLS data: %w", err)
	}
	_, err = w.Write([]byte(body))
	if err != nil {
		client.Close()
		return fmt.Errorf("STARTTLS write data: %w", err)
	}
	if err := w.Close(); err != nil {
		client.Close()
		return fmt.Errorf("STARTTLS close data: %w", err)
	}
	client.Close()
	return nil
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
