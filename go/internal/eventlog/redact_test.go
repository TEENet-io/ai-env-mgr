package eventlog

import (
	"encoding/json"
	"strings"
	"testing"
)

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

// TestRedactReachesEveryJSONSerialisableShape pins the rule that matters:
// whatever json.Marshal would have written, Redact has already seen. A type
// family that slips through here is a secret in the log file.
func TestRedactReachesEveryJSONSerialisableShape(t *testing.T) {
	type tagged struct {
		Token string `json:"token"`
		Note  string `json:"note"`
		Keep  int    `json:"keep"`
	}
	secret := "sk-leaked-via-pointer"
	raw := json.RawMessage(`{"api_key":"sk-leaked-via-raw","fine":1}`)

	cases := []struct {
		name string
		in   any
	}{
		{"string slice", []string{"sk-leaked-via-stringslice", "plain"}},
		{"string map", map[string]string{"authorization": "Bearer x", "detail": "sk-leaked-via-stringmap"}},
		{"struct with json tags", tagged{Token: "sk-leaked-via-struct", Note: "sk-leaked-in-note", Keep: 7}},
		{"pointer to struct", &tagged{Token: "sk-leaked-via-ptrstruct"}},
		{"string pointer", &secret},
		{"raw message", raw},
		{"nested slice of maps", []map[string]string{{"api_key": "sk-leaked-via-slicemap"}}},
		{"slice of structs", []tagged{{Token: "sk-leaked-via-structslice"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Redact(map[string]any{"field": c.in})
			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal redacted: %v", err)
			}
			if strings.Contains(string(encoded), "sk-") {
				t.Fatalf("secret survived: %s", encoded)
			}
			if !strings.Contains(string(encoded), "[redacted]") {
				t.Fatalf("nothing was redacted: %s", encoded)
			}
		})
	}
}

// Redaction must not cost the caller their data: everything that is not a
// secret has to come back, and in the shape json would have written.
func TestRedactKeepsHarmlessCompositesIntact(t *testing.T) {
	type row struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	out := Redact(map[string]any{
		"rows":    []row{{Name: "alice", Count: 2}},
		"numbers": []int{1, 2, 3},
		"flag":    true,
		"n":       42,
	})
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"flag":true,"n":42,"numbers":[1,2,3],"rows":[{"count":2,"name":"alice"}]}`
	if string(encoded) != want {
		t.Errorf("composites changed:\n got %s\nwant %s", encoded, want)
	}
}

// A value json cannot encode used to lose the whole event, because
// json.Marshal in emit failed on the map as a whole. It is now stringified.
func TestRedactStringifiesWhatJSONCannotEncode(t *testing.T) {
	out := Redact(map[string]any{"ch": make(chan int)})
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("event must still encode: %v", err)
	}
}
