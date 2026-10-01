package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/i18n"
)

// email sends plain-text mail over SMTP with implicit TLS, STARTTLS
// (required, never silently downgraded) or, explicitly, no encryption.
type email struct{ n config.Notifier }

func (e *email) Send(ctx context.Context, m Message) error {
	s := e.n.SMTP
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	tlsConf := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	if s.Security == "tls" {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer c.Close()
	if s.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return &PermanentError{fmt.Errorf("%s does not offer STARTTLS; set smtp.security to \"tls\" or \"none\"", addr)}
		}
		if err := c.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if s.Username != "" {
		auth := smtp.PlainAuth("", s.Username, s.Password, s.Host)
		if err := c.Auth(auth); err != nil {
			return &PermanentError{fmt.Errorf("smtp auth: %w", err)}
		}
	}
	from, err := mail.ParseAddress(e.n.From)
	if err != nil {
		return &PermanentError{fmt.Errorf("invalid from address: %w", err)}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	var to []string
	for _, rcpt := range e.n.To {
		a, err := mail.ParseAddress(rcpt)
		if err != nil {
			return &PermanentError{fmt.Errorf("invalid recipient %q: %w", rcpt, err)}
		}
		if err := c.Rcpt(a.Address); err != nil {
			return err
		}
		to = append(to, a.String())
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(buildMail(from.String(), to, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func buildMail(from string, to []string, m Message) []byte {
	var b bytes.Buffer
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	host := "pharos.local"
	if at := strings.LastIndex(from, "@"); at >= 0 {
		host = strings.Trim(from[at+1:], "> ")
	}
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	hdr("From", from)
	hdr("To", strings.Join(to, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Title))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+host+">")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	if m.Count > 1 {
		hdr("X-Pharos-Event", "group")
	} else {
		hdr("X-Pharos-Event", string(m.Event.Kind))
		if m.Event.Monitor.ID != "" {
			hdr("X-Pharos-Monitor", m.Event.Monitor.ID)
		}
	}
	b.WriteString("\r\n")
	body := m.Body
	if m.Link != "" {
		body += "\n\n" + i18nOpen(m.Lang) + ": " + m.Link
	}
	body += "\n\n-- \nPharos\n"
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
	_ = qp.Close()
	return b.Bytes()
}

func i18nOpen(lang string) string { return i18n.T(lang, "notify.open") }
