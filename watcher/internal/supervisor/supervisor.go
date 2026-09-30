// Package supervisor manages the gravwell_file_follow child process.
//
// The watcher binary (docker-log-watcher) acts as a lightweight process
// supervisor:
//
//	docker-log-watcher (tini → PID 1)
//	  └─ gravwell_file_follow  (child)
//
// The supervisor:
//   - Starts the child after initial config sync.
//   - Restarts the child when configs change (debounced by the caller).
//   - Respawns the child if it exits unexpectedly.
//   - Forwards SIGTERM/SIGINT received by the watcher to the child so that
//     container shutdown is clean.
package supervisor

import (
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	// stopTimeout is how long we wait for the child to exit after SIGTERM before
	// escalating to SIGKILL.
	stopTimeout = 10 * time.Second

	// respawnDelay is the minimum pause between an unexpected child exit and the
	// next spawn attempt.  Prevents a tight crash-loop from hammering the system.
	respawnDelay = 1 * time.Second
)

// Supervisor owns and manages a single child process.
type Supervisor struct {
	bin  string
	args []string

	mu      sync.Mutex
	cmd     *exec.Cmd
	gen     uint64       // incremented on every startLocked; lets watchExit detect stale goroutines
	done    chan struct{} // closed by watchExit when the current child exits
	stopped bool         // true when we intentionally stopped (container shutdown)
}

// New creates a Supervisor for the given binary + argument list.
func New(bin string, args []string) *Supervisor {
	return &Supervisor{bin: bin, args: args}
}

// Start launches the child process and returns immediately.  The child's stdout
// and stderr are connected to the watcher's own stdout/stderr so that Docker
// log captures everything in one stream.
func (s *Supervisor) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked()
}

func (s *Supervisor) startLocked() error {
	cmd := exec.Command(s.bin, s.args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Put child in its own process group so we can signal the group if needed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd = cmd
	s.gen++
	s.done = make(chan struct{})
	myGen := s.gen
	done := s.done
	slog.Info("child process started", "pid", cmd.Process.Pid, "bin", s.bin)

	// Watch for unexpected exits in the background.  Only this goroutine
	// calls cmd.Wait() — Restart() and Stop() wait on the done channel
	// instead, avoiding double-Wait races.
	go s.watchExit(cmd, myGen, done)
	return nil
}

// watchExit waits for the child to finish.  If the watcher has not requested a
// stop — and the supervisor hasn't already moved on to a new child (Restart) —
// it immediately respawns the child.
func (s *Supervisor) watchExit(cmd *exec.Cmd, myGen uint64, done chan struct{}) {
	err := cmd.Wait()
	close(done) // signal Restart()/Stop() that the process has exited

	s.mu.Lock()
	defer s.mu.Unlock()

	// If Restart() already replaced this child, this goroutine is stale — bail.
	if s.gen != myGen {
		slog.Info("child exited cleanly on request", "err", err)
		return
	}

	if s.stopped {
		slog.Info("child exited cleanly on request", "err", err)
		return
	}

	// A clean exit (status 0) means file_follow finished successfully — for
	// example there are no Follower stanzas left.  Don't respawn; the next
	// config change will restart it via the normal path.
	if err == nil {
		slog.Info("child exited with status 0, not respawning (no work to do?)")
		return
	}

	slog.Warn("child exited unexpectedly, respawning", "err", err)
	time.Sleep(respawnDelay)

	if err2 := s.startLocked(); err2 != nil {
		slog.Error("failed to respawn child", "err", err2)
	}
}

// Restart sends SIGTERM to the current child, waits for it to exit (up to
// stopTimeout), then starts a fresh child.  Safe to call from the event loop.
func (s *Supervisor) Restart() error {
	slog.Info("restarting child process")

	s.mu.Lock()
	old := s.cmd
	done := s.done
	s.stopped = true // prevent watchExit from respawning the old process
	s.mu.Unlock()

	if old != nil && old.Process != nil {
		_ = old.Process.Signal(syscall.SIGTERM)
		// Wait for watchExit to signal completion — it is the sole caller of
		// cmd.Wait(), so there is no double-Wait race.
		if done != nil {
			select {
			case <-done:
			case <-time.After(stopTimeout):
				slog.Warn("child did not exit within timeout, sending SIGKILL")
				_ = old.Process.Signal(syscall.SIGKILL)
				<-done
			}
		}
	}

	s.mu.Lock()
	s.stopped = false
	err := s.startLocked()
	s.mu.Unlock()
	return err
}

// Stop terminates the child cleanly.  It is called when the watcher itself
// receives a shutdown signal.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	s.stopped = true
	cmd := s.cmd
	done := s.done
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	slog.Info("stopping child process", "pid", cmd.Process.Pid)
	_ = cmd.Process.Signal(syscall.SIGTERM)

	if done != nil {
		select {
		case <-done:
			slog.Info("child exited cleanly")
		case <-time.After(stopTimeout):
			slog.Warn("child did not exit within timeout, sending SIGKILL")
			_ = cmd.Process.Signal(syscall.SIGKILL)
			<-done
		}
	}
}
