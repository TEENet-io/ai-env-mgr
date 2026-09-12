package eventlog

import (
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
	case error:
		return secretValues.ReplaceAllString(x.Error(), "[redacted]")
	default:
		return v
	}
}
