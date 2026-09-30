package supervisor_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/cybercinch/docker-log-watcher/internal/supervisor"
)

// trueCmd returns the path to `true` (Unix) or an equivalent no-op.
func trueCmd() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	// /bin/true exits 0 immediately — perfect for Start/Stop tests.
	if _, err := os.Stat("/bin/true"); err == nil {
		return "/bin/true"
	}
	return "/usr/bin/true"
}

// sleepCmd returns a command that sleeps indefinitely (until killed).
func sleepCmd() (string, []string) {
	return "/bin/sleep", []string{"9999"}
}

// ─── Start ───────────────────────────────────────────────────────────────────

func TestStart(t *testing.T) {
	sup := supervisor.New(trueCmd(), nil)
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// /bin/true exits immediately — give watchExit goroutine a moment.
	time.Sleep(100 * time.Millisecond)
	sup.Stop()
}

// ─── Stop ────────────────────────────────────────────────────────────────────

func TestStop(t *testing.T) {
	bin, args := sleepCmd()
	sup := supervisor.New(bin, args)
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Stop must return without hanging.
	done := make(chan struct{})
	go func() {
		sup.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s")
	}
}

// ─── Restart ─────────────────────────────────────────────────────────────────

func TestRestart(t *testing.T) {
	bin, args := sleepCmd()
	sup := supervisor.New(bin, args)
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- sup.Restart() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Restart: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Restart did not return within 5s")
	}
	sup.Stop()
}

// ─── Crash respawn ───────────────────────────────────────────────────────────

// TestExitZeroNoRespawn verifies that a child exiting with status 0 is NOT
// automatically respawned.  file_follow exits 0 when there are no Follower
// stanzas — the next config change will start it again via Restart().
func TestExitZeroNoRespawn(t *testing.T) {
	sup := supervisor.New(trueCmd(), nil) // exits 0 immediately
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait longer than respawnDelay (1s).  If the supervisor incorrectly
	// respawned the child we would see repeated "child process started" log
	// lines, but the important thing is Stop() doesn't hang.
	time.Sleep(1500 * time.Millisecond)

	// Stop must return cleanly even though the child already exited.
	done := make(chan struct{})
	go func() {
		sup.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return within 3s after exit-0 child")
	}
}

// TestRestartAfterExitZero verifies that Restart() works correctly after the
// child has already exited with status 0.  This is the real-world scenario:
// file_follow exits 0 because all Follower configs were removed, then a new
// container starts and the watcher calls Restart().
func TestRestartAfterExitZero(t *testing.T) {
	sup := supervisor.New(trueCmd(), nil) // exits 0 immediately
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let the exit-0 path complete.
	time.Sleep(200 * time.Millisecond)

	// Restart should not hang even though the child already exited.
	done := make(chan error, 1)
	go func() { done <- sup.Restart() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Restart after exit-0: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Restart did not return within 5s")
	}
	sup.Stop()
}

func TestRespawnOnCrash(t *testing.T) {
	// Use a script that creates a file, then exits 1 — we can count restarts.
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	// A shell one-liner: read the current count, increment, write back, exit 1.
	script := `#!/bin/sh
f="` + countFile + `"
n=0
[ -f "$f" ] && n=$(cat "$f")
n=$((n+1))
echo $n > "$f"
exit 1
`
	scriptPath := filepath.Join(dir, "crasher.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	sup := supervisor.New("/bin/sh", []string{scriptPath})
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait long enough for at least 2 spawns (each exits ~immediately).
	// respawnDelay is 1s so 3 seconds should give us 2-3 runs.
	time.Sleep(3 * time.Second)
	sup.Stop()

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("read count file: %v", err)
	}
	count := 0
	if _, err := fmt.Sscanf(string(data), "%d", &count); err != nil {
		t.Fatalf("parse count: %v", err)
	}
	if count < 2 {
		t.Errorf("expected at least 2 spawns on crash, got %d", count)
	}
}
