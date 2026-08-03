package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// readCityEvents reads every JSONL event durably recorded for the city at
// cityDir (the same file openCityRecorder writes: .gc/events.jsonl).
// cmd_session.go's kill path has no injectable fake recorder (unlike the
// reconciler test harness's env.rec), so this reads back the real on-disk log
// the production code path writes to — an effect-level check, not a grep for
// the emit call.
func readCityEvents(t *testing.T, cityDir string) []events.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading events.jsonl: %v", err)
	}
	var out []events.Event
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e events.Event
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("decoding event line %q: %v", line, err)
		}
		out = append(out, e)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning events.jsonl: %v", err)
	}
	return out
}

// TestCmdSessionKill_EmitsSessionSlept pins ga-e5ygdf's fourth genuine
// sleep-transition site: `gc session kill` writes the bead to state=asleep
// (sleep_reason=killed) via the same SleepPatch machinery the reconciler's
// sites use, but — unlike those — had no sibling lifecycle event next to its
// existing session.stopped emission. Verified on the actual durable event log
// the production path writes (readCityEvents), not by grepping the emit call.
func TestCmdSessionKill_EmitsSessionSlept(t *testing.T) {
	const identity = "session-a"
	const sessionName = "s-gc-kill-slept"
	store, bead, cityDir := newKillPokeSession(t, identity, sessionName)
	_ = store

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}

	all := readCityEvents(t, cityDir)
	var slept []events.Event
	for _, e := range all {
		if e.Type == events.SessionSlept {
			slept = append(slept, e)
		}
	}
	if len(slept) != 1 {
		t.Fatalf("session.slept events = %d, want exactly 1; all events = %+v", len(slept), all)
	}
	if slept[0].SessionID != bead.ID {
		t.Errorf("SessionID = %q, want %q", slept[0].SessionID, bead.ID)
	}
	var payload events.SessionSleptPayload
	if err := json.Unmarshal(slept[0].Payload, &payload); err != nil {
		t.Fatalf("decoding SessionSleptPayload: %v", err)
	}
	if payload.Reason != "killed" {
		t.Errorf("Reason = %q, want killed", payload.Reason)
	}
	if payload.SessionName != sessionName {
		t.Errorf("SessionName = %q, want %q", payload.SessionName, sessionName)
	}
}
