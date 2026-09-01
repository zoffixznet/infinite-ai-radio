package config

import (
	"encoding/json"
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
		[]byte(`{"remote":{"bind":"0.0.0.0","allowed_hosts":["192.168.1.42"]}}`), 0o644)
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

// The phone remote edits vocal_languages in place; every other setting
// in the file, including ones this build does not know about, survives.
func TestSetVocalLanguagesPreservesTheRestOfTheFile(t *testing.T) {
	dir := t.TempDir()
	p := Paths{ConfigDir: dir, DataDir: filepath.Join(dir, "data")}
	original := `{"engine":"acestep","volume":42,"remote":{"enabled":true,"port":9000},"future_setting":"keep me"}`
	if err := os.WriteFile(p.ConfigFile(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetVocalLanguages(p, []string{"English", "Bisaya (Cebuano)"}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	data, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("rewritten config is not valid JSON: %v\n%s", err, data)
	}
	if doc["future_setting"] != "keep me" || doc["engine"] != "acestep" {
		t.Errorf("unrelated settings were lost: %s", data)
	}
	if remote, ok := doc["remote"].(map[string]any); !ok || remote["port"].(float64) != 9000 {
		t.Errorf("nested settings were lost: %s", data)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.VocalLanguages, "|") != "English|Bisaya (Cebuano)" {
		t.Errorf("languages did not round-trip: %v", cfg.VocalLanguages)
	}
	// Clearing them is a first-class state, not a missing key.
	if err := SetVocalLanguages(p, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.VocalLanguages) != 0 {
		t.Errorf("languages were not cleared: %v", cfg.VocalLanguages)
	}
}

// A machine with no config file yet still gets its languages written.
func TestSetVocalLanguagesCreatesTheFile(t *testing.T) {
	dir := t.TempDir()
	p := Paths{ConfigDir: filepath.Join(dir, "config"), DataDir: filepath.Join(dir, "data")}
	if err := SetVocalLanguages(p, []string{"Russian"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.VocalLanguages) != 1 || cfg.VocalLanguages[0] != "Russian" {
		t.Errorf("languages = %v", cfg.VocalLanguages)
	}
}
