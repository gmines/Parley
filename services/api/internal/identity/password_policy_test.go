package identity

import (
	"errors"
	"strings"
	"testing"
)

func TestPasswordStrengthRejects(t *testing.T) {
	cases := []struct {
		password, username, displayName string
		hint                            string // substring expected in the hint
	}{
		{"12345678", "dave", "Dave", "common"},
		{"password123", "dave", "Dave", "common"},
		{"Password1!", "dave", "Dave", "common"},
		{"P@ssw0rd2024", "dave", "Dave", "common"},
		{"qwertyuiop", "dave", "Dave", "common"},
		{"iloveyou2", "dave", "Dave", "common"},
		{"abcdefghij", "dave", "Dave", "sequence"},
		{"zzzzzzzzzz", "dave", "Dave", "repeat"},
		{"davidsmith1", "davidsmith", "David Smith", "username"},
		{"parleychat1", "erin", "Erin", "username"}, // product name
		{"mxK7#pQ2", "dave", "Dave", "short"},
	}
	for _, c := range cases {
		err := checkPasswordStrength(c.password, c.username, c.displayName)
		var weak *WeakPasswordError
		if !errors.As(err, &weak) {
			t.Errorf("%q accepted, want rejected", c.password)
			continue
		}
		if !errors.Is(err, ErrWeakPassword) {
			t.Errorf("%q: error does not match ErrWeakPassword", c.password)
		}
		if !strings.Contains(weak.Hint, c.hint) {
			t.Errorf("%q: hint %q, want it to mention %q", c.password, weak.Hint, c.hint)
		}
	}
}

func TestPasswordStrengthAccepts(t *testing.T) {
	for _, p := range []string{
		"blue-kettle-drift",
		"my dog has fleas!",
		"correct horse battery staple",
		"plum-orbit-candle-42",
		"Vq9#tL2m!xR4",
	} {
		if err := checkPasswordStrength(p, "dave", "Dave"); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
}
