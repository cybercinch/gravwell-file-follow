package follower_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybercinch/docker-log-watcher/internal/follower"
)

// ─── SanitizeName ────────────────────────────────────────────────────────────

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"/my-container", "my-container"},
		{"my_container", "my_container"},
		{"my.container", "my_container"},
		{"my container", "my_container"},
		{"my/container", "my_container"},
		{"MixedCase-123", "MixedCase-123"},
		{"/leading-slash", "leading-slash"},
		{"double//slash", "double__slash"},
		{"", ""},
	}
	for _, tc := range cases {
		got := follower.SanitizeName(tc.input)
		if got != tc.want {
			t.Errorf("SanitizeName(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

func TestTagFromName(t *testing.T) {
	if got := follower.TagFromName("/nginx"); got != "nginx" {
		t.Errorf("TagFromName(%q) = %q; want %q", "/nginx", got, "nginx")
	}
	if got := follower.TagFromName("my.service"); got != "my_service" {
		t.Errorf("TagFromName(%q) = %q; want %q", "my.service", got, "my_service")
	}
}

// ─── Write / Exists / Remove ─────────────────────────────────────────────────

func TestWrite(t *testing.T) {
	confDir := t.TempDir()
	containerDir := t.TempDir()

	gen := follower.NewGenerator(confDir, containerDir)

	containerID := "abc123def456789"
	containerName := "test-container"
	tag := "test-tag"

	path, err := gen.Write(containerID, containerName, tag, "", "", false, 0, "", "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// File should be at confDir/test-container.conf
	expected := filepath.Join(confDir, "test-container.conf")
	if path != expected {
		t.Errorf("Write returned path %q; want %q", path, expected)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	s := string(content)

	// Verify key sections of the generated config.
	checks := []string{
		`[Follower "test-container"]`,
		`Tag-Name=test-tag`,
		`File-Filter="*-json.log"`,
		containerID,
		containerDir,
	}
	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("generated config missing %q\n\nFull content:\n%s", check, s)
		}
	}
}

func TestAtomicWrite(t *testing.T) {
	// After Write, no .tmp file should remain.
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	if _, err := gen.Write("id123", "myapp", "myapp", "", "", false, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, _ := os.ReadDir(confDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover tmp file found: %s", e.Name())
		}
	}
}

func TestExists(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	if gen.Exists("ghost") {
		t.Error("Exists should be false before Write")
	}

	if _, err := gen.Write("id1", "ghost", "ghost", "", "", false, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !gen.Exists("ghost") {
		t.Error("Exists should be true after Write")
	}
}

func TestRemove(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	if _, err := gen.Write("id1", "mycontainer", "mycontainer", "", "", false, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	removed, err := gen.Remove("mycontainer")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("Remove should return true when file was deleted")
	}

	if gen.Exists("mycontainer") {
		t.Error("config should not exist after Remove")
	}
}

func TestRemoveNonExistent(t *testing.T) {
	gen := follower.NewGenerator(t.TempDir(), t.TempDir())

	// Remove of a non-existent name should be a no-op — no error, removed=false.
	removed, err := gen.Remove("does-not-exist")
	if err != nil {
		t.Errorf("Remove of non-existent should be no-op, got: %v", err)
	}
	if removed {
		t.Error("Remove of non-existent should return removed=false")
	}
}

func TestWriteSanitisesName(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	// Container name with dots and slashes — should be sanitised to underscores.
	if _, err := gen.Write("id1", "/my.service.1", "mytag", "", "", false, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// File should exist under the sanitised name.
	expected := filepath.Join(confDir, "my_service_1.conf")
	if _, err := os.Stat(expected); os.IsNotExist(err) {
		t.Errorf("expected sanitised config file %q to exist", expected)
	}
}

func TestWriteBaseDirectoryInConfig(t *testing.T) {
	confDir := t.TempDir()
	containerDir := "/var/lib/docker/containers"
	gen := follower.NewGenerator(confDir, containerDir)

	containerID := "deadbeef1234567890"
	if _, err := gen.Write(containerID, "svc", "svc", "", "", false, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(confDir, "svc.conf"))
	wantDir := filepath.Join(containerDir, containerID)
	if !strings.Contains(string(content), wantDir) {
		t.Errorf("Base-Directory %q not found in config:\n%s", wantDir, content)
	}
}

// ─── Log-dir / file-filter overrides ─────────────────────────────────────────

func TestWriteLogDirOverride(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, "/var/lib/docker/containers")

	customDir := "/mnt/app-logs"
	if _, err := gen.Write("id1", "myapp", "myapp", customDir, "", false, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(confDir, "myapp.conf"))
	s := string(content)
	if !strings.Contains(s, `Base-Directory="`+customDir+`"`) {
		t.Errorf("expected custom log dir %q in config:\n%s", customDir, s)
	}
	// Default docker container path must NOT appear.
	if strings.Contains(s, "/var/lib/docker/containers") {
		t.Errorf("default container dir should not appear when logDir is overridden:\n%s", s)
	}
}

func TestWriteFileFilterOverride(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	customFilter := "*.log"
	if _, err := gen.Write("id1", "myapp", "myapp", "", customFilter, false, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(confDir, "myapp.conf"))
	s := string(content)
	if !strings.Contains(s, `File-Filter="`+customFilter+`"`) {
		t.Errorf("expected custom file filter %q in config:\n%s", customFilter, s)
	}
}

func TestWriteDefaultsWhenOverridesEmpty(t *testing.T) {
	confDir := t.TempDir()
	containerDir := t.TempDir()
	gen := follower.NewGenerator(confDir, containerDir)

	containerID := "abc123"
	if _, err := gen.Write(containerID, "svc", "svc", "", "", false, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(confDir, "svc.conf"))
	s := string(content)
	if !strings.Contains(s, `File-Filter="*-json.log"`) {
		t.Errorf("expected default file filter in config:\n%s", s)
	}
	if !strings.Contains(s, filepath.Join(containerDir, containerID)) {
		t.Errorf("expected default log dir in config:\n%s", s)
	}
}

// ─── Persist marker ──────────────────────────────────────────────────────────

func TestPersistMarkerWrittenAndCleared(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	// Write with persist=true — marker should appear.
	if _, err := gen.Write("id1", "myapp", "myapp", "", "", true, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !gen.IsPersisted("myapp") {
		t.Error("IsPersisted should be true after Write with persist=true")
	}

	// Rewrite with persist=false — marker should be cleared.
	if _, err := gen.Write("id1", "myapp", "myapp", "", "", false, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if gen.IsPersisted("myapp") {
		t.Error("IsPersisted should be false after Write with persist=false")
	}
}

func TestPersistMarkerRemovedOnRemove(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	if _, err := gen.Write("id1", "myapp", "myapp", "", "", true, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !gen.IsPersisted("myapp") {
		t.Fatal("expected persist marker before Remove")
	}

	removed, err := gen.Remove("myapp")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("Remove should return true")
	}
	if gen.IsPersisted("myapp") {
		t.Error("IsPersisted should be false after Remove")
	}
}

func TestIsPersistedFalseByDefault(t *testing.T) {
	gen := follower.NewGenerator(t.TempDir(), t.TempDir())

	if gen.IsPersisted("nonexistent") {
		t.Error("IsPersisted should be false for unknown container")
	}
}

// ─── Persist TTL ─────────────────────────────────────────────────────────────

func TestPersistTTLStoredInMarker(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	if _, err := gen.Write("id1", "myapp", "myapp", "", "", true, 24*time.Hour, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if ttl := gen.PersistTTL("myapp"); ttl != 24*time.Hour {
		t.Errorf("PersistTTL = %v; want 24h", ttl)
	}
}

func TestPersistTTLZeroWhenNoTTL(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	if _, err := gen.Write("id1", "myapp", "myapp", "", "", true, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if ttl := gen.PersistTTL("myapp"); ttl != 0 {
		t.Errorf("PersistTTL = %v; want 0", ttl)
	}
}

func TestSetAndReadExpiry(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	if _, err := gen.Write("id1", "myapp", "myapp", "", "", true, time.Hour, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := gen.SetExpiry("myapp", expiry); err != nil {
		t.Fatalf("SetExpiry: %v", err)
	}

	got, ok := gen.ReadExpiry("myapp")
	if !ok {
		t.Fatal("ReadExpiry returned false")
	}
	if !got.Equal(expiry) {
		t.Errorf("ReadExpiry = %v; want %v", got, expiry)
	}
}

func TestWriteClearsExpiryOnRestart(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	if _, err := gen.Write("id1", "myapp", "myapp", "", "", true, time.Hour, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := gen.SetExpiry("myapp", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetExpiry: %v", err)
	}

	// Simulate container restart — Write should clear the expiry.
	if _, err := gen.Write("id2", "myapp", "myapp", "", "", true, time.Hour, "", ""); err != nil {
		t.Fatalf("Write (restart): %v", err)
	}
	if _, ok := gen.ReadExpiry("myapp"); ok {
		t.Error("ReadExpiry should return false after Write (expiry cleared on restart)")
	}
}

func TestRemoveClearsExpiry(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	if _, err := gen.Write("id1", "myapp", "myapp", "", "", true, time.Hour, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := gen.SetExpiry("myapp", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetExpiry: %v", err)
	}

	if _, err := gen.Remove("myapp"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := gen.ReadExpiry("myapp"); ok {
		t.Error("ReadExpiry should return false after Remove")
	}
}

// ─── Timestamp config ────────────────────────────────────────────────────────

func TestWriteTimestampConfig(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	regex := `\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d+[+-]\d{2}:\d{2}`
	format := "2006-01-02 15:04:05.999999-07:00"

	if _, err := gen.Write("id1", "myapp", "myapp", "", "", false, 0, regex, format); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(confDir, "myapp.conf"))
	s := string(content)
	if !strings.Contains(s, "Timestamp-Regex=`"+regex+"`") {
		t.Errorf("Timestamp-Regex not found in config:\n%s", s)
	}
	if !strings.Contains(s, `Timestamp-Format-String="`+format+`"`) {
		t.Errorf("Timestamp-Format-String not found in config:\n%s", s)
	}
}

func TestWriteTimestampConfigOmittedWhenEmpty(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	if _, err := gen.Write("id1", "myapp", "myapp", "", "", false, 0, "", ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(confDir, "myapp.conf"))
	s := string(content)
	if strings.Contains(s, "Timestamp-Regex") {
		t.Errorf("Timestamp-Regex should not appear in config when empty:\n%s", s)
	}
	if strings.Contains(s, "Timestamp-Format-String") {
		t.Errorf("Timestamp-Format-String should not appear in config when empty:\n%s", s)
	}
}

// ─── ListConfigs ─────────────────────────────────────────────────────────────

func TestListConfigsEmpty(t *testing.T) {
	gen := follower.NewGenerator(t.TempDir(), t.TempDir())

	names, err := gen.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected empty list, got %v", names)
	}
}

func TestListConfigs(t *testing.T) {
	confDir := t.TempDir()
	gen := follower.NewGenerator(confDir, t.TempDir())

	// Write a few configs.
	for _, name := range []string{"nginx", "redis", "postgres"} {
		if _, err := gen.Write("id-"+name, name, name, "", "", false, 0, "", ""); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
	}

	// Drop a non-.conf file — it should be ignored.
	os.WriteFile(filepath.Join(confDir, "README.txt"), []byte("hi"), 0644)

	names, err := gen.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 configs, got %d: %v", len(names), names)
	}

	want := map[string]bool{"nginx": true, "redis": true, "postgres": true}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected config name %q", n)
		}
	}
}
