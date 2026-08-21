package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"iar/internal/state"
)

// oldAppDir is the directory name state lived under before the product
// was renamed to Infinite AI Radio.
const oldAppDir = "bgm"

// migrateOld renames the previous install's data and config directories
// into place, so the engine install, library, sessions and configuration
// survive the rename with zero re-setup. It is idempotent and refuses to
// move anything while the old install is still in use (a live engine
// daemon or a running player); in that case the OLD paths are returned so
// the current run keeps working and migration happens on a later launch.
func migrateOld(p Paths) Paths {
	oldData := filepath.Join(filepath.Dir(p.DataDir), oldAppDir)
	oldConfig := filepath.Join(filepath.Dir(p.ConfigDir), oldAppDir)
	dataNeeded := dirExists(oldData) && !dirExists(p.DataDir)
	configNeeded := dirExists(oldConfig) && !dirExists(p.ConfigDir)
	if !dataNeeded && !configNeeded {
		return p
	}
	if dataNeeded && oldInstallBusy(oldData) {
		// Keep this run on the old paths; migrate once the old daemon
		// and player are gone.
		fmt.Fprintln(os.Stderr, "iar: the previous install is still running; using its data for now (it will be migrated on a later start)")
		out := p
		out.DataDir = oldData
		if dirExists(oldConfig) && !dirExists(p.ConfigDir) {
			out.ConfigDir = oldConfig
		}
		return out
	}
	if dataNeeded {
		if err := os.Rename(oldData, p.DataDir); err != nil {
			fmt.Fprintf(os.Stderr, "iar: could not migrate data from %s: %v (continuing with the old location)\n", oldData, err)
			out := p
			out.DataDir = oldData
			return out
		}
		// The main log file carries the old name; move it along.
		oldLog := filepath.Join(p.DataDir, "logs", oldAppDir+".log")
		if _, err := os.Stat(oldLog); err == nil {
			os.Rename(oldLog, filepath.Join(p.DataDir, "logs", "iar.log"))
		}
		// The engine's Python environment embeds absolute paths (script
		// shebangs, venv metadata); repoint them at the new location so
		// the engine starts without a re-install.
		fixEngineVenv(filepath.Join(p.DataDir, "engine"), oldData, p.DataDir)
		fmt.Fprintf(os.Stderr, "iar: migrated data (engine, library, sessions) from %s to %s\n", oldData, p.DataDir)
	}
	if configNeeded {
		if err := os.Rename(oldConfig, p.ConfigDir); err != nil {
			fmt.Fprintf(os.Stderr, "iar: could not migrate config from %s: %v\n", oldConfig, err)
			out := p
			out.ConfigDir = oldConfig
			return out
		}
		fmt.Fprintf(os.Stderr, "iar: migrated configuration from %s to %s\n", oldConfig, p.ConfigDir)
	}
	return p
}

// oldInstallBusy reports whether the old install still has a live engine
// daemon or a running player.
func oldInstallBusy(oldData string) bool {
	stateDir := filepath.Join(oldData, "state")
	// Live engine daemon?
	if raw, err := os.ReadFile(filepath.Join(stateDir, "engine.json")); err == nil {
		var st struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(raw, &st) == nil && state.PIDAlive(st.PID) {
			return true
		}
	}
	// Running player (holds the lock)?
	if d, err := state.NewDir(oldData); err == nil {
		lock, err := d.AcquireLock("player.lock", false)
		if err != nil {
			return true // held elsewhere (or unreadable): treat as busy
		}
		lock.Release()
	}
	return false
}

// fixEngineVenv rewrites absolute-path references inside the engine's
// virtualenv (entry-point shebangs and venv metadata) after the data
// directory moved. Only small text files in the venv root and bin/ are
// touched.
func fixEngineVenv(engineDir, oldDataDir, newDataDir string) {
	venv := filepath.Join(engineDir, ".venv")
	targets := []string{filepath.Join(venv, "pyvenv.cfg")}
	if entries, err := os.ReadDir(filepath.Join(venv, "bin")); err == nil {
		for _, e := range entries {
			if e.Type().IsRegular() {
				targets = append(targets, filepath.Join(venv, "bin", e.Name()))
			}
		}
	}
	// Editable installs and install metadata under site-packages also
	// embed the absolute project path.
	if libs, err := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages")); err == nil {
		for _, sp := range libs {
			if pths, err := filepath.Glob(filepath.Join(sp, "*.pth")); err == nil {
				targets = append(targets, pths...)
			}
			if urls, err := filepath.Glob(filepath.Join(sp, "*", "direct_url.json")); err == nil {
				targets = append(targets, urls...)
			}
		}
	}
	oldB := []byte(oldDataDir)
	newB := []byte(newDataDir)
	fixed := 0
	for _, path := range targets {
		fi, err := os.Stat(path)
		if err != nil || fi.Size() > 1<<20 {
			continue // scripts and metadata are small; skip anything else
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(data, oldB) {
			continue
		}
		if err := os.WriteFile(path, bytes.ReplaceAll(data, oldB, newB), fi.Mode()); err == nil {
			fixed++
		}
	}
	if fixed > 0 {
		fmt.Fprintf(os.Stderr, "iar: repointed %d engine environment file(s) to the new location\n", fixed)
	}
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
