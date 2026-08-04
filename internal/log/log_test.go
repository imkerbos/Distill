package log_test

import (
	"bytes"
	"encoding/json"
	"testing"

	applog "github.com/imkerbos/Distill/internal/log"
)

func TestNewEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	logger, err := applog.New("INFO", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("hello", "key", "value")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, buf.String())
	}
	if got["msg"] != "hello" || got["key"] != "value" {
		t.Errorf("got %v, want msg=hello key=value", got)
	}
}

func TestNewRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, err := applog.New("WARN", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("should be dropped")
	if buf.Len() != 0 {
		t.Errorf("INFO leaked at WARN level: %q", buf.String())
	}
}

func TestNewRejectsUnknownLevel(t *testing.T) {
	if _, err := applog.New("CHATTY", &bytes.Buffer{}); err == nil {
		t.Fatal("New() = nil error, want error for an unknown level")
	}
}
