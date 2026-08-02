package doctor

import (
	"errors"
	"strings"
	"testing"
)

func TestHardeningSymbolsCheck_AllPresent_OK(t *testing.T) {
	c := NewHardeningSymbolsCheck()
	c.binaryPath = func() (string, error) { return "/opt/homebrew/bin/gc", nil }
	c.runNM = func(string) (string, error) {
		var b strings.Builder
		for _, sym := range hardeningSymbols {
			b.WriteString("0x1000000 T main." + sym.name + "\n")
		}
		return b.String(), nil
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want SeverityBlocking (default) even on OK", r.Severity)
	}
	if !strings.Contains(r.Message, "5") {
		t.Errorf("message = %q, want mention of all 5 symbols", r.Message)
	}
}

func TestHardeningSymbolsCheck_OneMissing_ErrorsWithBeadAndFixHint(t *testing.T) {
	c := NewHardeningSymbolsCheck()
	c.binaryPath = func() (string, error) { return "/opt/homebrew/bin/gc", nil }
	c.runNM = func(string) (string, error) {
		var b strings.Builder
		for _, sym := range hardeningSymbols {
			if sym.name == "checkProviderSeatCap" {
				continue // simulate a stale-base build that dropped this one
			}
			b.WriteString("0x1000000 T main." + sym.name + "\n")
		}
		return b.String(), nil
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want SeverityBlocking", r.Severity)
	}
	if !strings.Contains(r.Message, "checkProviderSeatCap") || !strings.Contains(r.Message, "ga-mpb0xu") {
		t.Errorf("message = %q, want missing symbol name and its bead", r.Message)
	}
	if !strings.Contains(r.Message, "1/5") {
		t.Errorf("message = %q, want 1/5 missing count", r.Message)
	}
	if r.FixHint == "" || !strings.Contains(r.FixHint, "LIVE base") {
		t.Errorf("FixHint = %q, want remediation pointing at the live-base fold pattern", r.FixHint)
	}
	if len(r.Details) == 0 {
		t.Error("Details empty, want reachability-not-effect caveat")
	}
}

func TestHardeningSymbolsCheck_AllMissing_ErrorsWithFullList(t *testing.T) {
	c := NewHardeningSymbolsCheck()
	c.binaryPath = func() (string, error) { return "/opt/homebrew/bin/gc", nil }
	c.runNM = func(string) (string, error) { return "0x1000000 T main.someUnrelatedFunc\n", nil }

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "5/5") {
		t.Errorf("message = %q, want 5/5 missing count", r.Message)
	}
	for _, sym := range hardeningSymbols {
		if !strings.Contains(r.Message, sym.name) {
			t.Errorf("message = %q, want missing symbol %q listed", r.Message, sym.name)
		}
	}
}

func TestHardeningSymbolsCheck_NMUnavailable_WarnsAdvisoryNotBlocking(t *testing.T) {
	c := NewHardeningSymbolsCheck()
	c.binaryPath = func() (string, error) { return "/opt/homebrew/bin/gc", nil }
	c.runNM = func(string) (string, error) { return "", errors.New("go: command not found") }

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want SeverityAdvisory — a missing go toolchain must not block doctor", r.Severity)
	}
}

func TestHardeningSymbolsCheck_BinaryPathUnresolvable_WarnsAdvisory(t *testing.T) {
	c := NewHardeningSymbolsCheck()
	c.binaryPath = func() (string, error) { return "", errors.New("os.Executable failed") }

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want SeverityAdvisory", r.Severity)
	}
}

func TestHardeningSymbolsCheck_CanFixAndWarmup(t *testing.T) {
	c := NewHardeningSymbolsCheck()
	if c.CanFix() {
		t.Error("CanFix() = true, want false — a missing symbol needs a real rebuild")
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil no-op", err)
	}
}

func TestRunGoToolNM_GoUnavailable_ReturnsError(t *testing.T) {
	t.Setenv("PATH", "")
	if _, err := runGoToolNM("/bin/ls"); err == nil {
		t.Error("runGoToolNM with empty PATH: want error, got nil")
	}
}
