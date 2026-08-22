package accounts

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// fileStamp identifies a file's on-disk version cheaply.
type fileStamp struct {
	mod  time.Time
	size int64
	seen bool
}

// changed reports whether the file differs from the stamped version (a
// missing file counts as a version too).
func (st fileStamp) changed(path string) bool {
	if !st.seen {
		return true
	}
	fi, err := os.Stat(path)
	if err != nil {
		return st.size != 0 || !st.mod.IsZero()
	}
	return !fi.ModTime().Equal(st.mod) || fi.Size() != st.size
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{seen: true}
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size(), seen: true}
}

// readJSON decodes the file into v. A missing file leaves v untouched
// and is not an error.
func readJSON(path string, v any) (fileStamp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fileStamp{seen: true}, nil
		}
		return fileStamp{}, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fileStamp{}, err
	}
	return stampOf(path), nil
}

// writeJSON writes v atomically (temp file + rename) with owner-only
// permissions, creating the parent directory as needed.
func writeJSON(path string, v any) (fileStamp, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fileStamp{}, err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fileStamp{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fileStamp{}, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fileStamp{}, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fileStamp{}, err
	}
	if err := tmp.Close(); err != nil {
		return fileStamp{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fileStamp{}, err
	}
	return stampOf(path), nil
}
