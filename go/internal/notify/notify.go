// Package notify delivers one alert to the people who should hear about it:
// a webhook in one of a few shapes, or a mail relay.
//
// Every channel is a plain Send with a context; retries, once-only delivery
// and what counts as an alert are the caller's business. Nothing here reads
// settings or the database.
package notify

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Message is what a channel is asked to deliver.
type Message struct {
	Title    string
	Severity string // warn | crit
	Subject  string // the machine, employee or task concerned
	Detail   string
	URL      string // where to look; may be empty
	At       time.Time
}

// Text is the message as one block of plain text, for channels that carry
// nothing structured.
func (m Message) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s\n", strings.ToUpper(m.Severity), m.Title)
	if m.Subject != "" {
		fmt.Fprintf(&b, "对象：%s\n", m.Subject)
	}
	if m.Detail != "" {
		fmt.Fprintf(&b, "%s\n", m.Detail)
	}
	if !m.At.IsZero() {
		fmt.Fprintf(&b, "时间：%s\n", m.At.Local().Format("2006-01-02 15:04"))
	}
	if m.URL != "" {
		fmt.Fprintf(&b, "%s\n", m.URL)
	}
	return b.String()
}

// Channel is one way out.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}
