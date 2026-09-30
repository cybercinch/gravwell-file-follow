// Package config parses all WATCHER_* environment variables and provides
// a single Config struct consumed by the rest of the application.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the runtime configuration for docker-log-watcher.
type Config struct {
	// Backend selects the container runtime. Supported values: "docker", "podman".
	// Controls default socket path, container directory layout, and log file glob.
	Backend string

	// SocketPath is the Unix socket path for the container runtime API.
	// When empty the canonical default for the selected Backend is used:
	//   docker → standard Docker client behaviour (respects DOCKER_HOST)
	//   podman → /run/podman/podman.sock
	// Set WATCHER_SOCKET to override regardless of backend.
	SocketPath string

	// ConfDir is the directory where per-container overlay configs are written.
	// Maps to /opt/gravwell/etc/file_follow.conf.d inside the container.
	ConfDir string

	// ContainerDir is the bind-mounted container storage path.
	// Auto-detected from the runtime API at startup when empty.
	ContainerDir string

	// DefaultTag is the Gravwell tag used when no gravwell.tag label is set and
	// the container name is somehow empty.
	DefaultTag string

	// SelfContainerName is the name of the watcher's own container.  It is
	// always excluded so we never ingest our own logs.
	SelfContainerName string

	// ExcludeNames is an optional list of container name prefixes that are
	// always skipped regardless of labels.  A container is excluded if its
	// name starts with any entry in this slice (prefix match).
	ExcludeNames []string

	// DebounceDuration is how long the watcher waits after the last event before
	// it writes configs and signals a file_follow restart.
	DebounceDuration time.Duration

	// DestroyDelay is how long the watcher waits after a container destroy
	// event before removing its config.  This gives file_follow time to drain
	// any remaining log data — especially important for short-lived / --rm
	// containers.  Set to 0 to remove configs immediately (old behaviour).
	DestroyDelay time.Duration

	// FileFollowBin is the path to the gravwell_file_follow binary.
	FileFollowBin string

	// FileFollowArgs are the arguments forwarded to gravwell_file_follow.
	FileFollowArgs []string

	// LogLevel controls the verbosity of the watcher's own log output.
	// Accepted values: debug, info, warn, error.
	LogLevel string

	// OptIn controls the global inclusion mode.  When false (default) every
	// container is ingested unless it opts out via gravwell.skip=true or an
	// ExcludeNames prefix.  When true, containers are skipped unless they carry
	// gravwell.enable=true — mirroring the Traefik exposedByDefault pattern.
	OptIn bool

	// StaticConfDir is an optional directory of hand-crafted [Follower] config
	// files for non-container log sources (e.g. host syslog, app logs).  When
	// set, it is passed to gravwell_file_follow as an additional
	// -config-overlays directory.  The watcher never reads or modifies files in
	// this directory — it is entirely user-managed.
	StaticConfDir string
}

// Load reads all WATCHER_* environment variables and returns a Config with
// sensible defaults for anything that is not set.
func Load() Config {
	backend := envOrDefault("WATCHER_BACKEND", "docker")
	cfg := Config{
		Backend:           backend,
		SocketPath:        os.Getenv("WATCHER_SOCKET"),
		ConfDir:           envOrDefault("WATCHER_CONF_DIR", "/opt/gravwell/etc/file_follow.conf.d"),
		ContainerDir:      os.Getenv("WATCHER_CONTAINER_DIR"), // empty → auto-detected at startup
		DefaultTag:        envOrDefault("WATCHER_DEFAULT_TAG", backend),
		SelfContainerName: envOrDefault("WATCHER_SELF_CONTAINER", "gravwell-file-follow"),
		DebounceDuration: parseDuration("WATCHER_DEBOUNCE_SECS", 2),
		DestroyDelay:     parseDuration("WATCHER_DESTROY_DELAY_SECS", 30),
		FileFollowBin:    envOrDefault("WATCHER_FF_BIN", "/opt/gravwell/bin/gravwell_file_follow"),
		LogLevel:         envOrDefault("WATCHER_LOG_LEVEL", "info"),
		OptIn:            os.Getenv("WATCHER_OPT_IN") == "true",
		StaticConfDir:    os.Getenv("WATCHER_STATIC_CONF_DIR"),
	}

	// Parse comma-separated exclude list into a slice for prefix matching.
	excludeRaw := os.Getenv("WATCHER_EXCLUDE_NAMES")
	if excludeRaw != "" {
		for _, name := range strings.Split(excludeRaw, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				cfg.ExcludeNames = append(cfg.ExcludeNames, name)
			}
		}
	}

	// Always exclude self.
	cfg.ExcludeNames = append(cfg.ExcludeNames, cfg.SelfContainerName)

	// Build the file_follow argument slice from WATCHER_FF_ARGS (comma-separated)
	// or fall back to sensible defaults.
	ffArgsRaw := os.Getenv("WATCHER_FF_ARGS")
	if ffArgsRaw != "" {
		for _, a := range strings.Split(ffArgsRaw, ",") {
			cfg.FileFollowArgs = append(cfg.FileFollowArgs, strings.TrimSpace(a))
		}
	} else {
		cfg.FileFollowArgs = []string{
			"-config-file", "/opt/gravwell/etc/file_follow.conf",
			"-config-overlays", "/opt/gravwell/etc/file_follow.conf.d",
		}
		if cfg.StaticConfDir != "" {
			cfg.FileFollowArgs = append(cfg.FileFollowArgs,
				"-config-overlays", cfg.StaticConfDir)
		}
	}

	return cfg
}

// ResolvedSocketPath returns the socket path to use for the container runtime
// API.  Explicit WATCHER_SOCKET wins; otherwise the backend default is used.
// An empty return value means "use standard Docker client env resolution".
func (c *Config) ResolvedSocketPath() string {
	if c.SocketPath != "" {
		return c.SocketPath
	}
	if c.Backend == "podman" {
		return "/run/podman/podman.sock"
	}
	return "" // Docker: honour DOCKER_HOST / default socket
}

// ContainerLogSubdir returns the subdirectory appended to <containerDir>/<id>
// to locate the container's log files.  Empty for Docker; "userdata" for Podman.
func (c *Config) ContainerLogSubdir() string {
	if c.Backend == "podman" {
		return "userdata"
	}
	return ""
}

// DefaultFileFilter returns the glob used to match log files within a
// container's log directory when no gravwell.log-filter label is set.
func (c *Config) DefaultFileFilter() string {
	if c.Backend == "podman" {
		return "*.log"
	}
	return "*-json.log"
}

// ContainerStorageSubdir returns the sub-path under the runtime's graph root
// that holds per-container directories.  "containers" for Docker;
// "overlay-containers" for Podman.
func (c *Config) ContainerStorageSubdir() string {
	if c.Backend == "podman" {
		return "overlay-containers"
	}
	return "containers"
}

// ShouldExclude reports whether the given container name should be skipped.
// A container is excluded if its name starts with any entry in ExcludeNames
// (prefix match), so "buildx_buildkit" will match "buildx_buildkit_builder-0".
func (c *Config) ShouldExclude(name string) bool {
	for _, prefix := range c.ExcludeNames {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// envOrDefault returns the value of the named environment variable, or
// fallback if the variable is unset or empty.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseDuration reads WATCHER_<KEY>_SECS and returns a time.Duration.
// If the variable is missing or invalid it falls back to defaultSecs seconds.
func parseDuration(key string, defaultSecs int) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return time.Duration(defaultSecs) * time.Second
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 0 {
		return time.Duration(defaultSecs) * time.Second
	}
	return time.Duration(secs) * time.Second
}
