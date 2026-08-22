package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePathsHonorsOverrides(t *testing.T) {
	t.Setenv(EnvDataDir, "/tmp/x/data")
	t.Setenv(EnvConfigDir, "/tmp/x/cfg")
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.DataDir != "/tmp/x/data" || p.ConfigDir != "/tmp/x/cfg" {
		t.Fatalf("paths = %+v", p)
	}
	if p.EngineDir() != filepath.Join("/tmp/x/data", "engine") {
		t.Fatalf("engine dir = %s", p.EngineDir())
	}
}

func TestResolvePathsXDGDefaults(t *testing.T) {
	t.Setenv(EnvDataDir, "")
	t.Setenv(EnvConfigDir, "")
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg-data")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-config")
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.DataDir != filepath.Join("/tmp/xdg-data", "iar") {
		t.Fatalf("data dir = %s", p.DataDir)
	}
	if p.ConfigDir != filepath.Join("/tmp/xdg-config", "iar") {
		t.Fatalf("config dir = %s", p.ConfigDir)
	}
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDataDir, filepath.Join(dir, "data"))
	t.Setenv(EnvConfigDir, filepath.Join(dir, "cfg"))
	p, _ := ResolvePaths()
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "acestep" || cfg.TrackSeconds != 150 || cfg.ACEStep.InferenceSteps != 12 {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

func TestLoadOverridesAndSanitizes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDataDir, filepath.Join(dir, "data"))
	t.Setenv(EnvConfigDir, dir)
	body := `{"engine":"noise","track_seconds":10000,"volume":250,"crossfade_seconds":0.1}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _ := ResolvePaths()
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "noise" {
		t.Fatalf("engine = %s", cfg.Engine)
	}
	if cfg.TrackSeconds != 300 || cfg.Volume != 100 || cfg.CrossfadeSeconds != 0.5 {
		t.Fatalf("sanitize failed: %+v", cfg)
	}
}

func TestLoadRejectsMalformedFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDataDir, filepath.Join(dir, "data"))
	t.Setenv(EnvConfigDir, dir)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{broken"), 0o644)
	p, _ := ResolvePaths()
	if _, err := Load(p); err == nil {
		t.Fatal("malformed config accepted")
	}
}

func TestBindListAcceptsStringAndList(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDataDir, filepath.Join(dir, "data"))
	t.Setenv(EnvConfigDir, dir)

	// A plain string (the shape of existing config files) is one entry.
	os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"remote":{"bind":"0.0.0.0","allowed_hosts":["192.168.8.187"]}}`), 0o644)
	p, _ := ResolvePaths()
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Remote.Bind) != 1 || cfg.Remote.Bind[0] != "0.0.0.0" {
		t.Fatalf("string bind = %v", cfg.Remote.Bind)
	}

	// A list works too.
	os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"remote":{"bind":["192.168.1.5","10.0.0.2"]}}`), 0o644)
	cfg, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Remote.Bind) != 2 || cfg.Remote.Bind[1] != "10.0.0.2" {
		t.Fatalf("list bind = %v", cfg.Remote.Bind)
	}

	// Empty string means no extra binds.
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"remote":{"bind":""}}`), 0o644)
	cfg, _ = Load(p)
	if len(cfg.Remote.Bind) != 0 {
		t.Fatalf("empty bind = %v", cfg.Remote.Bind)
	}
}

