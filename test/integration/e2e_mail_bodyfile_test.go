//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests are the acceptance floor for ga-c3zu97: gc mail send bodies
// were shell-substituted when a caller passed markdown prose (backticks,
// $(...)) through a double-quoted -m/positional argument -- the shell
// expands those as command substitution before gc ever sees the string,
// silently corrupting or executing the body while gc mail send still
// reports success. The fix adds --body-file, which reads the body via
// filesystem I/O instead of a shell argument.

// extractSentMessageID pulls the message ID out of gc mail send's stdout,
// e.g. "Sent message gc-1 to mayor" -> "gc-1".
func extractSentMessageID(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "Sent" && fields[1] == "message" {
			return fields[2]
		}
	}
	t.Fatalf("no message ID found in send output:\n%s", out)
	return ""
}

// TestE2E_MailSendBodyFile_RoundTripsByteIdentical is acceptance-floor #1:
// a body containing backticked text, a $(...) form, and an apostrophe must
// round-trip byte-identical through --body-file, verified by reading the
// stored message back -- not by trusting the send command's success line.
func TestE2E_MailSendBodyFile_RoundTripsByteIdentical(t *testing.T) {
	city := e2eCity{Agents: []e2eAgent{{Name: "bodyfile-recv", StartCommand: e2eSleepScript()}}}
	cityDir := setupE2ECity(t, nil, city)

	body := "See `cmd/gc/cmd_mail.go` and run $(go test ./...) -- don't skip it."
	bodyPath := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write body file: %v", err)
	}

	out, err := gc(cityDir, "mail", "send", "bodyfile-recv", "--body-file", bodyPath)
	if err != nil {
		t.Fatalf("gc mail send --body-file failed: %v\noutput: %s", err, out)
	}
	id := extractSentMessageID(t, out)

	readOut, err := gc(cityDir, "mail", "read", id)
	if err != nil {
		t.Fatalf("gc mail read failed: %v\noutput: %s", err, readOut)
	}
	if !strings.Contains(readOut, "Body:     "+body) {
		t.Fatalf("body did not round-trip byte-identical.\nwant body: %q\ngot output:\n%s", body, readOut)
	}
}

// TestE2E_MailSendInlineDoubleQuoted_ShellSubstitutionCorrupts is the
// acceptance-floor #2 ablation: the OLD invocation shape (-m "..." with a
// markdown code span inside a double-quoted shell argument) must still
// demonstrably corrupt the body via real shell command substitution --
// proving this fixture discriminates rather than passing regardless of
// whether the fix exists. Per feedback_regression_test_fixture_must_discriminate.
func TestE2E_MailSendInlineDoubleQuoted_ShellSubstitutionCorrupts(t *testing.T) {
	city := e2eCity{Agents: []e2eAgent{{Name: "ablation-recv", StartCommand: e2eSleepScript()}}}
	cityDir := setupE2ECity(t, nil, city)

	envDir := commandCityDirForArgs(cityDir, []string{"mail", "send"})
	env := commandEnvForDir(envDir, false)
	env = append(env, "GC_BIN="+gcBinary)

	// Exactly the shape a real agent types: a double-quoted shell argument
	// containing a markdown code span. bash expands the backticks as
	// command substitution before gc ever sees the string.
	const shellLine = `"$GC_BIN" mail send ablation-recv -m "before ` + "`echo MIDDLE`" + ` after"`
	cmd := exec.Command("bash", "-c", shellLine)
	cmd.Dir = cityDir
	cmd.Env = env
	sendOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash -c gc mail send failed: %v\n%s", err, sendOut)
	}
	id := extractSentMessageID(t, string(sendOut))

	readOut, err := gc(cityDir, "mail", "read", id)
	if err != nil {
		t.Fatalf("gc mail read failed: %v\noutput: %s", err, readOut)
	}
	if strings.Contains(readOut, "before `echo MIDDLE` after") {
		t.Fatal("expected shell command substitution to corrupt the inline body, but literal backticks survived -- this ablation no longer discriminates, re-check the fixture")
	}
	if !strings.Contains(readOut, "before MIDDLE after") {
		t.Fatalf("expected body corrupted to 'before MIDDLE after' (echo's stdout spliced in by the shell), got:\n%s", readOut)
	}
}

// TestE2E_MailSendBodyFile_UntrustedContentDoesNotExecute is
// acceptance-floor #3, the security leg: a body-file containing a $(...)
// form must round-trip as literal text through --body-file, and whatever
// it names must NOT execute. This is what makes ga-c3zu97 more than a
// formatting bug -- an agent relaying text it did not author (scraped
// pages, third-party bead text) must not get an arbitrary-command-execution
// path into its own shell just because that text contains $(...).
func TestE2E_MailSendBodyFile_UntrustedContentDoesNotExecute(t *testing.T) {
	city := e2eCity{Agents: []e2eAgent{{Name: "pwn-recv", StartCommand: e2eSleepScript()}}}
	cityDir := setupE2ECity(t, nil, city)

	sentinel := filepath.Join(t.TempDir(), "pwned")
	body := fmt.Sprintf("Untrusted excerpt: $(touch %s) end of excerpt.", sentinel)
	bodyPath := filepath.Join(t.TempDir(), "malicious-body.txt")
	if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write body file: %v", err)
	}

	out, err := gc(cityDir, "mail", "send", "pwn-recv", "--body-file", bodyPath)
	if err != nil {
		t.Fatalf("gc mail send --body-file failed: %v\noutput: %s", err, out)
	}
	id := extractSentMessageID(t, out)

	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatalf("SECURITY FAILURE: %s was created -- the $(...) form executed instead of round-tripping as literal text", sentinel)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error on sentinel path: %v", statErr)
	}

	readOut, err := gc(cityDir, "mail", "read", id)
	if err != nil {
		t.Fatalf("gc mail read failed: %v\noutput: %s", err, readOut)
	}
	if !strings.Contains(readOut, body) {
		t.Fatalf("expected literal untrusted body to round-trip unchanged.\nwant: %q\ngot:\n%s", body, readOut)
	}
}
