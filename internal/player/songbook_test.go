package player

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"iar/internal/audio"
	"iar/internal/export"
	"iar/internal/snippets"
)

// oneSnippet waits for exactly one MP3 under the tag and returns it.
func oneSnippet(t *testing.T, o *Orchestrator, tag string) string {
	t.Helper()
	var found []string
	waitFor(t, 20*time.Second, "the snippet on disk", func() bool {
		found, _ = filepath.Glob(filepath.Join(o.SnippetsDir, tag, "*.mp3"))
		return len(found) == 1
	})
	waitFor(t, 5*time.Second, "the save recorded", func() bool {
		return o.Songbook.RecentlySaved(1) != nil
	})
	return found[0]
}

// A song still on disk is saved as the file itself, retagged: the music
// in the snippet is exactly what the phone was handed, not a second
// encode of it.
func TestSavingASongOnDiskCopiesTheFile(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const id = "t-1790000000000-0021"
	renderSong(t, o, 1, id)
	src, ok := o.TrackFile(id)
	if !ok {
		t.Fatal("the rendered song has no file to serve")
	}
	if ack := o.SaveSnippet(id, "road"); !strings.HasPrefix(ack, "saving this track to road/") {
		t.Fatalf("save ack = %q", ack)
	}
	saved := oneSnippet(t, o, "road")

	ctx := context.Background()
	want, err := export.DecodePCM(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	have, err := export.DecodePCM(ctx, saved)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(audio.SamplesToBytes(want), audio.SamplesToBytes(have)) {
		t.Fatal("the saved copy decodes differently from the stored file: it was encoded again")
	}
	if _, err := os.Stat(snippets.LyricsSidecar(saved)); err != nil {
		t.Fatal("the lyric sheet was not written beside the copy")
	}
	if got := o.Songbook.SavedPath(id); got != saved {
		t.Fatalf("the book says the song was saved to %q, the file is %q", got, saved)
	}
	if ack := o.SaveSnippet(id, "road"); !strings.HasPrefix(ack, "already saved") {
		t.Fatalf("repeat save ack = %q", ack)
	}
}

// The song is gone from the radio - fed, played, steered away, whatever
// - but the phone still has it. The radio asks for that copy, checks it
// is one of its own, and saves it under what it recorded at render.
func TestSavingFromACopyTheDeviceSendsBack(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const id = "t-1790000000000-0022"
	hash := renderSong(t, o, 1, id)
	src, _ := o.TrackFile(id)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	o.Buffer.DropAll()

	if ack := o.SaveSnippet(id, "road"); ack != AckSendCopy {
		t.Fatalf("save of a gone song = %q, want a request for the copy", ack)
	}
	// Not music of the radio's: refused, nothing written.
	if ack := o.SaveUpload("", "road", bytes.NewReader([]byte("not music"))); !strings.Contains(ack, "not a song this radio made") {
		t.Fatalf("stray upload ack = %q", ack)
	}
	// Claiming a hash the radio never made is refused before the bytes are read.
	if ack := o.SaveUpload(strings.Repeat("0", 64), "road", bytes.NewReader(data)); !strings.Contains(ack, "not a song this radio made") {
		t.Fatalf("unknown-hash upload ack = %q", ack)
	}
	if m, _ := filepath.Glob(filepath.Join(o.SnippetsDir, "road", "*")); len(m) != 0 {
		t.Fatalf("a refused upload left files behind: %v", m)
	}
	if m, _ := filepath.Glob(filepath.Join(o.SnippetsDir, ".upload-*")); len(m) != 0 {
		t.Fatalf("a refused upload left its temporary file behind: %v", m)
	}

	ack := o.SaveUpload(hash, "road", bytes.NewReader(data))
	if !strings.HasPrefix(ack, "track saved: road/") {
		t.Fatalf("upload ack = %q", ack)
	}
	saved := oneSnippet(t, o, "road")
	if _, err := os.Stat(snippets.LyricsSidecar(saved)); err != nil {
		t.Fatal("the lyric sheet was not written beside the uploaded copy")
	}
	if got := o.Songbook.SavedPath(id); got != saved {
		t.Fatalf("the book says the song was saved to %q, the file is %q", got, saved)
	}
	// Again, hash unstated: recognised from the bytes alone, and already saved.
	if ack := o.SaveUpload("", "road", bytes.NewReader(data)); !strings.HasPrefix(ack, "already saved") {
		t.Fatalf("repeat upload ack = %q", ack)
	}
	if m, _ := filepath.Glob(filepath.Join(o.SnippetsDir, "road", "*.mp3")); len(m) != 1 {
		t.Fatalf("a repeat upload wrote a second file: %v", m)
	}
}

// A listener renames a song; the copy that comes back to be saved later
// is saved under the new name, though the file on the phone knows
// nothing of it.
func TestARenamedSongIsSavedFromItsCopyUnderTheNewName(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const id = "t-1790000000000-0023"
	hash := renderSong(t, o, 1, id)
	src, _ := o.TrackFile(id)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if ack := o.Retitle(id, "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
		t.Fatalf("rename ack = %q", ack)
	}
	if s, _ := o.Songbook.ByID(id); s.Title != "Harbour Lights" {
		t.Fatalf("the book still calls the song %q", s.Title)
	}
	o.Buffer.DropAll()
	if ack := o.SaveUpload(hash, "", bytes.NewReader(data)); !strings.HasPrefix(ack, "track saved: ") {
		t.Fatalf("upload ack = %q", ack)
	}
	saved := o.Songbook.SavedPath(id)
	if !strings.Contains(strings.ToLower(filepath.Base(saved)), "harbour") {
		t.Fatalf("the copy was saved as %q, not under its new name", filepath.Base(saved))
	}
}

// allSnippets waits until n MP3s lie under the tag and every one of
// the ids is recorded as saved, and returns the files sorted by name.
func allSnippets(t *testing.T, o *Orchestrator, tag string, ids ...string) []string {
	t.Helper()
	var found []string
	waitFor(t, 30*time.Second, "every snippet on disk", func() bool {
		found, _ = filepath.Glob(filepath.Join(o.SnippetsDir, tag, "*.mp3"))
		return len(found) == len(ids)
	})
	waitFor(t, 5*time.Second, "every save recorded", func() bool {
		for _, id := range ids {
			if o.Songbook.SavedPath(id) == "" {
				return false
			}
		}
		return true
	})
	sort.Strings(found)
	return found
}

// A phone back on the network after a tunnel delivers the saves it
// kept there in a burst: the second arrives while the first is still
// being written. Both are kept, in the order they were asked for, and
// asking again for a song already on its way is the same success
// rather than a second file.
func TestSavesArrivingTogetherWaitTheirTurn(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const a, b = "t-1790000000000-0031", "t-1790000000000-0032"
	renderSong(t, o, 1, a)
	renderSong(t, o, 2, b)

	if ack := o.SaveSnippet(a, "road"); !strings.HasPrefix(ack, "saving this track to road/") {
		t.Fatalf("first save ack = %q", ack)
	}
	ack := o.SaveSnippet(b, "road")
	if !strings.HasPrefix(ack, "saving this track to road/") {
		t.Fatalf("second save ack = %q, want it queued rather than refused", ack)
	}
	if !strings.HasSuffix(ack, ", behind 1 other save") {
		t.Fatalf("second save ack = %q, want it to say what it waits for", ack)
	}
	if ack := o.SaveSnippet(b, "road"); !strings.HasPrefix(ack, "already saving:") {
		t.Fatalf("repeat of a queued save ack = %q", ack)
	}

	files := allSnippets(t, o, "road", a, b)
	if got := o.Songbook.RecentlySaved(2); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("saved in the order %v, want [%s %s]", got, a, b)
	}
	if o.Songbook.SavedPath(a) == o.Songbook.SavedPath(b) {
		t.Fatalf("both songs were saved to %q", o.Songbook.SavedPath(a))
	}
	for _, id := range []string{a, b} {
		if ack := o.SaveSnippet(id, "road"); !strings.HasPrefix(ack, "already saved") {
			t.Fatalf("save of %s after the queue drained = %q", id, ack)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(o.SnippetsDir, "road", "*.mp3")); len(m) != len(files) {
		t.Fatalf("a repeat save wrote another file: %v", m)
	}
}

// A copy sent from a device for a song whose save is already queued
// from the radio's own store is the same save: acknowledged as on its
// way, and never a second file.
func TestAnUploadOfASongAlreadyOnItsWayIsTheSameSave(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const a, b, c = "t-1790000000000-0041", "t-1790000000000-0042", "t-1790000000000-0043"
	renderSong(t, o, 1, a)
	renderSong(t, o, 2, b)
	hash := renderSong(t, o, 3, c)
	src, _ := o.TrackFile(c)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a, b, c} {
		if ack := o.SaveSnippet(id, "road"); !strings.HasPrefix(ack, "saving this track to road/") {
			t.Fatalf("save of %s ack = %q", id, ack)
		}
	}
	// c is two writes away: its copy arriving now is not a second save.
	if ack := o.SaveUpload(hash, "road", bytes.NewReader(data)); !strings.HasPrefix(ack, "already saving:") {
		t.Fatalf("upload of a queued song ack = %q", ack)
	}
	allSnippets(t, o, "road", a, b, c)
	if m, _ := filepath.Glob(filepath.Join(o.SnippetsDir, "road", "*.mp3")); len(m) != 3 {
		t.Fatalf("three songs became %d files: %v", len(m), m)
	}
	if m, _ := filepath.Glob(filepath.Join(o.SnippetsDir, ".upload-*")); len(m) != 0 {
		t.Fatalf("the refused upload left its temporary file behind: %v", m)
	}
}

