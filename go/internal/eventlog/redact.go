package eventlog

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// Keys whose value is never written, whatever it looks like. Compared
// case-insensitively after removing '-' and '_'.
var secretKeys = map[string]bool{
	"authorization": true, "cookie": true, "setcookie": true, "password": true,
	"apikey": true, "accesskeyid": true, "accesskeysecret": true, "secret": true,
	"token": true, "bearer": true, "experimentalbearertoken": true, "masterkey": true,
	"litellmmasterkey": true, "aienvmgrgatewayadminkey": true,
}

// Value patterns: LiteLLM/OpenAI style keys and Alibaba Cloud AccessKey ids.
var secretValues = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{6,}|LTAI[A-Za-z0-9]{12,}`)

func normKey(k string) string {
	return strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(k))
}

// Redact returns a copy of fields with secret keys replaced by "[redacted]"
// and secret-looking substrings removed from string values, recursively.
// The input is not modified.
func Redact(fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		if secretKeys[normKey(k)] {
			out[k] = "[redacted]"
			continue
		}
		out[k] = redactValue(v)
	}
	return out
}

func redactValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return secretValues.ReplaceAllString(x, "[redacted]")
	case map[string]any:
		return Redact(x)
	case []any:
		c := make([]any, len(x))
		for i := range x {
			c[i] = redactValue(x[i])
		}
		return c
	case []string:
		c := make([]string, len(x))
		for i := range x {
			c[i] = secretValues.ReplaceAllString(x[i], "[redacted]")
		}
		return c
	case map[string]string:
		c := make(map[string]string, len(x))
		for k, sv := range x {
			if secretKeys[normKey(k)] {
				c[k] = "[redacted]"
				continue
			}
			c[k] = secretValues.ReplaceAllString(sv, "[redacted]")
		}
		return c
	case error:
		return secretValues.ReplaceAllString(x.Error(), "[redacted]")
	default:
		return redactComposite(v)
	}
}

// redactComposite covers every remaining shape a caller can hand us: a
// []int, a struct with JSON tags, a *string, a json.RawMessage, a
// map[string]SomeType, a pointer to any of those.
//
// Listing those types case by case is a losing game -- the one that gets
// forgotten is the one that leaks -- so instead the value is put through
// exactly the encoder that would have written it. What json.Marshal produces
// is what would have landed in the file, and unmarshalling that back into
// `any` yields only maps, slices, strings, numbers and bools, which the
// cases above already handle by key and by value. Re-encoding the redacted
// result later reproduces the same JSON, minus the secrets.
//
// Scalars json writes verbatim (numbers, bools) cannot carry a secret and
// are returned untouched, so the common case pays nothing.
func redactComposite(v any) any {
	// Enumerating what to inspect is the losing game again, so enumerate what
	// is safe instead: the kinds json writes as a bare number or bool, which
	// have nowhere to hide a string. Everything else -- slices, arrays, maps,
	// structs, pointers, and the kinds json refuses outright -- goes through.
	switch reflect.ValueOf(v).Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return v
	}
	b, err := json.Marshal(v)
	if err != nil {
		// A channel, a func, a cycle: json.Marshal in emit would fail on the
		// whole event and the line would be lost entirely. Keep the event and
		// write the value's printed form instead, scrubbed by value.
		return secretValues.ReplaceAllString(fmt.Sprintf("%v", v), "[redacted]")
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return secretValues.ReplaceAllString(fmt.Sprintf("%v", v), "[redacted]")
	}
	return redactValue(generic)
}
