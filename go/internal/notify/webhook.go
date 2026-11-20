package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Webhook formats. The generic one is ours; the other two are what the
// two common chat robots accept, signing included.
const (
	FormatGeneric  = "generic"
	FormatDingTalk = "dingtalk"
	FormatFeishu   = "feishu"
)

// Webhook posts one JSON body per message.
type Webhook struct {
	URL    string
	Format string
	Secret string // DingTalk / Feishu signing secret; empty for none
	Client *http.Client
	Now    func() time.Time
}

func (w Webhook) Name() string { return "webhook" }

// Send posts the message. A response outside 2xx is an error; DingTalk and
// Feishu also answer 200 with an error code in the body, which is read.
func (w Webhook) Send(ctx context.Context, m Message) error {
	if w.URL == "" {
		return errors.New("webhook: no URL")
	}
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	target, body, err := w.request(m, now)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer res.Body.Close()
	answer, readErr := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("webhook: %s: %s", res.Status, string(answer))
	}
	if w.Format == FormatDingTalk || w.Format == FormatFeishu {
		var reply struct {
			Code    int    `json:"code"`
			ErrCode int    `json:"errcode"`
			Msg     string `json:"msg"`
			ErrMsg  string `json:"errmsg"`
		}
		// The robots always answer JSON; an answer that cannot be read is
		// not proof the message was taken.
		if readErr != nil {
			return fmt.Errorf("webhook: reading the robot's answer: %w", readErr)
		}
		if err := json.Unmarshal(answer, &reply); err != nil {
			return fmt.Errorf("webhook: the robot's answer is not JSON: %.200s", answer)
		}
		if reply.Code != 0 || reply.ErrCode != 0 {
			return fmt.Errorf("webhook: robot refused: %d %s%s", reply.Code+reply.ErrCode, reply.Msg, reply.ErrMsg)
		}
	}
	return nil
}

// request builds the URL (with a signature where the format has one) and
// the body for the format.
func (w Webhook) request(m Message, now time.Time) (string, []byte, error) {
	switch w.Format {
	case FormatDingTalk:
		target := w.URL
		if w.Secret != "" {
			// The robot's own scheme: HMAC-SHA256 over "<ms>\n<secret>" with
			// the secret as the key, base64, then URL-encoded.
			ts := strconv.FormatInt(now.UnixMilli(), 10)
			mac := hmac.New(sha256.New, []byte(w.Secret))
			mac.Write([]byte(ts + "\n" + w.Secret))
			sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
			sep := "&"
			if !containsQuery(target) {
				sep = "?"
			}
			target += sep + "timestamp=" + ts + "&sign=" + url.QueryEscape(sign)
		}
		body, err := json.Marshal(map[string]any{
			"msgtype": "text", "text": map[string]string{"content": m.Text()},
		})
		return target, body, err
	case FormatFeishu:
		payload := map[string]any{
			"msg_type": "text", "content": map[string]string{"text": m.Text()},
		}
		if len(m.Fields) > 0 {
			payload = map[string]any{"msg_type": "interactive", "card": feishuCard(m)}
		}
		if w.Secret != "" {
			// Feishu signs with the key "<s>\n<secret>" over an empty message.
			ts := strconv.FormatInt(now.Unix(), 10)
			mac := hmac.New(sha256.New, []byte(ts+"\n"+w.Secret))
			payload["timestamp"] = ts
			payload["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
		}
		body, err := json.Marshal(payload)
		return w.URL, body, err
	case FormatGeneric, "":
		body, err := json.Marshal(map[string]any{
			"title": m.Title, "severity": m.Severity, "subject": m.Subject,
			"detail": m.Detail, "url": m.URL, "at": m.At.UTC().Format(time.RFC3339),
			"text": m.Text(),
		})
		return w.URL, body, err
	}
	return "", nil, fmt.Errorf("webhook: unknown format %q", w.Format)
}

// feishuCard is a notice as a Feishu message card: coloured header, one
// line per field, and a button to the page.
func feishuCard(m Message) map[string]any {
	color := m.Color
	if color == "" {
		color = "blue"
	}
	// Plain text, not Feishu's markdown: titles and names come from
	// customers, and must not turn into links or formatting.
	var lines []string
	for _, f := range m.Fields {
		lines = append(lines, f.Label+"："+f.Value)
	}
	elements := []any{map[string]any{"tag": "div", "text": map[string]string{"tag": "plain_text", "content": strings.Join(lines, "\n")}}}
	if m.URL != "" {
		label := m.Button
		if label == "" {
			label = "打開"
		}
		elements = append(elements, map[string]any{"tag": "action", "actions": []any{map[string]any{
			"tag": "button", "type": "primary", "url": m.URL,
			"text": map[string]string{"tag": "plain_text", "content": label},
		}}})
	}
	return map[string]any{
		"config":   map[string]bool{"wide_screen_mode": true},
		"header":   map[string]any{"template": color, "title": map[string]string{"tag": "plain_text", "content": m.Title}},
		"elements": elements,
	}
}

func containsQuery(u string) bool {
	parsed, err := url.Parse(u)
	return err == nil && parsed.RawQuery != ""
}
