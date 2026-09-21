package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func writeReachableManagedDoltState(t *testing.T, cityPath string) int {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dolt): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatalf("MkdirAll(city .beads): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveOwnedDoltTestListener(t, ln)

	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltState: %v", err)
	}
	return port
}

func writeReachableProviderManagedDoltState(t *testing.T, cityPath string) int {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dolt): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(city .beads/dolt): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveOwnedDoltTestListener(t, ln)

	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("write provider Dolt state: %v", err)
	}
	return port
}

// serveOwnedDoltTestListener drains the real TCP listener used by state
// validity probes. Repeated self-dials must not fill an unserved accept queue.
func serveOwnedDoltTestListener(t *testing.T, ln net.Listener) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				done <- err
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		if err := <-done; !errors.Is(err, net.ErrClosed) {
			t.Errorf("owned Dolt listener accept: %v", err)
		}
	})
}

// installOwnedDoltPortLsofFixture makes only the disposable listener's PID
// probe deterministic. Other lsof requests still use the real executable.
func installOwnedDoltPortLsofFixture(t *testing.T, port int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("self-dial owned Dolt listener on port %d: %v", port, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close self-dial to owned Dolt listener: %v", err)
	}

	realLsof, _ := exec.LookPath("lsof")
	binDir := t.TempDir()
	shim := fmt.Sprintf(`#!/bin/sh
target=
pid_only=
for arg do
  case "$arg" in
    -iTCP:%d) target=1 ;;
    -t) pid_only=1 ;;
  esac
done
if [ "$target" = 1 ] && [ "$pid_only" = 1 ]; then
  printf '%%s\n' '%d'
  exit 0
fi
if [ -n "${GC_TEST_REAL_LSOF:-}" ]; then
  exec "$GC_TEST_REAL_LSOF" "$@"
fi
exit 1
`, port, os.Getpid())
	shimPath := filepath.Join(binDir, "lsof")
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatalf("write owned-port lsof fixture: %v", err)
	}
	t.Setenv("GC_TEST_REAL_LSOF", realLsof)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if selected, err := exec.LookPath("lsof"); err != nil || selected != shimPath {
		t.Fatalf("owned-port lsof fixture selected %q, err %v; want %q", selected, err, shimPath)
	}
	if holder := findPortHolderPID(strconv.Itoa(port)); holder != os.Getpid() {
		t.Fatalf("owned-port fixture holder PID = %d, want test PID %d", holder, os.Getpid())
	}
}

//nolint:unused // exercised by native_dolt_rebind_integration_test.go
func occupyManagedDoltPort(t *testing.T, port int) {
	t.Helper()

	cmd := exec.Command("python3", "-c", `
import signal
import socket
import sys
import time

port = int(sys.argv[1])
deadline = time.time() + 10.0
sock = None
while time.time() < deadline:
    candidate = socket.socket()
    candidate.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        candidate.bind(("127.0.0.1", port))
        candidate.listen(5)
        sock = candidate
        break
    except OSError:
        candidate.close()
        time.sleep(0.05)

if sock is None:
    raise SystemExit(3)

def _stop(*_args):
    raise SystemExit(0)

signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    conn, _ = sock.accept()
    conn.close()
`, strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start managed port blocker: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("managed port blocker for %d exited early", port)
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("managed port blocker on %d did not become ready", port)
}
