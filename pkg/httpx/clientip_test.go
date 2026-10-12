package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	trusted, err := ParsePrefixes("10.0.0.0/8, 192.168.1.5")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, remote, xff, want string
	}{
		{"direct, no header", "203.0.113.7:5555", "", "203.0.113.7"},
		{"untrusted peer can't spoof", "203.0.113.7:5555", "1.2.3.4", "203.0.113.7"},
		{"trusted proxy", "10.1.2.3:80", "198.51.100.9", "198.51.100.9"},
		{"spoofed left-most entry ignored", "10.1.2.3:80", "1.2.3.4, 198.51.100.9", "198.51.100.9"},
		{"chain of trusted proxies", "10.1.2.3:80", "198.51.100.9, 192.168.1.5, 10.9.9.9", "198.51.100.9"},
		{"trusted proxy, no header", "10.1.2.3:80", "", "10.1.2.3"},
		{"garbage header falls back to peer", "10.1.2.3:80", "not-an-ip", "10.1.2.3"},
		{"ipv6 peer", "[2001:db8::1]:443", "", "2001:db8::1"},
		{"ipv4-mapped ipv6", "[::ffff:203.0.113.7]:443", "", "203.0.113.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := ClientIP(r, trusted); got != c.want {
				t.Fatalf("ClientIP = %q, want %q", got, c.want)
			}
		})
	}
}
