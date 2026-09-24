package songbook

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func song(id, hash string) Song {
	return Song{ID: id, Hash: hash, Title: "Title " + id, Prompt: "prompt", Lyrics: "[Verse]\nwords", Created: time.Now()}
}

// The whole point: a song is found by its hash, and everything the book
// knows about it comes back after a restart.
func TestABookRemembersAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "songbook.jsonl")
	b := Open(path, nil)
	if err := b.Record(song("t-1", "aaa")); err != nil {
		t.Fatal(err)
	}
	if err := b.Record(song("t-2", "bbb")); err != nil {
		t.Fatal(err)
	}
	b.Retitle("t-1", "Harbour Lights")
	b.MarkSaved("t-2", "/snips/x.mp3")

	again := Open(path, nil)
	if again.Len() != 2 {
		t.Fatalf("reopened book holds %d songs, want 2", again.Len())
	}
	s, ok := again.ByHash("aaa")
	if !ok || s.ID != "t-1" || s.Title != "Harbour Lights" || s.Lyrics == "" {
		t.Fatalf("t-1 by hash = %+v (ok=%v)", s, ok)
	}
	if got := again.SavedPath("t-2"); got != "/snips/x.mp3" {
		t.Fatalf("t-2 saved path = %q", got)
	}
	if got := again.RecentlySaved(10); len(got) != 1 || got[0] != "t-2" {
		t.Fatalf("recently saved = %v", got)
	}
}

// A song the book never saw made can still be marked saved, and a
// re-record of a saved song does not forget the save.
func TestSavesSurviveUnknownSongsAndReRecords(t *testing.T) {
	b := Open("", nil)
	b.MarkSaved("lib:key/file", "/snips/a.mp3")
	if b.SavedPath("lib:key/file") == "" {
		t.Fatal("a save of an unknown song was not remembered")
	}
	if err := b.Record(song("t-9", "ccc")); err != nil {
		t.Fatal(err)
	}
	b.MarkSaved("t-9", "/snips/b.mp3")
	if err := b.Record(song("t-9", "ccc")); err != nil {
		t.Fatal(err)
	}
	if b.SavedPath("t-9") != "/snips/b.mp3" {
		t.Fatal("re-recording a song forgot that it was saved")
	}
	if got := b.RecentlySaved(1); len(got) != 1 || got[0] != "t-9" {
		t.Fatalf("recently saved = %v", got)
	}
}

// Every change is one more line; a reopen with more dead lines than
// live ones squeezes the file back to one line per song.
func TestTheFileIsCompactedWhenStaleLinesOutnumberLiveOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "songbook.jsonl")
	b := Open(path, nil)
	if err := b.Record(song("t-1", "aaa")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		b.Retitle("t-1", "Name "+strings.Repeat("x", i+1))
	}
	if n := lines(t, path); n != 6 {
		t.Fatalf("file has %d lines before compaction, want 6", n)
	}
	again := Open(path, nil)
	if n := lines(t, path); n != 1 {
		t.Fatalf("file has %d lines after compaction, want 1", n)
	}
	if s, _ := again.ByID("t-1"); s.Title != "Name xxxxx" {
		t.Fatalf("compaction kept the wrong line: %+v", s)
	}
}

// A crash mid-write leaves a torn last line; the book reads past it.
func TestATornLineIsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "songbook.jsonl")
	b := Open(path, nil)
	if err := b.Record(song("t-1", "aaa")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"id":"t-2","hash":"bb`)
	f.Close()
	again := Open(path, nil)
	if again.Len() != 1 {
		t.Fatalf("book holds %d songs, want 1", again.Len())
	}
	if _, ok := again.ByID("t-1"); !ok {
		t.Fatal("the whole song before the torn line was lost")
	}
}

// Past the cap the oldest songs are forgotten, hash and all.
func TestTheOldestSongsAreForgottenPastTheCap(t *testing.T) {
	b := Open("", nil)
	for i := 0; i < MaxSongs+3; i++ {
		id := "t-" + strings.Repeat("0", 5) + string(rune('a'+i%26)) + itoa(i)
		if err := b.Record(song(id, "h"+itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	if b.Len() != MaxSongs {
		t.Fatalf("book holds %d, want %d", b.Len(), MaxSongs)
	}
	if _, ok := b.ByHash("h0"); ok {
		t.Fatal("the oldest song is still found by hash")
	}
	if _, ok := b.ByHash("h" + itoa(MaxSongs+2)); !ok {
		t.Fatal("the newest song is not found by hash")
	}
}

func lines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}
