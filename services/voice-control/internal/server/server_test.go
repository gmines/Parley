package server

import (
	"errors"
	"testing"
)

func TestProxyFor(t *testing.T) {
	const tls, tcp = "sip:voice.example.org:5061;transport=tls", "sip:203.0.113.5:5060;transport=tcp"
	cases := []struct {
		name            string
		proxyTLS, proxy string
		transports      []string
		want            string
		wantErr         error
	}{
		{"TLS client gets TLS", tls, tcp, []string{"tls", "tcp"}, tls, nil},
		{"old client gets plain TCP", tls, tcp, nil, tcp, nil},
		{"old client refused when TLS only", tls, "", nil, "", errUpgradeRequired},
		{"TLS client on a plain-only server", "", tcp, []string{"tls", "tcp"}, tcp, nil},
		{"TLS-only client on a plain-only server", "", tcp, []string{"tls"}, "", errUpgradeRequired},
		{"unknown transports", tls, tcp, []string{"udp"}, "", errUpgradeRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{SIPProxy: c.proxy, SIPProxyTLS: c.proxyTLS}
			got, err := s.proxyFor(c.transports)
			if got != c.want || !errors.Is(err, c.wantErr) {
				t.Fatalf("proxyFor(%v) = %q, %v; want %q, %v", c.transports, got, err, c.want, c.wantErr)
			}
		})
	}
}
