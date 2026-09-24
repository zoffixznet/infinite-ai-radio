package player

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
