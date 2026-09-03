package export

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
)

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

// probe returns codec name and duration in seconds via ffprobe.
func probe(t *testing.T, path string) (string, float64) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "stream=codec_name:format=duration",
		"-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var data struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Streams) == 0 {
		t.Fatal("no streams in probe output")
	}
	dur, _ := strconv.ParseFloat(data.Format.Duration, 64)
	return data.Streams[0].CodecName, dur
}

func TestRenderMusicToMP3(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp3")
	r := &Renderer{
		Engine:           enginetest.NewMock(),
		Builder:          prompting.NewBuilder(nil, nil),
		TrackSeconds:     30,
		CrossfadeSeconds: 2,
	}
	sess := session.New()
	if err := r.Render(context.Background(), sess, Request{Minutes: 1, OutPath: out}); err != nil {
		t.Fatal(err)
	}
	codec, dur := probe(t, out)
	if codec != "mp3" {
		t.Fatalf("codec = %s", codec)
	}
	// Whole songs, never cut: 30s tracks joined over 2s fades reach the
	// 60s ask at the third song (30 + 28 + 28 = 86s), and the file
	// carries all of it rather than being trimmed to exactly 60.
	if dur < 84 || dur > 88 {
		t.Fatalf("duration = %.1fs; want ~86s (whole songs, no trim)", dur)
	}
}

func TestRenderExactSongCount(t *testing.T) {
	requireTools(t)
	out := filepath.Join(t.TempDir(), "two.mp3")
	r := &Renderer{
		Engine:           enginetest.NewMock(),
		Builder:          prompting.NewBuilder(nil, nil),
		TrackSeconds:     30,
		CrossfadeSeconds: 2,
	}
	if err := r.Render(context.Background(), session.New(), Request{Songs: 2, OutPath: out}); err != nil {
		t.Fatal(err)
	}
	if _, dur := probe(t, out); dur < 56 || dur > 60 {
		t.Fatalf("duration = %.1fs; want ~58s for two whole songs", dur)
	}
}

func TestRenderRequestValidation(t *testing.T) {
	r := &Renderer{}
	out := filepath.Join(t.TempDir(), "x.mp3")
	if err := r.Render(context.Background(), session.New(), Request{Minutes: 1, Songs: 1, OutPath: out}); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("minutes+songs err = %v", err)
	}
	if err := r.Render(context.Background(), session.New(), Request{Songs: MaxSongs + 1, OutPath: out}); err == nil || !strings.Contains(err.Error(), "capped") {
		t.Fatalf("songs cap err = %v", err)
	}
	noise := session.New()
	noise.Mode = session.ModeNoise
	noise.NoiseColor = "pink"
	if err := r.Render(context.Background(), noise, Request{Songs: 1, OutPath: out}); err == nil || !strings.Contains(err.Error(), "no songs") {
		t.Fatalf("noise songs err = %v", err)
	}
}

func TestRenderNoiseToMP3(t *testing.T) {
	requireTools(t)
	out := filepath.Join(t.TempDir(), "noise.mp3")
	r := &Renderer{}
	sess := session.New()
	sess.Mode = session.ModeNoise
	sess.NoiseColor = "pink"
	if err := r.Render(context.Background(), sess, Request{Minutes: 1, OutPath: out}); err != nil {
		t.Fatal(err)
	}
	codec, dur := probe(t, out)
	if codec != "mp3" || dur < 59 || dur > 61 {
		t.Fatalf("codec %s, duration %.1f", codec, dur)
	}
}

func TestRenderCapsMinutes(t *testing.T) {
	r := &Renderer{}
	err := r.Render(context.Background(), session.New(), Request{Minutes: MaxMinutes + 1, OutPath: filepath.Join(t.TempDir(), "x.mp3")})
	if err == nil || !strings.Contains(err.Error(), "capped") {
		t.Fatalf("cap not enforced: %v", err)
	}
}

func TestDefaultPathShape(t *testing.T) {
	p := DefaultPath("/tmp/exports", "my-session", 20, 0)
	if !strings.HasPrefix(p, "/tmp/exports/my-session-20min-") || !strings.HasSuffix(p, ".mp3") {
		t.Fatalf("path = %s", p)
	}
	p = DefaultPath("/tmp/exports", "my-session", 0, 3)
	if !strings.HasPrefix(p, "/tmp/exports/my-session-3songs-") {
		t.Fatalf("songs path = %s", p)
	}
}
