package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeExecutor captures tmux command arguments for unit testing.
type fakeExecutor struct {
	calls [][]string // each call's full args
	out   string
	err   error
	outs  []string
	errs  []error
	idx   int
}

func (f *fakeExecutor) execute(args []string) (string, error) {
	// Copy args to avoid aliasing with the caller's slice.
	cp := make([]string, len(args))
	copy(cp, args)
	f.calls = append(f.calls, cp)
	if f.idx < len(f.outs) || f.idx < len(f.errs) {
		var out string
		var err error
		if f.idx < len(f.outs) {
			out = f.outs[f.idx]
		}
		if f.idx < len(f.errs) {
			err = f.errs[f.idx]
		}
		f.idx++
		return out, err
	}
	return f.out, f.err
}

func (f *fakeExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func TestNewSessionWithCommandAndEnvClearsEmptyVars(t *testing.T) {
	cleanupShellEnvFiles(t, "gc-test-locale-clear")
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec

	env := map[string]string{
		"LANG":     "en_US.UTF-8",
		"LC_ALL":   "",
		"LC_CTYPE": "",
	}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-locale-clear", "", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}

	args := exec.calls[0]
	joined := strings.Join(args, "\x00")
	if strings.Contains(joined, "\x00-e\x00") {
		t.Fatalf("new-session args must not carry -e env flags: %v", args)
	}
	if strings.Contains(joined, "LANG=en_US.UTF-8") {
		t.Fatalf("new-session argv leaked env value: %v", args)
	}
	if got := args[len(args)-1]; !strings.Contains(got, "__gc_env=") || !strings.Contains(got, "exec claude") {
		t.Fatalf("command = %q, want shell env source wrapper around claude", got)
	}
	if !hasTmuxSourceFileCall(exec.calls) {
		t.Fatalf("tmux env source-file call missing: %v", exec.calls)
	}

	shellFile := soleShellEnvFile(t, "gc-test-locale-clear")
	defer func() { _ = os.Remove(shellFile) }()
	body, err := os.ReadFile(shellFile)
	if err != nil {
		t.Fatalf("read shell env file: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"export LANG='en_US.UTF-8'",
		"unset LC_ALL",
		"unset LC_CTYPE",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("shell env file missing %q:\n%s", want, text)
		}
	}
}

func TestNewSessionWithCommandAndEnvKeepsSecretsOutOfTmuxArgv(t *testing.T) {
	cleanupShellEnvFiles(t, "gc-test-secret-argv")
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec

	env := map[string]string{
		"OPENAI_API_KEY":       "sk-proj-test-secret",
		"GEMINI_API_KEY":       "gemini-test-secret",
		"GOOGLE_API_KEY":       "AIzaSy-test-secret",
		"BEADS_HOLDER_TOKEN":   "holder-test-secret",
		"GC_INSTANCE_TOKEN":    "instance-test-secret",
		"VISIBLE_NON_SECRET":   "plain-value",
		"EMPTY_INHERITED_VAR":  "",
		"QUOTED_PROVIDER_DATA": "value with spaces and 'quote'",
	}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-secret-argv", "", "codex resume", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}

	for _, call := range exec.calls {
		joined := strings.Join(call, "\x00")
		for _, forbidden := range []string{
			"sk-proj-test-secret",
			"gemini-test-secret",
			"AIzaSy-test-secret",
			"holder-test-secret",
			"instance-test-secret",
			"plain-value",
			"value with spaces",
			"\x00-e\x00",
		} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("tmux argv leaked %q in call %v", forbidden, call)
			}
		}
	}
	if !hasTmuxSourceFileCall(exec.calls) {
		t.Fatalf("tmux env source-file call missing: %v", exec.calls)
	}

	shellFile := soleShellEnvFile(t, "gc-test-secret-argv")
	defer func() { _ = os.Remove(shellFile) }()
	body, err := os.ReadFile(shellFile)
	if err != nil {
		t.Fatalf("read shell env file: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"export OPENAI_API_KEY='sk-proj-test-secret'",
		"export GOOGLE_API_KEY='AIzaSy-test-secret'",
		"unset EMPTY_INHERITED_VAR",
		"export QUOTED_PROVIDER_DATA='value with spaces and '\\''quote'\\'''",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("shell env file missing %q:\n%s", want, text)
		}
	}
}

