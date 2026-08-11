//go:build darwin

package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// realBinaryIsImmutable reports whether the symlink-resolved real file
// backing target carries UF_IMMUTABLE (prong 1 — blocks write-through, e.g.
// `cp <new> target` follows the symlink and writes the resolved file).
func realBinaryIsImmutable(target string) (bool, error) {
	realPath, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false, err
	}
	var st unix.Stat_t
	if err := unix.Stat(realPath, &st); err != nil {
		return false, err
	}
	return st.Flags&unix.UF_IMMUTABLE != 0, nil
}

// nameIsImmutable reports whether target itself — not resolved; the
// PATH-visible name, which is ordinarily a symlink — carries UF_IMMUTABLE
// (prong 2 — blocks a pure rename-over-name: `mv` / `ln -sf`, neither of
// which follows an existing symlink at the destination. `go build -o`
// attempts the same fast rename first, but on failure falls back to a
// write-through path this prong does not cover — see prong 1 and the
// package doc comment in doctor_binary_immutability.go for the full
// go-build-o mapping, empirically verified in the ga-y275uo thread).
func nameIsImmutable(target string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Lstat(target, &st); err != nil {
		return false, err
	}
	return st.Flags&unix.UF_IMMUTABLE != 0, nil
}

// armRealBinaryImmutable sets UF_IMMUTABLE on the symlink-resolved real file
// backing target (prong 1).
func armRealBinaryImmutable(target string) error {
	realPath, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	return unix.Chflags(realPath, unix.UF_IMMUTABLE)
}

// armNameImmutable sets UF_IMMUTABLE on target itself without following a
// symlink (prong 2; mirrors `chflags -h uchg`). golang.org/x/sys/unix has no
// Darwin Lchflags binding — only *BSD/Dragonfly declare the lchflags(2)
// syscall number — so this shells out to the system chflags(1) -h flag
// rather than trapping a raw, ABI-fragile syscall (Apple does not guarantee
// direct syscall numbers as stable ABI outside libSystem).
func armNameImmutable(target string) error {
	return runChflags("-h", "uchg", target)
}

func runChflags(args ...string) error {
	out, err := exec.Command("/usr/bin/chflags", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chflags %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isPlatformUnsupported always returns false on Darwin — the real
// implementation is available.
func isPlatformUnsupported(_ error) bool { return false }
