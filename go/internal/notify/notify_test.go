package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhookFormats(t *testing.T) {
	var got struct {
		path, query string
		body        map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.query = r.URL.Path, r.URL.RawQuery
		raw, _ := io.ReadAll(r.Body)
		got.body = map[string]any{}
		json.Unmarshal(raw, &got.body)
		w.Write([]byte(`{"errcode":0,"code":0}`))
	}))
	defer srv.Close()
	at := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	m := Message{Title: "机器 PC-1 已 30 小时未上报", Severity: "warn", Subject: "PC-1", Detail: "最后上报 09-20", URL: "https://c/alerts", At: at}

	if err := (Webhook{URL: srv.URL + "/hook", Format: FormatGeneric, Client: srv.Client()}).Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got.body["title"] != m.Title || got.body["severity"] != "warn" || got.body["url"] != m.URL || !strings.Contains(got.body["text"].(string), "[WARN]") {
		t.Fatalf("generic body = %v", got.body)
	}

	if err := (Webhook{URL: srv.URL + "/robot/send?access_token=abc", Format: FormatDingTalk, Secret: "SEC", Client: srv.Client(), Now: func() time.Time { return at }}).Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.query, "access_token=abc&timestamp="+"1789977600000"+"&sign=") || got.body["msgtype"] != "text" {
		t.Fatalf("dingtalk query=%q body=%v", got.query, got.body)
	}
	if text := got.body["text"].(map[string]any)["content"].(string); !strings.Contains(text, m.Title) {
		t.Fatalf("dingtalk text = %q", text)
	}

	if err := (Webhook{URL: srv.URL + "/feishu", Format: FormatFeishu, Secret: "SEC", Client: srv.Client(), Now: func() time.Time { return at }}).Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got.body["msg_type"] != "text" || got.body["timestamp"] != "1789977600" || got.body["sign"] == "" {
		t.Fatalf("feishu body = %v", got.body)
	}
}

func TestWebhookReportsRefusals(t *testing.T) {
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errcode":310000,"errmsg":"keywords not in content"}`))
	}))
	defer refuse.Close()
	err := (Webhook{URL: refuse.URL, Format: FormatDingTalk, Client: refuse.Client()}).Send(context.Background(), Message{Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "310000") {
		t.Fatalf("a robot's error code must surface: %v", err)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer down.Close()
	if err := (Webhook{URL: down.URL, Client: down.Client()}).Send(context.Background(), Message{Title: "x"}); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("a 503 must be an error: %v", err)
	}
}

// fakeSMTP speaks just enough of the protocol to take one message.
func fakeSMTP(t *testing.T) (addr string, received <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	out := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { conn.Write([]byte(s + "\r\n")) }
		say("220 fake ESMTP")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-fake")
				say("250 8BITMIME")
			case strings.HasPrefix(cmd, "HELO"), strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				out <- data.String()
				return
			default:
				say("500 what")
			}
		}
	}()
	return ln.Addr().String(), out
}

func TestSMTPSendsOneMailPerMessage(t *testing.T) {
	addr, received := fakeSMTP(t)
	host, portText, _ := net.SplitHostPort(addr)
	var port int
	for _, c := range portText {
		port = port*10 + int(c-'0')
	}
	s := SMTP{Host: host, Port: port, From: "console@example.com", To: []string{"ops@example.com", "boss@example.com"}}
	m := Message{Title: "work1 本月已用预算 85%", Severity: "warn", Subject: "work1", Detail: "已用 $85.00 / $100.00"}
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	select {
	case mail := <-received:
		if !strings.Contains(mail, "To: ops@example.com, boss@example.com") || !strings.Contains(mail, "Subject: =?utf-8?q?") || !strings.Contains(mail, "$85.00") {
			t.Fatalf("mail:\n%s", mail)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no mail arrived")
	}
	// A password over a relay without TLS is refused.
	addr2, _ := fakeSMTP(t)
	host2, portText2, _ := net.SplitHostPort(addr2)
	port = 0
	for _, c := range portText2 {
		port = port*10 + int(c-'0')
	}
	s = SMTP{Host: host2, Port: port, Username: "u", Password: "p", From: "a@b", To: []string{"c@d"}}
	if err := s.Send(context.Background(), m); err == nil || !strings.Contains(err.Error(), "no TLS") {
		t.Fatalf("plain-text credentials must be refused: %v", err)
	}
}
