package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/cybercinch/docker-log-watcher/internal/config"
)

func TestDefaults(t *testing.T) {
	// Clear any WATCHER_* vars that might be set in the environment.
	clearEnv(t)

	cfg := config.Load()

	if cfg.ConfDir != "/opt/gravwell/etc/file_follow.conf.d" {
		t.Errorf("ConfDir default: got %q", cfg.ConfDir)
	}
	if cfg.ContainerDir != "" {
		t.Errorf("ContainerDir default: got %q (want empty — auto-detected at runtime)", cfg.ContainerDir)
	}
	if cfg.DefaultTag != "docker" {
		t.Errorf("DefaultTag default: got %q", cfg.DefaultTag)
	}
	if cfg.DebounceDuration != 2*time.Second {
		t.Errorf("DebounceDuration default: got %v", cfg.DebounceDuration)
	}
	if cfg.DestroyDelay != 30*time.Second {
		t.Errorf("DestroyDelay default: got %v", cfg.DestroyDelay)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default: got %q", cfg.LogLevel)
	}
	if cfg.FileFollowBin != "/opt/gravwell/bin/gravwell_file_follow" {
		t.Errorf("FileFollowBin default: got %q", cfg.FileFollowBin)
	}
}

func TestEnvOverrides(t *testing.T) {
	clearEnv(t)

	t.Setenv("WATCHER_CONF_DIR", "/tmp/conf.d")
	t.Setenv("WATCHER_CONTAINER_DIR", "/tmp/containers")
	t.Setenv("WATCHER_DEFAULT_TAG", "mytag")
	t.Setenv("WATCHER_DEBOUNCE_SECS", "5")
	t.Setenv("WATCHER_DESTROY_DELAY_SECS", "60")
	t.Setenv("WATCHER_LOG_LEVEL", "debug")

	cfg := config.Load()

	if cfg.ConfDir != "/tmp/conf.d" {
		t.Errorf("ConfDir: got %q", cfg.ConfDir)
	}
	if cfg.ContainerDir != "/tmp/containers" {
		t.Errorf("ContainerDir: got %q", cfg.ContainerDir)
	}
	if cfg.DefaultTag != "mytag" {
		t.Errorf("DefaultTag: got %q", cfg.DefaultTag)
	}
	if cfg.DebounceDuration != 5*time.Second {
		t.Errorf("DebounceDuration: got %v", cfg.DebounceDuration)
	}
	if cfg.DestroyDelay != 60*time.Second {
		t.Errorf("DestroyDelay: got %v", cfg.DestroyDelay)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q", cfg.LogLevel)
	}
}

func TestSelfAlwaysExcluded(t *testing.T) {
	clearEnv(t)

	t.Setenv("WATCHER_SELF_CONTAINER", "my-watcher")
	cfg := config.Load()

	if !cfg.ShouldExclude("my-watcher") {
		t.Error("self container should always be excluded")
	}
}

func TestExcludeNames(t *testing.T) {
	clearEnv(t)

	t.Setenv("WATCHER_EXCLUDE_NAMES", "portainer, traefik ,watchtower")
	cfg := config.Load()

	for _, name := range []string{"portainer", "traefik", "watchtower"} {
		if !cfg.ShouldExclude(name) {
			t.Errorf("expected %q to be excluded", name)
		}
	}
	if cfg.ShouldExclude("nginx") {
		t.Error("nginx should NOT be excluded")
	}
}

func TestExcludeNamesPrefixMatch(t *testing.T) {
	clearEnv(t)

	t.Setenv("WATCHER_EXCLUDE_NAMES", "buildx_buildkit,portainer")
	cfg := config.Load()

	// Exact matches still work.
	if !cfg.ShouldExclude("portainer") {
		t.Error("portainer should be excluded (exact)")
	}
	// Prefix matches work: "buildx_buildkit" matches anything starting with it.
	for _, name := range []string{
		"buildx_buildkit",
		"buildx_buildkit_builder-0",
		"buildx_buildkit_builder-abc123",
	} {
		if !cfg.ShouldExclude(name) {
			t.Errorf("expected %q to be excluded (prefix match)", name)
		}
	}
	// A container that only partially overlaps should NOT match.
	if cfg.ShouldExclude("buildx_other") {
		t.Error("buildx_other should NOT be excluded")
	}
	if cfg.ShouldExclude("nginx") {
		t.Error("nginx should NOT be excluded")
	}
}

