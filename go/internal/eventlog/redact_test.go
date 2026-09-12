package eventlog

import "testing"

func TestRedactByKeyAndByValue(t *testing.T) {
	in := map[string]any{
		"Authorization":             "Bearer abc",
		"cookie":                    "session=1",
		"access_key_secret":         "xyz",
		"experimental_bearer_token": "sk-111",
		"note":                      "token sk-abcdefghijklmnop was used, AK LTAI5tABCDEFGHIJKLMN too",
		"employee_id":               "alice",
		"count":                     3,
		"nested":                    map[string]any{"api_key": "k", "fine": "v"},
	}
	out := Redact(in)
	for _, k := range []string{"Authorization", "cookie", "access_key_secret", "experimental_bearer_token"} {
		if out[k] != "[redacted]" {
			t.Errorf("%s not redacted: %v", k, out[k])
		}
	}
	if out["note"] != "token [redacted] was used, AK [redacted] too" {
		t.Errorf("value redaction: %v", out["note"])
	}
	if out["employee_id"] != "alice" || out["count"] != 3 {
		t.Errorf("plain fields changed: %v", out)
	}
	n := out["nested"].(map[string]any)
	if n["api_key"] != "[redacted]" || n["fine"] != "v" {
		t.Errorf("nested: %v", n)
	}
	if in["Authorization"] != "Bearer abc" {
		t.Errorf("input must not be mutated")
	}
}
