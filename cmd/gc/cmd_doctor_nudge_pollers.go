package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/doctor"
)

type nudgePollerDirectoryCheck struct{ cityPath string }

func newNudgePollerDirectoryCheck(cityPath string) *nudgePollerDirectoryCheck {
	return &nudgePollerDirectoryCheck{cityPath: cityPath}
}

func (c *nudgePollerDirectoryCheck) Name() string { return "nudge-poller-directory" }

func (c *nudgePollerDirectoryCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	dir := citylayout.RuntimePath(c.cityPath, "nudges", "pollers")
	entries, err := os.ReadDir(dir)
	result := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}
	if errors.Is(err, os.ErrNotExist) {
		result.Status = doctor.StatusOK
		result.Message = "nudge poller directory: 0 entries (0 PID files, 0 lock files)"
		return result
	}
	if err != nil {
		result.Status = doctor.StatusWarning
		result.Message = fmt.Sprintf("cannot count nudge poller directory: %v", err)
		return result
	}
	var pidFiles, lockFiles int
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".pid") {
			pidFiles++
		}
		if strings.HasSuffix(entry.Name(), ".pid.lock") {
			lockFiles++
		}
	}
	result.Status = doctor.StatusOK
	if len(entries) >= 1024 {
		result.Status = doctor.StatusWarning
		result.FixHint = "stale PID files are reaped on poller spawn; investigate growing stable lock files before removing any lock inode"
	}
	result.Message = fmt.Sprintf("nudge poller directory: %d entries (%d PID files, %d lock files)", len(entries), pidFiles, lockFiles)
	return result
}

func (c *nudgePollerDirectoryCheck) CanFix() bool { return false }

func (c *nudgePollerDirectoryCheck) WarmupEligible() bool { return false }

func (c *nudgePollerDirectoryCheck) Fix(_ *doctor.CheckContext) error { return nil }