func TestFileFollowArgsDefault(t *testing.T) {
	clearEnv(t)

	cfg := config.Load()

	want := []string{
		"-config-file", "/opt/gravwell/etc/file_follow.conf",
		"-config-overlays", "/opt/gravwell/etc/file_follow.conf.d",
	}
	if len(cfg.FileFollowArgs) != len(want) {
		t.Fatalf("FileFollowArgs len: got %d want %d", len(cfg.FileFollowArgs), len(want))
	}
	for i, a := range want {
		if cfg.FileFollowArgs[i] != a {
			t.Errorf("FileFollowArgs[%d]: got %q want %q", i, cfg.FileFollowArgs[i], a)
		}
	}
}

func TestFileFollowArgsCustom(t *testing.T) {
	clearEnv(t)

	t.Setenv("WATCHER_FF_ARGS", "-config-file,/custom/file_follow.conf,-verbose")
	cfg := config.Load()

	if len(cfg.FileFollowArgs) != 3 {
		t.Fatalf("FileFollowArgs len: got %d want 3", len(cfg.FileFollowArgs))
	}
	if cfg.FileFollowArgs[0] != "-config-file" {
		t.Errorf("FileFollowArgs[0]: got %q", cfg.FileFollowArgs[0])
	}
}

func TestStaticConfDirDefault(t *testing.T) {
	clearEnv(t)

	cfg := config.Load()

	if cfg.StaticConfDir != "" {
		t.Errorf("StaticConfDir default: got %q (want empty)", cfg.StaticConfDir)
	}
	// Default FileFollowArgs should NOT contain a second -config-overlays.
	count := 0
	for _, a := range cfg.FileFollowArgs {
		if a == "-config-overlays" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 -config-overlays in FileFollowArgs, got %d: %v", count, cfg.FileFollowArgs)
	}
}

func TestStaticConfDirAddsOverlay(t *testing.T) {
	clearEnv(t)

	t.Setenv("WATCHER_STATIC_CONF_DIR", "/opt/gravwell/etc/static.conf.d")
	cfg := config.Load()

	if cfg.StaticConfDir != "/opt/gravwell/etc/static.conf.d" {
		t.Errorf("StaticConfDir: got %q", cfg.StaticConfDir)
	}
	// FileFollowArgs should have two -config-overlays entries.
	count := 0
	found := false
	for i, a := range cfg.FileFollowArgs {
		if a == "-config-overlays" {
			count++
			if i+1 < len(cfg.FileFollowArgs) && cfg.FileFollowArgs[i+1] == "/opt/gravwell/etc/static.conf.d" {
				found = true
			}
		}
	}
	if count != 2 {
		t.Errorf("expected 2 -config-overlays in FileFollowArgs, got %d: %v", count, cfg.FileFollowArgs)
	}
	if !found {
		t.Errorf("static conf dir not found in FileFollowArgs: %v", cfg.FileFollowArgs)
	}
}

func TestOptInDefault(t *testing.T) {
	clearEnv(t)

	cfg := config.Load()

	if cfg.OptIn {
		t.Error("OptIn default should be false (opt-out mode)")
	}
}

func TestOptInEnabled(t *testing.T) {
	clearEnv(t)

	t.Setenv("WATCHER_OPT_IN", "true")
	cfg := config.Load()

	if !cfg.OptIn {
		t.Error("OptIn should be true when WATCHER_OPT_IN=true")
	}
}

func TestDebounceInvalidFallsBack(t *testing.T) {
	clearEnv(t)

	t.Setenv("WATCHER_DEBOUNCE_SECS", "not-a-number")
	cfg := config.Load()

	// Invalid values should fall back to the default (2s).
	if cfg.DebounceDuration != 2*time.Second {
		t.Errorf("DebounceDuration on invalid input: got %v", cfg.DebounceDuration)
	}
}

// clearEnv unsets all WATCHER_* environment variables for the duration of the
// test and restores them via t.Cleanup.
func clearEnv(t *testing.T) {
	t.Helper()
	vars := []string{
		"WATCHER_CONF_DIR", "WATCHER_CONTAINER_DIR", "WATCHER_DEFAULT_TAG",
		"WATCHER_SELF_CONTAINER", "WATCHER_EXCLUDE_NAMES", "WATCHER_DEBOUNCE_SECS",
		"WATCHER_DESTROY_DELAY_SECS", "WATCHER_FF_BIN", "WATCHER_FF_ARGS",
		"WATCHER_LOG_LEVEL", "WATCHER_OPT_IN", "WATCHER_STATIC_CONF_DIR",
	}
	for _, v := range vars {
		old, hadOld := os.LookupEnv(v)
		os.Unsetenv(v)
		if hadOld {
			t.Cleanup(func() { os.Setenv(v, old) })
		}
	}
}
