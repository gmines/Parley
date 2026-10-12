package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Twilio sends SMS through Twilio's Messages API.
type Twilio struct {
	AccountSID string
	AuthToken  string
	From       string // a Twilio number in E.164 form, or a Messaging Service SID (MG...)
	BaseURL    string // defaults to https://api.twilio.com; overridden in tests
	HTTP       *http.Client
}

func (t *Twilio) Send(ctx context.Context, m Message) error {
	if err := checkHeaders(m); err != nil {
		return err
	}
	base := t.BaseURL
	if base == "" {
		base = "https://api.twilio.com"
	}
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	form := url.Values{"To": {m.To}, "Body": {m.Body}}
	if strings.HasPrefix(t.From, "MG") {
		form.Set("MessagingServiceSid", t.From)
	} else {
		form.Set("From", t.From)
	}
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json", base, url.PathEscape(t.AccountSID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(t.AccountSID, t.AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("twilio: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	return fmt.Errorf("twilio: status %d: code %d: %s", resp.StatusCode, body.Code, body.Message)
}
