package config

import "testing"

func TestCheckConsoleURL(t *testing.T) {
	ok := []string{
		"https://windows-control.teenet.app",
		"https://console.example.com:8443",
		"http://127.0.0.1:8091",
		"http://localhost:8091",
		"http://[::1]:8091",
	}
	for _, raw := range ok {
		if err := CheckConsoleURL(raw); err != nil {
			t.Errorf("CheckConsoleURL(%q) = %v, want nil", raw, err)
		}
	}
	bad := []string{
		"",
		"windows-control.teenet.app",
		"http://windows-control.teenet.app",
		"http://10.0.0.5:8091",
		"https://windows-control.teenet.app/",
		"https://windows-control.teenet.app/agent",
		"https://user:pw@windows-control.teenet.app",
		"https://windows-control.teenet.app?x=1",
		"https://windows-control.teenet.app#x",
		"https://",
		"ftp://windows-control.teenet.app",
		"http://localhost.evil.com",
	}
	for _, raw := range bad {
		if err := CheckConsoleURL(raw); err == nil {
			t.Errorf("CheckConsoleURL(%q) = nil, want an error", raw)
		}
	}
}
