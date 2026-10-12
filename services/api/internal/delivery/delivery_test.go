package delivery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTwilioRequest(t *testing.T) {
	var got struct {
		path, user, pass, to, from, body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.user, got.pass, _ = r.BasicAuth()
		r.ParseForm()
		got.to, got.from, got.body = r.Form.Get("To"), r.Form.Get("From"), r.Form.Get("Body")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	tw := &Twilio{AccountSID: "AC123", AuthToken: "secret", From: "+15550001111", BaseURL: srv.URL}
	if err := tw.Send(context.Background(), Message{To: "+15552223333", Body: "code 123456"}); err != nil {
		t.Fatal(err)
	}
	if got.path != "/2010-04-01/Accounts/AC123/Messages.json" || got.user != "AC123" || got.pass != "secret" {
		t.Fatalf("request = %+v", got)
	}
	if got.to != "+15552223333" || got.from != "+15550001111" || got.body != "code 123456" {
		t.Fatalf("form = %+v", got)
	}
}

func TestTwilioError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":21211,"message":"Invalid 'To' Phone Number"}`))
	}))
	defer srv.Close()
	err := (&Twilio{AccountSID: "AC1", From: "+1", BaseURL: srv.URL}).Send(context.Background(), Message{To: "+1", Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "21211") {
		t.Fatalf("err = %v, want Twilio error code", err)
	}
}

func TestHeaderInjectionRejected(t *testing.T) {
	r := &Recorder{}
	err := r.Send(context.Background(), Message{To: "a@example.org\r\nBcc: victim@example.org", Subject: "x"})
	if err != errHeaderInjection {
		t.Fatalf("err = %v", err)
	}
}

func TestSMTPRender(t *testing.T) {
	s := &SMTP{From: "Parley <no-reply@example.org>"}
	out := string(s.render(Message{To: "a@example.org", Subject: "Your code", Body: "line1\nline2"}, "no-reply@example.org"))
	for _, want := range []string{"From: Parley <no-reply@example.org>\r\n", "To: a@example.org\r\n", "Subject: Your code\r\n", "@example.org>\r\n", "\r\n\r\nline1\r\nline2\r\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered message missing %q:\n%s", want, out)
		}
	}
}
