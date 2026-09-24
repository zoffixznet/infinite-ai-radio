package export

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"iar/internal/audio"
)

// tags returns the format tags ffprobe reads from a file.
func tags(t *testing.T, path string) map[string]string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format_tags", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var data struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		t.Fatal(err)
	}
	return data.Format.Tags
}

// A copy carries new tags and the very same music: what comes out of
// the decoder is sample for sample what the source gives, which a
// second encode would never manage.
func TestCopyMP3KeepsTheFramesAndChangesTheTags(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	samples := make([]int16, 2*audio.SampleRate*audio.Channels)
	for i := 0; i < len(samples); i += 2 {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i/2)/audio.SampleRate))
		samples[i], samples[i+1] = v, v
	}
	src := filepath.Join(dir, "src.mp3")
	ctx := context.Background()
	if err := EncodeMP3(ctx, samples, src, MP3Options{Quality: 5, Title: "As Rendered", Comment: "buffered track"}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "saved", "copy.mp3")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CopyMP3(ctx, src, dst, MP3Options{Title: "As Saved", Album: "road", Subtitle: "a mood", Comment: "the words"}); err != nil {
		t.Fatal(err)
	}

	got := tags(t, dst)
	if got["title"] != "As Saved" || got["album"] != "road" || got["comment"] != "the words" || got["TIT3"] != "a mood" {
		t.Fatalf("copy tags = %v", got)
	}
	if bytes.Contains([]byte(got["comment"]), []byte("buffered")) {
		t.Fatalf("the source's tags leaked into the copy: %v", got)
	}

	want, err := DecodePCM(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	have, err := DecodePCM(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(audio.SamplesToBytes(want), audio.SamplesToBytes(have)) {
		t.Fatalf("the copy decodes differently from the source (%d vs %d samples): it was encoded again",
			len(have), len(want))
	}
}
