package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const goodToken = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"

func setup(t *testing.T, upstream http.HandlerFunc) *httptest.Server {
	t.Helper()
	api := httptest.NewServer(upstream)
	t.Cleanup(api.Close)
	web := httptest.NewServer(newServer(api.URL, "account.example.org").handler())
	t.Cleanup(web.Close)
	return web
}

func post(t *testing.T, url, contentType, origin, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestOnlyKnownPathsAndHeaders(t *testing.T) {
	web := setup(t, func(http.ResponseWriter, *http.Request) { t.Error("upstream called") })
	resp, err := http.Get(web.URL + "/account/set-password")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe") {
		t.Fatalf("status %d csp %q", resp.StatusCode, csp)
	}
	for _, p := range []string{"/", "/internal/v1/links/inspect", "/account/", "/account/../static/index.html", "/.git/config", "/v1/users/me"} {
		r, err := http.Get(web.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 404 {
			t.Errorf("GET %s: %d, want 404", p, r.StatusCode)
		}
	}
}

func TestRelayRejectsBeforeUpstream(t *testing.T) {
	web := setup(t, func(http.ResponseWriter, *http.Request) { t.Error("upstream called") })
	url := web.URL + "/account/api/inspect"
	good := `{"token":"` + goodToken + `"}`
	cases := []struct {
		name, ct, origin, body string
		want                   int
	}{
		{"form post", "application/x-www-form-urlencoded", "", good, 415},
		{"foreign origin", "application/json", "https://evil.example", good, 403},
		{"malformed token", "application/json", "", `{"token":"../../etc/passwd"}`, 400},
		{"oversized", "application/json", "", `{"token":"` + strings.Repeat("a", 5000) + `"}`, 400},
		{"not json", "application/json", "", "nope", 400},
	}
	for _, c := range cases {
		if r := post(t, url, c.ct, c.origin, c.body); r.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, r.StatusCode, c.want)
		}
	}
}

func TestRelayForwardsOnlyKnownFields(t *testing.T) {
	web := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/links/complete" {
			t.Errorf("path %s", r.URL.Path)
		}
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		if len(got) != 2 || got["token"] != goodToken || got["new_password"] != "plum-orbit" {
			t.Errorf("forwarded %v", got)
		}
		w.Header().Set("X-Internal", "secret")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"code":"weak_password","message":"too common","extra":"internal"}`)
	})
	r := post(t, web.URL+"/account/api/complete", "application/json", "https://account.example.org",
		`{"token":"`+goodToken+`","new_password":"plum-orbit","url":"http://evil/","admin":true}`)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 400 || r.Header.Get("X-Internal") != "" || strings.Contains(string(b), "internal") {
		t.Fatalf("status %d headers %v body %s", r.StatusCode, r.Header, b)
	}
}

func TestUpstreamFailureIsOpaque(t *testing.T) {
	web := setup(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "pgx: connection to 10.0.0.5 refused", 500)
	})
	r := post(t, web.URL+"/account/api/inspect", "application/json", "", `{"token":"`+goodToken+`"}`)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 502 || strings.Contains(string(b), "pgx") {
		t.Fatalf("status %d body %s", r.StatusCode, b)
	}
}
