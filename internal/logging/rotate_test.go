package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

func TestRotatingFileRotatesAtTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iar.log")
	r, err := OpenRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	line := strings.Repeat("a", 39) + "\n" // 40 bytes
	for i := 0; i < 2; i++ {
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("rotated before reaching the cap")
	}
	// The third line would carry the file to 120 bytes: it starts a new one.
	third := strings.Repeat("c", 39) + "\n"
	if _, err := r.Write([]byte(third)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path+".1"); got != line+line {
		t.Fatalf(".1 = %q, want the two lines before the cap", got)
	}
	if got := readFile(t, path); got != third {
		t.Fatalf("fresh log = %q, want only the line past the cap", got)
	}
}

func TestRotatingFileKeepsTwoOldCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iar.log")
	r, err := OpenRotating(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, w := range []string{"one......\n", "two......\n", "three....\n", "four.....\n"} {
		if _, err := r.Write([]byte(w)); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{
		path:        "four.....\n",
		path + ".1": "three....\n",
		path + ".2": "two......\n",
	}
	for p, w := range want {
		if got := readFile(t, p); got != w {
			t.Errorf("%s = %q, want %q", filepath.Base(p), got, w)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("a third old copy was kept")
	}
	matches, _ := filepath.Glob(path + "*")
	if len(matches) != 3 {
		t.Errorf("files = %v, want the log and two old copies", matches)
	}
}

func TestRotatingFileOverTheCapAtOpenIsRotated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine-daemon.log")
	old := strings.Repeat("x", 200)
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := readFile(t, path+".1"); got != old {
		t.Fatalf(".1 should hold the oversized log, got %d bytes", len(got))
	}
	if n := size(t, path); n != 0 {
		t.Fatalf("fresh log is %d bytes, want empty", n)
	}
	if _, err := r.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "hello\n" {
		t.Fatalf("log = %q", got)
	}
}

func TestRotatingFileUnderTheCapAtOpenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iar.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Write([]byte("after\n"))
	if got := readFile(t, path); got != "before\nafter\n" {
		t.Fatalf("log = %q", got)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("a log under its cap was rotated")
	}
	// The counter starts from the existing size: 7+6 bytes are in, so
	// a 90-byte line crosses 100.
	r.Write([]byte(strings.Repeat("z", 89) + "\n"))
	if got := readFile(t, path+".1"); got != "before\nafter\n" {
		t.Fatalf(".1 = %q, the existing bytes were not counted", got)
	}
}

func TestRotatingFileConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iar.log")
	r, err := OpenRotating(path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	const writers, lines = 8, 200
	line := []byte(strings.Repeat("c", 31) + "\n")
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < lines; i++ {
				if _, err := r.Write(line); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Every file holds whole lines and stays within the cap; the
	// newest three hold the last of what was written.
	var total int
	for _, p := range []string{path, path + ".1", path + ".2"} {
		data := []byte(readFile(t, p))
		if len(data) > 4096 {
			t.Errorf("%s is %d bytes, over the cap", filepath.Base(p), len(data))
		}
		if len(data)%len(line) != 0 || bytes.Count(data, line) != len(data)/len(line) {
			t.Errorf("%s holds torn lines", filepath.Base(p))
		}
		total += len(data)
	}
	if total == 0 {
		t.Fatal("nothing was written")
	}
	if _, err := r.Write(line); err == nil {
		t.Error("a write after Close succeeded")
	}
}

func TestRotatingFileOnRotateSeesTheFreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine-daemon.log")
	r, err := OpenRotating(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var got []string
	r.OnRotate = func(f *os.File) { got = append(got, f.Name()) }
	r.Write([]byte("0123456789"))
	if len(got) != 0 {
		t.Fatal("OnRotate called before any rotation")
	}
	r.Write([]byte("next\n"))
	if len(got) != 1 || got[0] != path {
		t.Fatalf("OnRotate calls = %v, want one with the fresh file", got)
	}
}

// Another iar command appending to the same log may rotate it first;
// the rotator then picks up the fresh file instead of pushing it aside.
func TestRotatingFileAdoptsAnotherProcessRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iar.log")
	a, err := OpenRotating(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenRotating(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	a.Write([]byte(strings.Repeat("a", 29) + "\n"))
	b.Write([]byte(strings.Repeat("b", 29) + "\n")) // b's counter: 30
	a.Write([]byte(strings.Repeat("A", 29) + "\n")) // a's counter 60 > 50: a rotates
	b.Write([]byte(strings.Repeat("B", 29) + "\n")) // b's counter 60 > 50: adopts
	if got := readFile(t, path); got != strings.Repeat("A", 29)+"\n"+strings.Repeat("B", 29)+"\n" {
		t.Fatalf("log = %q, want both writers in the fresh file", got)
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Fatal("the second writer rotated the fresh file away")
	}
}

func TestOpenRotatingRejectsAZeroCap(t *testing.T) {
	if _, err := OpenRotating(filepath.Join(t.TempDir(), "x.log"), 0); err == nil {
		t.Fatal("a zero cap was accepted")
	}
}
