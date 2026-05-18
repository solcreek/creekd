package main

import "testing"

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"127.0.0.1:9080", true},
		{"localhost:9080", true},
		{"[::1]:9080", true},
		{"0.0.0.0:9080", false},
		{"192.168.1.5:9080", false},
		{":9080", false},     // empty host == any interface
		{"example.com:80", false},
		{"not-an-addr", false}, // malformed
	}
	for _, c := range cases {
		if got := isLoopback(c.in); got != c.want {
			t.Errorf("isLoopback(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("CREEKD_TEST_VAR", "")
	if got := envOr("CREEKD_TEST_VAR", "fallback"); got != "fallback" {
		t.Errorf("empty env: got %q, want fallback", got)
	}
	t.Setenv("CREEKD_TEST_VAR", "set")
	if got := envOr("CREEKD_TEST_VAR", "fallback"); got != "set" {
		t.Errorf("set env: got %q, want set", got)
	}
}
