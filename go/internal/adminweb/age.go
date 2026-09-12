package adminweb

import (
	"fmt"
	"time"
)

// humanAge turns an RFC3339 timestamp into how long ago it was.
//
// "还活着吗" is the question this column answers, and a wall-clock stamp in
// UTC makes the reader do the subtraction. The exact value is kept in the
// cell's title, so precision is one hover away and nothing is lost.
func humanAge(stamp string) string {
	if stamp == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return stamp // show whatever the agent reported rather than hiding it
	}
	d := time.Since(t)
	switch {
	case d < 0:
		// A machine's clock can run ahead of ours; saying "just now" beats
		// showing a negative age.
		return "刚刚"
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
}

// localStamp renders an RFC3339 timestamp as Shanghai wall-clock time.
//
// The policy's UpdatedAt is written in UTC, and everyone reading this console
// is eight hours ahead of it: shown raw, "2026-08-16T09:03:50Z" is a stamp the
// reader has to convert before it means anything. Seconds are dropped because
// nobody cares which second a policy was saved in. Same zone as the log page
// (see shanghai), so two pages never disagree about what time it is.
func localStamp(stamp string) string {
	if stamp == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return stamp // show what was actually stored rather than hiding it
	}
	return t.In(shanghai()).Format("2006-01-02 15:04")
}
