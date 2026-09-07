package adminweb

import (
	"testing"
)

func TestGatewayRefusesWithoutConfiguration(t *testing.T) {
	// Rendering buttons that cannot work is worse than saying why.
	s := &Server{}
	if _, err := s.gateway(); err == nil {
		t.Fatal("expected an error when no gateway URL is set")
	}

	s = &Server{opts: Options{GatewayURL: "https://gw.example"}}
	_, err := s.gateway()
	if err == nil {
		t.Fatal("expected an error when the admin key is missing")
	}

	s = &Server{opts: Options{GatewayURL: "https://gw.example", GatewayAdminKey: "sk-admin"}}
	if _, err := s.gateway(); err != nil {
		t.Fatalf("fully configured gateway should build: %v", err)
	}
}

func TestContextWindowLabel(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{256000, "256K"},
		{128000, "128K"},
		{1048576, "1M"},
		{999, "999"},
		{0, "—"},
	} {
		if got := contextWindowLabel(tc.in); got != tc.want {
			t.Errorf("contextWindowLabel(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
