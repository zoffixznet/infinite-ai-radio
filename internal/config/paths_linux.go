package config

import (
	"os"
	"path/filepath"
)

// userDataHome returns the base directory for user data files, following the
// XDG base directory specification.
func userDataHome() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}
