package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Paths resolves every directory the application reads or writes. All state
// lives under two roots (data and config) so tests and scripts can redirect
// the whole application with the IAR_DATA_DIR and IAR_CONFIG_DIR environment
// variables.
type Paths struct {
	// DataDir is the root for engine installs, sessions, exports and logs.
	DataDir string
	// ConfigDir holds the user configuration file.
	ConfigDir string
}

// Environment variables honored by ResolvePaths.
const (
	EnvDataDir   = "IAR_DATA_DIR"
	EnvConfigDir = "IAR_CONFIG_DIR"
)

// ResolvePaths determines the data and config directories from the
// environment: explicit IAR_* overrides win, then the platform's user
// directories (XDG on Linux).
func ResolvePaths() (Paths, error) {
	var p Paths
	if dir := os.Getenv(EnvDataDir); dir != "" {
		p.DataDir = dir
	} else {
		base, err := userDataHome()
		if err != nil {
			return Paths{}, fmt.Errorf("resolving data directory: %w", err)
		}
		p.DataDir = filepath.Join(base, "iar")
	}
	if dir := os.Getenv(EnvConfigDir); dir != "" {
		p.ConfigDir = dir
	} else {
		base, err := os.UserConfigDir()
		if err != nil {
			return Paths{}, fmt.Errorf("resolving config directory: %w", err)
		}
		p.ConfigDir = filepath.Join(base, "iar")
	}
	// A previous install under the old name is migrated into place (or
	// used as-is while it is still running). Explicit env overrides are
	// exempt: they point exactly where the caller wants.
	if os.Getenv(EnvDataDir) == "" && os.Getenv(EnvConfigDir) == "" {
		p = migrateOld(p)
	}
	return p, nil
}

// EnsureDirs creates every directory the application expects to exist.
func (p Paths) EnsureDirs() error {
	for _, dir := range []string{
		p.DataDir,
		p.ConfigDir,
		p.SessionsDir(),
		p.ExportsDir(),
		p.LogsDir(),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return nil
}

// EngineDir is where the music engine (a git checkout with its own
// virtualenv and model checkpoints) is installed.
func (p Paths) EngineDir() string { return filepath.Join(p.DataDir, "engine") }

// SessionsDir holds one JSON file per saved session.
func (p Paths) SessionsDir() string { return filepath.Join(p.DataDir, "sessions") }

// ExportsDir is the default destination for rendered MP3 files.
func (p Paths) ExportsDir() string { return filepath.Join(p.DataDir, "exports") }

// LogsDir holds the application log files.
func (p Paths) LogsDir() string { return filepath.Join(p.DataDir, "logs") }

// LogFile is the main structured log file.
func (p Paths) LogFile() string { return filepath.Join(p.LogsDir(), "iar.log") }

// ConfigFile is the JSON configuration file.
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "config.json") }
