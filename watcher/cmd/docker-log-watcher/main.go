// docker-log-watcher: watches Docker container lifecycle events and dynamically
// manages gravwell_file_follow configuration overlays.
//
// It acts as a lightweight process supervisor for gravwell_file_follow:
//
//	tini → docker-log-watcher (PID 1 child)
//	              └─ gravwell_file_follow (child)
//
// Usage:
//
//	docker-log-watcher [flags]
//
// All settings can be provided via environment variables (see internal/config).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	dockerclient "github.com/docker/docker/client"

	"github.com/cybercinch/docker-log-watcher/internal/config"
	"github.com/cybercinch/docker-log-watcher/internal/docker"
	"github.com/cybercinch/docker-log-watcher/internal/follower"
	"github.com/cybercinch/docker-log-watcher/internal/supervisor"
)

func main() {
	cfg := config.Load()
	initLogging(cfg.LogLevel)

	// Auto-detect container directory from the runtime if not explicitly set.
	// The host root is expected at /host (bind-mounted read-only).
	if cfg.ContainerDir == "" {
		if detected := autoDetectContainerDir(&cfg); detected != "" {
			cfg.ContainerDir = detected
		} else {
			cfg.ContainerDir = "/host/" + defaultContainerDir(cfg.Backend)
			slog.Warn("auto-detection failed, falling back to default container dir",
				"backend", cfg.Backend, "containerDir", cfg.ContainerDir)
		}
	}

	slog.Info("container-log-watcher starting",
		"backend", cfg.Backend,
		"confDir", cfg.ConfDir,
		"containerDir", cfg.ContainerDir,
		"debounce", cfg.DebounceDuration,
		"destroyDelay", cfg.DestroyDelay,
		"defaultTag", cfg.DefaultTag,
		"excludeNames", cfg.ExcludeNames,
	)

	// Ensure the conf.d overlay directory exists.
	if err := os.MkdirAll(cfg.ConfDir, 0750); err != nil {
		slog.Error("cannot create conf dir", "path", cfg.ConfDir, "err", err)
		os.Exit(1)
	}

	gen := follower.NewGeneratorWithBackend(cfg.ConfDir, cfg.ContainerDir, cfg.ContainerLogSubdir(), cfg.DefaultFileFilter())
	sup := supervisor.New(cfg.FileFollowBin, cfg.FileFollowArgs)
	watcher := docker.New(cfg, gen, sup)

	// Root context — cancelled on SIGTERM / SIGINT.
	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Run blocks until ctx is cancelled.
	if err := watcher.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("watcher exited with error", "err", err)
	}

	// Cleanly stop the child process.
	sup.Stop()
	slog.Info("docker-log-watcher stopped")
}

// autoDetectContainerDir queries the container runtime for its graph root and
// returns the per-container storage directory as seen from inside the watcher
// container (/host is where the host root is bind-mounted).
func autoDetectContainerDir(cfg *config.Config) string {
	socket := cfg.ResolvedSocketPath()
	opts := []dockerclient.Opt{dockerclient.WithAPIVersionNegotiation()}
	if socket != "" {
		opts = append(opts, dockerclient.WithHost("unix://"+socket))
	} else {
		opts = append(opts, dockerclient.FromEnv)
	}
	cli, err := dockerclient.NewClientWithOpts(opts...)
	if err != nil {
		slog.Warn("auto-detection: failed to create runtime client", "backend", cfg.Backend, "err", err)
		return ""
	}
	defer cli.Close()

	info, err := cli.Info(context.Background())
	if err != nil {
		slog.Warn("auto-detection: failed to query runtime info", "backend", cfg.Backend, "err", err)
		return ""
	}

	// Both Docker and Podman expose their graph root via DockerRootDir in the
	// Docker-compatible Info response.  The sub-path under that root differs.
	dir := filepath.Join("/host", info.DockerRootDir, cfg.ContainerStorageSubdir())
	slog.Info("auto-detected container directory",
		"backend", cfg.Backend, "graphRoot", info.DockerRootDir, "containerDir", dir)
	return dir
}

// defaultContainerDir returns the conventional container storage path
// (relative to the host root) for the given backend.
func defaultContainerDir(backend string) string {
	if backend == "podman" {
		return "var/lib/containers/storage/overlay-containers"
	}
	return "var/lib/docker/containers"
}

// initLogging configures the global slog handler based on a level string.
func initLogging(level string) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}