func TestMigrationFromOldName(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvDataDir, "")
	t.Setenv(EnvConfigDir, "")
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	// A previous install: engine payload and a config file.
	oldData := filepath.Join(root, "data", "bgm")
	oldConfig := filepath.Join(root, "config", "bgm")
	os.MkdirAll(filepath.Join(oldData, "engine", "checkpoints"), 0o755)
	os.MkdirAll(filepath.Join(oldData, "logs"), 0o755)
	os.MkdirAll(oldConfig, 0o755)
	os.WriteFile(filepath.Join(oldData, "engine", "checkpoints", "weights.bin"), []byte("payload"), 0o644)
	// A venv entry point with an absolute shebang into the old path.
	os.MkdirAll(filepath.Join(oldData, "engine", ".venv", "bin"), 0o755)
	os.WriteFile(filepath.Join(oldData, "engine", ".venv", "bin", "engine-api"),
		[]byte("#!"+filepath.Join(oldData, "engine", ".venv", "bin", "python")+"\nrun()\n"), 0o755)
	os.WriteFile(filepath.Join(oldData, "logs", "bgm.log"), []byte("old log"), 0o644)
	os.WriteFile(filepath.Join(oldConfig, "config.json"), []byte(`{"volume":42}`), 0o644)

	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.DataDir != filepath.Join(root, "data", "iar") {
		t.Fatalf("data dir = %s", p.DataDir)
	}
	// Everything moved, nothing copied or lost.
	if _, err := os.Stat(filepath.Join(p.DataDir, "engine", "checkpoints", "weights.bin")); err != nil {
		t.Fatal("engine payload did not survive migration")
	}
	if _, err := os.Stat(filepath.Join(p.DataDir, "logs", "iar.log")); err != nil {
		t.Fatal("log file not renamed")
	}
	shebang, _ := os.ReadFile(filepath.Join(p.DataDir, "engine", ".venv", "bin", "engine-api"))
	if !strings.Contains(string(shebang), p.DataDir) || strings.Contains(string(shebang), oldData) {
		t.Fatalf("venv shebang not repointed: %q", shebang)
	}
	if _, err := os.Stat(oldData); err == nil {
		t.Fatal("old data dir still present after migration")
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Volume != 42 {
		t.Fatalf("config content lost: volume=%d", cfg.Volume)
	}

	// Idempotent: a second resolve changes nothing.
	p2, err := ResolvePaths()
	if err != nil || p2.DataDir != p.DataDir {
		t.Fatalf("second resolve: %v %v", p2, err)
	}
}

func TestMigrationDefersWhileOldInstallBusy(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvDataDir, "")
	t.Setenv(EnvConfigDir, "")
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	oldData := filepath.Join(root, "data", "bgm")
	os.MkdirAll(filepath.Join(oldData, "state"), 0o755)
	// A live engine daemon from the old install (our own pid is alive).
	os.WriteFile(filepath.Join(oldData, "state", "engine.json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"port":1}`, os.Getpid())), 0o644)

	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.DataDir != oldData {
		t.Fatalf("busy old install must keep old paths, got %s", p.DataDir)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "iar")); err == nil {
		t.Fatal("migration ran despite the live old daemon")
	}
}

func TestLoadPurgesObsoleteTokenKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDataDir, filepath.Join(dir, "data"))
	t.Setenv(EnvConfigDir, dir)
	file := filepath.Join(dir, "config.json")
	body := `{"volume":33,"remote":{"enabled":true,"port":9000,"token":"old-secret","bind":["192.168.1.5"],
	  "allowed_hosts":["radio.lan"],"smtp":{"host":"smtp.example.com","password":"p"}},"custom_key":{"a":1}}`
	os.WriteFile(file, []byte(body), 0o644)
	p, _ := ResolvePaths()
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// Everything else survives, in memory and on disk.
	if cfg.Volume != 33 || !cfg.Remote.Enabled || cfg.Remote.Port != 9000 ||
		len(cfg.Remote.Bind) != 1 || cfg.Remote.AllowedHosts[0] != "radio.lan" ||
		cfg.Remote.SMTP.Host != "smtp.example.com" {
		t.Fatalf("config lost values: %+v", cfg)
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "token") || strings.Contains(string(raw), "old-secret") {
		t.Fatalf("obsolete key survived in the file: %s", raw)
	}
	for _, keep := range []string{`"custom_key"`, `"radio.lan"`, `"smtp.example.com"`, `"volume": 33`} {
		if !strings.Contains(string(raw), keep) {
			t.Fatalf("rewrite dropped %s: %s", keep, raw)
		}
	}
	fi, _ := os.Stat(file)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("rewritten config mode = %o, want 0600", fi.Mode().Perm())
	}
	// Idempotent: a clean file is left alone (mode and content).
	os.Chmod(file, 0o644)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(file)
	if fi2.Mode().Perm() != 0o644 {
		t.Fatal("clean config was rewritten")
	}
	if _, changed := purgeObsoleteKeys([]byte(`{"remote":{"port":1}}`)); changed {
		t.Fatal("purge reports a change on a clean document")
	}
}
