package config

import (
	"os"
	"path/filepath"
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
	if p.DataDir != filepath.Join("/tmp/xdg-data", "bgm") {
		t.Fatalf("data dir = %s", p.DataDir)
	}
	if p.ConfigDir != filepath.Join("/tmp/xdg-config", "bgm") {
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
