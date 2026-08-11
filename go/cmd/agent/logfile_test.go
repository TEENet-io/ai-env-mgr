package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadLogTailSmallFileReturnsAll(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.log"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readLogTail(dir); string(got) != "line1\nline2\n" {
		t.Fatalf("got %q", got)
	}
}

func TestReadLogTailMissingReturnsNil(t *testing.T) {
	if readLogTail(t.TempDir()) != nil {
		t.Fatal("a missing log should yield nil, not an empty upload")
	}
}

func TestReadLogTailCapsSizeAndTrimsPartialLine(t *testing.T) {
	dir := t.TempDir()
	// A file larger than the tail window: an old head line, a big filler with no
	// newlines, then a fresh tail line.
	content := append([]byte("OLDHEAD\n"), bytes.Repeat([]byte("x"), logTailBytes)...)
	content = append(content, []byte("\nFRESHTAIL\n")...)
	if err := os.WriteFile(filepath.Join(dir, "agent.log"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	got := readLogTail(dir)
	if len(got) > logTailBytes {
		t.Fatalf("tail is %d bytes, larger than the %d cap", len(got), logTailBytes)
	}
	if bytes.Contains(got, []byte("OLDHEAD")) {
		t.Fatal("tail should not include content beyond the window")
	}
	if !bytes.Contains(got, []byte("FRESHTAIL")) {
		t.Fatal("tail should include the most recent line")
	}
}
