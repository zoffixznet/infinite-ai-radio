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
	if err := r.Render(context.Background(), sess, 1, out); err != nil {
		t.Fatal(err)
	}
	codec, dur := probe(t, out)
	if codec != "mp3" {
		t.Fatalf("codec = %s", codec)
	}
	if dur < 59 || dur > 61 {
		t.Fatalf("duration = %.1fs; want ~60s", dur)
	}
}

func TestRenderNoiseToMP3(t *testing.T) {
	requireTools(t)
	out := filepath.Join(t.TempDir(), "noise.mp3")
	r := &Renderer{}
	sess := session.New()
	sess.Mode = session.ModeNoise
	sess.NoiseColor = "pink"
	if err := r.Render(context.Background(), sess, 1, out); err != nil {
		t.Fatal(err)
	}
	codec, dur := probe(t, out)
	if codec != "mp3" || dur < 59 || dur > 61 {
		t.Fatalf("codec %s, duration %.1f", codec, dur)
	}
}

func TestRenderCapsMinutes(t *testing.T) {
	r := &Renderer{}
	err := r.Render(context.Background(), session.New(), MaxMinutes+1, filepath.Join(t.TempDir(), "x.mp3"))
	if err == nil || !strings.Contains(err.Error(), "capped") {
		t.Fatalf("cap not enforced: %v", err)
	}
}

func TestDefaultPathShape(t *testing.T) {
	p := DefaultPath("/tmp/exports", "my-session", 20)
	if !strings.HasPrefix(p, "/tmp/exports/my-session-20min-") || !strings.HasSuffix(p, ".mp3") {
		t.Fatalf("path = %s", p)
	}
}
