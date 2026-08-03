//go:build integration

package tmux

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// buildBusyOnEnterBinary compiles a fake agent TUI that echoes stdin and, after
// receiving GC_TEST_BUSY_AFTER Enter keystrokes (default 1), prints an
// "esc to interrupt" busy footer — the same signal paneContainsBusyIndicator
// uses to detect a live Claude turn. Each Enter also prints an "ENTER#<n>"
// marker so a test can assert exactly how many submit keystrokes were delivered.
func buildBusyOnEnterBinary(t *testing.T, dir, name string) string {
	t.Helper()
	bin := dir + "/" + name
	src := dir + "/" + name + ".go"
	prog := `package main
import ("bufio";"fmt";"os";"strconv")
func main(){
	busyAfter:=1
	if v:=os.Getenv("GC_TEST_BUSY_AFTER"); v!=""{ if n,err:=strconv.Atoi(v); err==nil && n>0 { busyAfter=n } }
	enters:=0
	r:=bufio.NewReader(os.Stdin)
	for{
		b,err:=r.ReadByte()
		if err!=nil{ return }
		if b=='\r'||b=='\n'{
			enters++
			fmt.Printf("\nENTER#%d\n", enters)
			if enters>=busyAfter { fmt.Print("esc to interrupt\n") }
			continue
		}
		_,_=os.Stdout.Write([]byte{b})
	}
}
`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", src, err)
	}
	build := exec.Command("go", "build", "-o", bin, src)
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", name, err, string(out))
	}
	return bin
}

// TestNudgeSessionConfirmsSubmitForClaude proves the verified-submit path
// against real tmux: a single Enter that submits drives the fake agent busy,
// NudgeSession confirms it, and does NOT issue a redundant second Enter.
func TestNudgeSessionConfirmsSubmitForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	dir := t.TempDir()
	fake := buildBusyOnEnterBinary(t, dir, "fakeclaude")
	sessionName := fmt.Sprintf("gt-test-nudge-confirm-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, fake, map[string]string{
		"GC_PROVIDER":        "claude",
		"GC_TEST_BUSY_AFTER": "1",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)

	if err := tm.NudgeSession(sessionName, "hello-confirm"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, "esc to interrupt") {
		t.Fatalf("pane never reached submitted/busy state:\n%s", out)
	}
	if strings.Contains(out, "ENTER#2") {
		t.Fatalf("issued a redundant Enter after the turn already submitted (double-submit):\n%s", out)
	}
}

// TestNudgeSessionReEntersUntilSubmittedForClaude proves the ga-bwm fix
// end-to-end on real tmux: when the first Enter is dropped (the draft stays in
// the input box), NudgeSession re-sends Enter, and the second submit drives the
// agent busy. Pre-fix, the message would sit "drafted but not submitted".
func TestNudgeSessionReEntersUntilSubmittedForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	dir := t.TempDir()
	fake := buildBusyOnEnterBinary(t, dir, "fakeclaude")
	sessionName := fmt.Sprintf("gt-test-nudge-reenter-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, fake, map[string]string{
		"GC_PROVIDER":        "claude",
		"GC_TEST_BUSY_AFTER": "2", // drop the first Enter, submit on the second
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)

	if err := tm.NudgeSession(sessionName, "hello-reenter"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, "ENTER#2") {
		t.Fatalf("did not re-send Enter after the first was dropped:\n%s", out)
	}
	if !strings.Contains(out, "esc to interrupt") {
		t.Fatalf("never reached submitted/busy state after re-send:\n%s", out)
	}
}

// TestNudgeSessionLogsPayloadOnExhaustionForClaude proves the ga-9rqtdh
// instrumentation end-to-end against real tmux: when the pane never goes busy
// within the submit-confirm budget (every Enter dropped), NudgeSession still
// returns nil — the historical "nil == handed to tmux" fail-open contract is
// unchanged, so callers do not re-paste — but it logs the undelivered payload
// text, not just the fact of the failure. Without this, a drafted message is
// unrecoverable the moment the holding pane dies (the ga-9rqtdh cross-family
// instance: a lost operator ruling with no other durable copy anywhere).
func TestNudgeSessionLogsPayloadOnExhaustionForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	dir := t.TempDir()
	fake := buildBusyOnEnterBinary(t, dir, "fakeclaude")
	sessionName := fmt.Sprintf("gt-test-nudge-exhaust-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, fake, map[string]string{
		"GC_PROVIDER": "claude",
		// Beyond submitEnterMaxSends (3): the fake agent never goes busy
		// within budget, forcing the exhaustion path.
		"GC_TEST_BUSY_AFTER": "5",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)

	const payload = "defer the re-derivation project — record it and close the loop"

	var logBuf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	if err := tm.NudgeSession(sessionName, payload); err != nil {
		t.Fatalf("NudgeSession: expected fail-open (nil) on exhaustion per the historical contract, got error: %v", err)
	}

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, fmt.Sprintf("ENTER#%d", submitEnterMaxSends)) {
		t.Fatalf("expected all %d Enter attempts to be sent before giving up:\n%s", submitEnterMaxSends, out)
	}
	if strings.Contains(out, "esc to interrupt") {
		t.Fatalf("fake agent went busy — this test requires the never-busy exhaustion path, adjust GC_TEST_BUSY_AFTER:\n%s", out)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "ga-9rqtdh") {
		t.Fatalf("exhaustion log line missing entirely — the failure mode is silent again:\n%s", logged)
	}
	if !strings.Contains(logged, payload) {
		t.Fatalf("exhaustion log line did not include the undelivered payload — the drafted text is still unrecoverable once the pane dies:\n%s", logged)
	}
}
