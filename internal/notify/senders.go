package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/config"
)

// Sender delivers a message through one channel.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// PermanentError marks a failure that retrying will not fix, such as a
// rejected webhook URL (HTTP 4xx other than 429).
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// UserAgent is sent with every HTTP notification.
var UserAgent = "Pharos"

var httpClient = &http.Client{Timeout: 15 * time.Second}

// NewSender builds the sender for a notifier configuration.
func NewSender(n config.Notifier) (Sender, error) {
	switch n.Type {
	case config.NotifyWebhook:
		return &webhook{n: n}, nil
	case config.NotifySlack:
		return &slack{url: n.URL}, nil
	case config.NotifyDiscord:
		return &discord{url: n.URL}, nil
	case config.NotifyTelegram:
		return &telegram{token: n.Token, chatID: n.ChatID, api: "https://api.telegram.org"}, nil
	case config.NotifyNtfy:
		return newNtfy(n)
	case config.NotifyEmail:
		return &email{n: n}, nil
	}
	return nil, fmt.Errorf("unknown notifier type %q", n.Type)
}

func postJSON(ctx context.Context, rawURL string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return &PermanentError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return redact(err, rawURL)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
		return &PermanentError{err}
	}
	return err
}

// redact removes the URL (which often embeds a secret token) from an error.
func redact(err error, rawURL string) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s request failed: %w", ue.Op, ue.Err)
	}
	return errors.New(strings.ReplaceAll(err.Error(), rawURL, "[notifier url]"))
}

// webhook posts a JSON document describing the event, optionally signed.
type webhook struct{ n config.Notifier }

// WebhookPayload is the JSON body sent by the webhook notifier. Its shape is
// part of Pharos's public API.
type WebhookPayload struct {
	Event           string            `json:"event"`
	At              time.Time         `json:"at"`
	Status          string            `json:"status"`
	PreviousStatus  string            `json:"previous_status"`
	Monitor         map[string]string `json:"monitor"`
	Title           string            `json:"title"`
	Text            string            `json:"text"`
	Message         string            `json:"message,omitempty"`
	Incident        any               `json:"incident,omitempty"`
	DurationSeconds float64           `json:"duration_seconds,omitempty"`
	CertExpiry      *time.Time        `json:"cert_expiry,omitempty"`
	URL             string            `json:"url,omitempty"`
}

func (w *webhook) Send(ctx context.Context, m Message) error {
	ev := m.Event
	p := WebhookPayload{
		Event: string(ev.Kind), At: ev.At.UTC(), Status: ev.Status.String(), PreviousStatus: ev.Previous.String(),
		Monitor: map[string]string{"id": ev.Monitor.ID, "name": ev.Monitor.Name, "type": ev.Monitor.Type, "target": ev.Monitor.Target},
		Title:   m.Title, Text: m.Body, Message: ev.Message, URL: m.Link,
		DurationSeconds: ev.Duration.Seconds(),
	}
	if ev.Incident != nil {
		p.Incident = ev.Incident
	}
	if !ev.CertExpiry.IsZero() {
		t := ev.CertExpiry.UTC()
		p.CertExpiry = &t
	}
	body, err := json.Marshal(p)
	if err != nil {
		return &PermanentError{err}
	}
	headers := map[string]string{"X-Pharos-Event": string(ev.Kind)}
	for k, v := range w.n.Headers {
		headers[k] = v
	}
	if w.n.Secret != "" {
		mac := hmac.New(sha256.New, []byte(w.n.Secret))
		mac.Write(body)
		headers["X-Pharos-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	return postJSON(ctx, w.n.URL, body, headers)
}

func colorOf(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0xd03b3b
	case SeverityWarning:
		return 0xcc8a00
	case SeverityGood:
		return 0x2f9e44
	}
	return 0x2a78d6
}

// slack posts to a Slack incoming webhook.
type slack struct{ url string }

func (s *slack) Send(ctx context.Context, m Message) error {
	text := "*" + slackEscape(m.Title) + "*\n" + slackEscape(m.Body)
	blocks := []any{map[string]any{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": text}}}
	if m.Link != "" {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []any{
			map[string]string{"type": "mrkdwn", "text": "<" + m.Link + "|" + slackEscape(i18nOpen(m.Lang)) + ">"},
		}})
	}
	body, _ := json.Marshal(map[string]any{"text": m.Title, "blocks": blocks})
	return postJSON(ctx, s.url, body, nil)
}

func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// discord posts an embed to a Discord webhook.
type discord struct{ url string }

func (d *discord) Send(ctx context.Context, m Message) error {
	embed := map[string]any{
		"title":       truncate(m.Title, 256),
		"description": truncate(m.Body, 4000),
		"color":       colorOf(m.Severity),
		"timestamp":   m.Event.At.UTC().Format(time.RFC3339),
		"footer":      map[string]string{"text": "Pharos"},
	}
	if m.Link != "" {
		embed["url"] = m.Link
	}
	body, _ := json.Marshal(map[string]any{"username": "Pharos", "embeds": []any{embed}, "allowed_mentions": map[string]any{"parse": []string{}}})
	return postJSON(ctx, d.url, body, nil)
}

// telegram sends a message through the Bot API.
type telegram struct {
	token, chatID string
	api           string
}

func (t *telegram) Send(ctx context.Context, m Message) error {
	text := "<b>" + html.EscapeString(m.Title) + "</b>\n" + html.EscapeString(m.Body)
	if m.Link != "" {
		text += "\n<a href=\"" + html.EscapeString(m.Link) + "\">" + html.EscapeString(i18nOpen(m.Lang)) + "</a>"
	}
	body, _ := json.Marshal(map[string]any{"chat_id": t.chatID, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true})
	err := postJSON(ctx, t.api+"/bot"+t.token+"/sendMessage", body, nil)
	if err == nil {
		return nil
	}
	// Never leak the bot token into logs, but keep the retry decision.
	redacted := errors.New(strings.ReplaceAll(err.Error(), t.token, "[token]"))
	var perm *PermanentError
	if errors.As(err, &perm) {
		return &PermanentError{redacted}
	}
	return redacted
}

// ntfy publishes to an ntfy topic using the JSON API, which carries
// non-ASCII titles safely (HTTP headers cannot).
type ntfy struct {
	server, topic, token string
}

func newNtfy(n config.Notifier) (*ntfy, error) {
	u, err := url.Parse(n.URL)
	if err != nil {
		return nil, err
	}
	topic := strings.Trim(u.Path, "/")
	if topic == "" || strings.Contains(topic, "/") {
		return nil, fmt.Errorf("ntfy url must be a topic URL such as https://ntfy.sh/my-alerts")
	}
	u.Path = ""
	return &ntfy{server: u.String(), topic: topic, token: n.Token}, nil
}

func (n *ntfy) Send(ctx context.Context, m Message) error {
	prio, tags := 3, []string{"information_source"}
	switch m.Severity {
	case SeverityCritical:
		prio, tags = 5, []string{"rotating_light"}
	case SeverityWarning:
		prio, tags = 4, []string{"warning"}
	case SeverityGood:
		prio, tags = 3, []string{"white_check_mark"}
	}
	payload := map[string]any{"topic": n.topic, "title": m.Title, "message": m.Body, "priority": prio, "tags": tags}
	if m.Link != "" {
		payload["click"] = m.Link
	}
	body, _ := json.Marshal(payload)
	var headers map[string]string
	if n.token != "" {
		headers = map[string]string{"Authorization": "Bearer " + n.token}
	}
	return postJSON(ctx, n.server, body, headers)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
