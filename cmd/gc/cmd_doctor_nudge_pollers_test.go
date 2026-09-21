package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestNudgePollerDirectoryCheckReportsEntryCounts(t *testing.T) {
	cityPath := t.TempDir()
	check := newNudgePollerDirectoryCheck(cityPath)
	if result := check.Run(nil); result.Status != doctor.StatusOK || !strings.Contains(result.Message, "0 entries") {
		t.Fatalf("missing directory result = %+v", result)
	}
	dir := citylayout.RuntimePath(cityPath, "nudges", "pollers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.pid", "one.pid.lock", "two.pid.lock"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result := check.Run(nil)
	if result.Status != doctor.StatusOK || !strings.Contains(result.Message, "3 entries") ||
		!strings.Contains(result.Message, "1 PID") || !strings.Contains(result.Message, "2 lock") {
		t.Fatalf("directory count result = %+v", result)
	}
}
