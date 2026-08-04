package logging

import (
	"regexp"
	"strings"
	"testing"
)

// linePattern matches "[{timestamp}] ({level}) {fields...} msg={message}".
var linePattern = regexp.MustCompile(
	`^\[\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z\] \((DEBUG|INFO|WARN|ERROR)\)( \S+=\S+| \S+=".*")* msg=".*"\n$`)

func TestFormat(t *testing.T) {
	var buf strings.Builder
	logger := NewWithLevel(LevelDebug, &buf).WithFields("kb_id", "kb-1")
	logger.Info("document queued", "file_name", "notes.txt")

	line := buf.String()
	if !linePattern.MatchString(line) {
		t.Fatalf("line does not match format: %q", line)
	}
	if !strings.Contains(line, `(INFO) kb_id=kb-1 file_name=notes.txt msg="document queued"`) {
		t.Fatalf("unexpected line contents: %q", line)
	}
}

func TestMessageIsAlwaysQuoted(t *testing.T) {
	var buf strings.Builder
	NewWithLevel(LevelInfo, &buf).Info(`say "hi" back`)
	if !strings.Contains(buf.String(), `msg="say \"hi\" back"`) {
		t.Fatalf("message not escaped with quotes: %q", buf.String())
	}
}

func TestQuotesValuesWithSpaces(t *testing.T) {
	var buf strings.Builder
	NewWithLevel(LevelInfo, &buf).Info("m", "err", "connection refused")
	if !strings.Contains(buf.String(), `err="connection refused"`) {
		t.Fatalf("value with spaces not quoted: %q", buf.String())
	}
}

func TestLevelFiltering(t *testing.T) {
	cases := []struct {
		configured Level
		want       []string
		suppressed []string
	}{
		{LevelError, []string{"ERROR"}, []string{"WARN", "INFO", "DEBUG"}},
		{LevelWarn, []string{"ERROR", "WARN"}, []string{"INFO", "DEBUG"}},
		{LevelInfo, []string{"ERROR", "WARN", "INFO"}, []string{"DEBUG"}},
		{LevelDebug, []string{"ERROR", "WARN", "INFO", "DEBUG"}, nil},
	}
	for _, tc := range cases {
		var buf strings.Builder
		logger := NewWithLevel(tc.configured, &buf)
		logger.Debug("m")
		logger.Info("m")
		logger.Warn("m")
		logger.Error("m")
		for _, lvl := range tc.want {
			if !strings.Contains(buf.String(), "("+lvl+")") {
				t.Errorf("level %v: expected %s to be printed", tc.configured, lvl)
			}
		}
		for _, lvl := range tc.suppressed {
			if strings.Contains(buf.String(), "("+lvl+")") {
				t.Errorf("level %v: expected %s to be suppressed", tc.configured, lvl)
			}
		}
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"":        LevelInfo,
		"info":    LevelInfo,
		"DEBUG":   LevelDebug,
		"warn":    LevelWarn,
		"WARNING": LevelWarn,
		"error":   LevelError,
		"2":       LevelDebug,
		"1":       LevelInfo,
		"0":       LevelWarn,
		"-1":      LevelError,
		"bogus":   LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestWithFieldsDoesNotMutateParent(t *testing.T) {
	var buf strings.Builder
	parent := NewWithLevel(LevelInfo, &buf).WithFields("a", 1)
	_ = parent.WithFields("b", 2)
	parent.Info("m")
	if strings.Contains(buf.String(), "b=2") {
		t.Fatalf("child fields leaked into parent: %q", buf.String())
	}
}
