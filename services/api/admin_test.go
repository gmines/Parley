package main

import "testing"

func TestAdminListensOnLoopbackOnly(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8090": true,
		"[::1]:8090":     true,
		"localhost:8090": true,
		":8090":          false,
		"0.0.0.0:8090":   false,
		"10.0.0.5:8090":  false,
		"[::]:8090":      false,
		"8090":           false,
	} {
		if err := loopbackOnly(addr); (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", addr, err, ok)
		}
	}
}
