package delivery

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTP sends email through a relay. TLS is mandatory: either implicit TLS
// (usually port 465) or STARTTLS (usually 587). Plain-text SMTP is refused.
type SMTP struct {
	Host        string
	Port        int
	Username    string // empty disables AUTH
	Password    string
	From        string // e.g. "Parley <no-reply@example.org>"
	ImplicitTLS bool
	Timeout     time.Duration
}

func (s *SMTP) Send(ctx context.Context, m Message) error {
	if err := checkHeaders(m); err != nil {
		return err
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < timeout {
		timeout = time.Until(dl)
	}

	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	tlsCfg := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Timeout: timeout}

	var conn net.Conn
	var err error
	if s.ImplicitTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	conn.SetDeadline(time.Now().Add(timeout))
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer c.Close()

	if !s.ImplicitTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: server does not offer STARTTLS; refusing to send in plain text")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	fromAddr := s.From
	if i := strings.LastIndex(fromAddr, "<"); i >= 0 {
		fromAddr = strings.TrimSuffix(fromAddr[i+1:], ">")
	}
	if err := c.Mail(fromAddr); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(s.render(m, fromAddr)); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return c.Quit()
}

func (s *SMTP) render(m Message, fromAddr string) []byte {
	id := make([]byte, 12)
	rand.Read(id)
	domain := fromAddr[strings.LastIndex(fromAddr, "@")+1:]
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.From)
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), domain)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(m.Body, "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}