func hasTmuxSourceFileCall(calls [][]string) bool {
	for _, call := range calls {
		for i, arg := range call {
			if arg == "source-file" && i+1 < len(call) {
				return true
			}
		}
	}
	return false
}

func soleShellEnvFile(t *testing.T, sessionName string) string {
	t.Helper()
	matches := shellEnvFiles(t, sessionName)
	if len(matches) != 1 {
		t.Fatalf("shell env files for %s = %v, want exactly 1", sessionName, matches)
	}
	return matches[0]
}

func cleanupShellEnvFiles(t *testing.T, sessionName string) {
	t.Helper()
	for _, path := range shellEnvFiles(t, sessionName) {
		_ = os.Remove(path)
	}
	t.Cleanup(func() {
		for _, path := range shellEnvFiles(t, sessionName) {
			_ = os.Remove(path)
		}
	})
}

func shellEnvFiles(t *testing.T, sessionName string) []string {
	t.Helper()
	pattern := filepath.Join(os.TempDir(), fmt.Sprintf(".gc-%d", os.Getuid()), "tmux-env", "gc-"+sessionName+"-*.shenv")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob shell env file: %v", err)
	}
	return matches
}

type promptFooterExecutor struct {
	calls [][]string
}

func (p *promptFooterExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	p.calls = append(p.calls, cp)
	if len(args) == 0 {
		return "", nil
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] != "-S" {
			continue
		}
		lines, err := strconv.Atoi(strings.TrimPrefix(args[i+1], "-"))
		if err != nil {
			return "", nil
		}
		if lines >= promptObservationLines {
			return strings.Join([]string{
				"Claude Code v2.1.112",
				"status line",
				"❯\u00a0",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
				"",
			}, "\n"), nil
		}
		return strings.Repeat("\n", 20), nil
	}
	return "", nil
}

func (p *promptFooterExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return p.execute(args)
}

// ctxBlockingExecutor blocks executeCtx until ctx is canceled. Used to
// verify that callers honor a wall-clock deadline on the subprocess.
type ctxBlockingExecutor struct {
	calls [][]string
}

func (b *ctxBlockingExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	b.calls = append(b.calls, cp)
	return "", nil
}

func (b *ctxBlockingExecutor) executeCtx(ctx context.Context, args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	b.calls = append(b.calls, cp)
	<-ctx.Done()
	return "", ctx.Err()
}

// TestRunBoundsByTmuxSubprocessTimeout verifies that Tmux.run applies a
// wall-clock cap to subprocess invocations. A wedged tmux subprocess must
// not be able to hang the shutdown path indefinitely.
func TestRunBoundsByTmuxSubprocessTimeout(t *testing.T) {
	orig := tmuxSubprocessTimeout
	tmuxSubprocessTimeout = 50 * time.Millisecond
	t.Cleanup(func() { tmuxSubprocessTimeout = orig })

	bx := &ctxBlockingExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: bx}

	type result struct {
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, err := tm.run("list-sessions")
		done <- result{err: err}
	}()

	select {
	case r := <-done:
		elapsed := time.Since(start)
		if r.err == nil {
			t.Fatalf("err = nil after %s, want context.DeadlineExceeded", elapsed)
		}
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded chain", r.err)
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("elapsed = %s, want < 500ms", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tm.run did not return within 2s — tmuxSubprocessTimeout not applied")
	}
}

