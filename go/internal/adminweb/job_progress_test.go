package adminweb

import (
	"strings"
	"testing"
	"time"
)

// A 700 MB publish shows only a rising elapsed time without this, which looks
// the same whether the transfer is moving or wedged.
func TestJobReportsProgress(t *testing.T) {
	var r jobRunner
	step := make(chan struct{})
	done := make(chan struct{})
	if err := r.start("codex", "26.810.52044-b1", func(setStep func(string), setProgress func(done, total int64)) error {
		setStep("下载安装包")
		setProgress(350<<20, 700<<20)
		step <- struct{}{}
		<-done
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-step

	j := r.snapshot()
	if !j.Measured() {
		t.Fatal("a transfer with a known total was reported as indeterminate")
	}
	if got := j.Percent(); got != 50 {
		t.Fatalf("percent = %d, want 50", got)
	}
	if got := j.Sized(); got != "350 / 700 MB" {
		t.Fatalf("sized = %q", got)
	}
	close(done)
}

// Moving to the next step must reset the bar. Carrying the download's numbers
// into the upload would show a bar that starts full and never moves.
func TestJobProgressResetsBetweenSteps(t *testing.T) {
	var r jobRunner
	// Handshakes rather than bare sends: the job must not move to the next
	// step until this goroutine has looked at the current one.
	downloaded := make(chan struct{})
	checked := make(chan struct{})
	uploading := make(chan struct{})
	release := make(chan struct{})
	if err := r.start("codex", "1.0.0", func(setStep func(string), setProgress func(done, total int64)) error {
		setStep("下载安装包")
		setProgress(700<<20, 700<<20)
		downloaded <- struct{}{}
		<-checked
		setStep("上传到 OSS")
		uploading <- struct{}{}
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	<-downloaded
	if got := r.snapshot().Percent(); got != 100 {
		t.Fatalf("download percent = %d, want 100", got)
	}
	close(checked)
	<-uploading
	j := r.snapshot()
	if j.Measured() || j.Percent() != 0 || j.Sized() != "" {
		t.Fatalf("the new step inherited the previous one's progress: %+v", j)
	}
	close(release)
}

// A server that sends no Content-Length leaves the total unknown. Claiming a
// percentage there would be inventing one, so the page shows an indeterminate
// bar instead.
func TestJobProgressWithoutATotal(t *testing.T) {
	j := &job{Done: 120 << 20, Total: -1}
	if j.Measured() {
		t.Fatal("an unknown total was treated as measurable")
	}
	if j.Percent() != 0 {
		t.Fatalf("percent = %d with no total, want 0", j.Percent())
	}
	if got := j.Sized(); got != "120 MB" {
		t.Fatalf("sized = %q, want the bytes seen so far", got)
	}
}

// The publish page draws the bar from attributes, never an inline style: the
// CSP has no 'unsafe-inline' for styles, so a style attribute would be dropped
// by the browser and the bar would silently never move.
func TestPublishPageDrawsProgressWithoutInlineStyle(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	data := pageData{CSRF: "t", Nav: "rollout", Job: &job{
		Kind: "codex", Version: "26.810.52044-b1", State: jobRunning,
		Step: "下载安装包", Started: time.Now(), Done: 350 << 20, Total: 700 << 20,
	}}
	var sb strings.Builder
	if err := s.tpl.ExecuteTemplate(&sb, "rollout.html", data); err != nil {
		t.Fatalf("rollout.html: %v", err)
	}
	body := sb.String()
	if !strings.Contains(body, `<progress value="367001600" max="734003200">`) {
		t.Fatal("the progress element is missing or not carrying its value")
	}
	if !strings.Contains(body, "50%") || !strings.Contains(body, "350 / 700 MB") {
		t.Fatal("the page does not state the progress in text")
	}
	if strings.Contains(body, "style=") {
		t.Fatal("the page uses an inline style, which this CSP drops")
	}
}