// Two songs with one name, saved within the same second, would land on
// one file name - the second overwriting the first. The later one is
// numbered instead, and the acknowledgment names the file it gets.
func TestTwoSongsOfOneNameSavedInOneSecondKeepBothFiles(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.SnippetsDir = t.TempDir()
	const a, b = "t-1790000000000-0051", "t-1790000000000-0052"
	renderSong(t, o, 1, a)
	renderSong(t, o, 2, b)
	for _, id := range []string{a, b} {
		if ack := o.Retitle(id, "Harbour Lights"); !strings.Contains(ack, "Harbour Lights") {
			t.Fatalf("rename ack = %q", ack)
		}
	}
	// Both saves are asked for within one tick of the clock: the test
	// waits for the start of a second rather than hoping.
	for time.Now().Nanosecond() > 500_000_000 {
		time.Sleep(10 * time.Millisecond)
	}
	ackA := o.SaveSnippet(a, "")
	ackB := o.SaveSnippet(b, "")
	if !strings.HasPrefix(ackA, "saving this track to untagged/") || !strings.HasPrefix(ackB, "saving this track to untagged/") {
		t.Fatalf("acks = %q, %q", ackA, ackB)
	}
	files := allSnippets(t, o, "untagged", a, b)
	if filepath.Base(files[0]) == filepath.Base(files[1]) {
		t.Fatalf("both songs are one file: %v", files)
	}
	for _, f := range files {
		if !strings.Contains(filepath.Base(f), "harbour-lights") {
			t.Fatalf("saved as %q, not under the song's name", filepath.Base(f))
		}
	}
	numbered := filepath.Base(o.Songbook.SavedPath(b))
	if !strings.HasSuffix(numbered, "-2.mp3") {
		t.Fatalf("the second song was saved as %q, want it numbered", numbered)
	}
	if !strings.Contains(ackB, "untagged/"+numbered) {
		t.Fatalf("the second save was promised %q and written to %q", ackB, numbered)
	}
}