func TestRunInjectsSocketFlag(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: Config{SocketName: "bright-lights"}, exec: fe}
	_, _ = tm.run("list-sessions")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	want := []string{"-u", "-L", "bright-lights", "list-sessions"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunNoSocketFlagWhenEmpty(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	_, _ = tm.run("list-sessions")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	want := []string{"-u", "list-sessions"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHiddenAttachedKeyBytesSupportsArrowNavigation(t *testing.T) {
	tests := map[string]string{
		"Up":    "\x1b[A",
		"Down":  "\x1b[B",
		"Right": "\x1b[C",
		"Left":  "\x1b[D",
	}
	for key, want := range tests {
		got, ok := hiddenAttachedKeyBytes(key)
		if !ok {
			t.Fatalf("hiddenAttachedKeyBytes(%q) not supported", key)
		}
		if string(got) != want {
			t.Fatalf("hiddenAttachedKeyBytes(%q) = %q, want %q", key, string(got), want)
		}
	}
}

func TestRunAlwaysPrependsUTF8Flag(t *testing.T) {
	fe := &fakeExecutor{}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	_, _ = tm.run("new-session", "-s", "test")

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	got := fe.calls[0]
	if got[0] != "-u" {
		t.Errorf("args[0] = %q, want %q", got[0], "-u")
	}
	// Verify full arg list: -u -L x new-session -s test
	want := []string{"-u", "-L", "x", "new-session", "-s", "test"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLatestActivityTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "single timestamp", input: "123", want: 123},
		{name: "multiple timestamps", input: "123\n456\n234", want: 456},
		{name: "blank lines ignored", input: "\n123\n\n456\n", want: 456},
		{name: "invalid timestamp", input: "123\nnope", wantErr: true},
		{name: "no timestamps", input: "\n\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := latestActivityTimestamp(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("latestActivityTimestamp(%q) error = nil, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("latestActivityTimestamp(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("latestActivityTimestamp(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsSessionRunningFalseWhenPaneDead(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"", "1"},
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	if tm.IsSessionRunning("runner") {
		t.Fatal("IsSessionRunning = true, want false for dead pane")
	}

	if len(fe.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(fe.calls))
	}
	want := [][]string{
		{"-u", "-L", "x", "has-session", "-t", "=runner"},
		{"-u", "-L", "x", "display-message", "-t", "runner:^.0", "-p", "#{pane_dead}"},
	}
	for i := range want {
		if len(fe.calls[i]) != len(want[i]) {
			t.Fatalf("call %d = %v, want %v", i, fe.calls[i], want[i])
		}
		for j := range want[i] {
			if fe.calls[i][j] != want[i][j] {
				t.Errorf("call %d arg %d = %q, want %q", i, j, fe.calls[i][j], want[i][j])
			}
		}
	}
}

func TestIsSessionRunningFallsBackToSessionExistsOnPaneQueryError(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{""},
		errs: []error{nil, ErrNoServer},
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

	if !tm.IsSessionRunning("runner") {
		t.Fatal("IsSessionRunning = false, want true when pane query fails after session exists")
	}
}

func TestProviderIsDeadRuntimeSessionRequiresEveryPaneDead(t *testing.T) {
	fe := &fakeExecutor{
		out: "1\n0",
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	p := &Provider{tm: tm}

	dead, err := p.IsDeadRuntimeSession("runner")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if dead {
		t.Fatal("IsDeadRuntimeSession = true, want false when any pane is live")
	}

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fe.calls))
	}
	want := []string{"-u", "-L", "x", "list-panes", "-s", "-t", "=runner", "-F", "#{pane_dead}"}
	if len(fe.calls[0]) != len(want) {
		t.Fatalf("call = %v, want %v", fe.calls[0], want)
	}
	for i := range want {
		if fe.calls[0][i] != want[i] {
			t.Fatalf("call arg %d = %q, want %q; call=%v", i, fe.calls[0][i], want[i], fe.calls[0])
		}
	}
}

func TestProviderIsDeadRuntimeSessionTrueWhenAllPanesDead(t *testing.T) {
	fe := &fakeExecutor{
		out: "1\n1",
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	p := &Provider{tm: tm}

	dead, err := p.IsDeadRuntimeSession("runner")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if !dead {
		t.Fatal("IsDeadRuntimeSession = false, want true when all panes are dead")
	}
}

func TestProviderIsDeadRuntimeSessionTreatsAbsentSessionAsNotDead(t *testing.T) {
	fe := &fakeExecutor{
		err: ErrSessionNotFound,
	}
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}
	p := &Provider{tm: tm}

	dead, err := p.IsDeadRuntimeSession("missing")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if dead {
		t.Fatal("IsDeadRuntimeSession = true, want false for absent session")
	}
}

func TestWaitForRuntimeReadyCapturesPromptAboveBlankFooter(t *testing.T) {
	fe := &promptFooterExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := tm.WaitForRuntimeReady(ctx, "mayor", &RuntimeConfig{
		Tmux: &RuntimeTmuxConfig{ReadyPromptPrefix: "❯ "},
	}, time.Second)
	if err != nil {
		t.Fatalf("WaitForRuntimeReady() error = %v, want nil", err)
	}

	if len(fe.calls) == 0 {
		t.Fatal("expected capture-pane call")
	}
	got := fe.calls[0]
	want := []string{"-u", "capture-pane", "-p", "-t", "mayor", "-S", "-120"}
	if len(got) != len(want) {
		t.Fatalf("first call = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("first call arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}
