package trackbuffer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"testing"

	"iar/internal/audio"
	"iar/internal/engine"
)

// A stored song's hash is the hash of the file exactly as it will be
// served, and the file can be found by name for serving as it is.
func TestAStoredSongKnowsItsOwnHash(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	s := New(t.TempDir(), 9, nil)
	track := &engine.Track{ID: "t-1790000000000-0001", Prompt: "a song",
		Samples: make([]int16, audio.SampleRate*audio.Channels/2)}
	hash, err := s.PutTrack(context.Background(), 2, 5, track)
	if err != nil {
		t.Fatal(err)
	}
	path, ok := s.TrackPath(2, "e00000002-00000005")
	if !ok {
		t.Fatal("the stored song has no file to serve")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if want := hex.EncodeToString(sum[:]); hash != want {
		t.Fatalf("PutTrack reported hash %s; the file hashes to %s", hash, want)
	}
	entries := s.List(2)
	if len(entries) != 1 || entries[0].Hash != hash {
		t.Fatalf("listed entries = %+v, want the hash %s", entries, hash)
	}

	// Another epoch's name, or a name that is not a song, is refused.
	if _, ok := s.TrackPath(3, "e00000002-00000005"); ok {
		t.Fatal("a song was served under another epoch")
	}
	if _, ok := s.TrackPath(2, "../../etc/passwd"); ok {
		t.Fatal("a path that is not a song name was served")
	}
	// A taken song is still there to serve; a dropped one is not.
	if !s.Take(2, "e00000002-00000005") {
		t.Fatal("the song could not be taken")
	}
	if _, ok := s.TrackPath(2, "e00000002-00000005"); !ok {
		t.Fatal("a taken song has no file to serve")
	}
	s.DropTrack(2, "e00000002-00000005")
	if _, ok := s.TrackPath(2, "e00000002-00000005"); ok {
		t.Fatal("a dropped song still has a file to serve")
	}
}
