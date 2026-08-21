// Package export renders a session's steering context to an MP3 file:
// tracks are generated (or noise synthesized), crossfade-joined into one
// PCM stream and piped once through ffmpeg's libmp3lame encoder.
package export

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"bgm/internal/audio"
	"bgm/internal/engine"
	"bgm/internal/prompting"
	"bgm/internal/session"
)

// MaxMinutes caps a single export.
const MaxMinutes = 180

// Renderer renders exports. Engine may be nil for noise-mode sessions.
type Renderer struct {
	// Engine generates music tracks (unused for noise sessions).
	Engine engine.Engine
	// Builder produces generation specs from the session context.
	Builder *prompting.Builder
	// TrackSeconds is the per-generation duration.
	TrackSeconds int
	// CrossfadeSeconds is the overlap between joined tracks.
	CrossfadeSeconds float64
	// Gate, when set, is awaited before each generation so a live stream
	// keeps priority over the export.
	Gate func(ctx context.Context) error
	// Progress receives human-readable progress lines (may be nil).
	Progress func(string)
	// Log receives structured events (nil uses the default logger).
	Log *slog.Logger
}

// Render produces minutes of audio matching sess into an MP3 at outPath.
func (r *Renderer) Render(ctx context.Context, sess *session.Session, minutes int, outPath string) error {
	if minutes < 1 {
		return fmt.Errorf("export needs at least 1 minute")
	}
	if minutes > MaxMinutes {
		return fmt.Errorf("export capped at %d minutes; ask for %d or fewer", MaxMinutes, MaxMinutes)
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	progress := r.Progress
	if progress == nil {
		progress = func(string) {}
	}

	totalFrames := minutes * 60 * audio.SampleRate
	var samples []int16
	var err error
	if sess.Mode == session.ModeNoise {
		progress(fmt.Sprintf("synthesizing %d minutes of %s noise", minutes, sess.NoiseColor))
		samples = r.renderNoise(sess, totalFrames)
	} else {
		samples, err = r.renderMusic(ctx, sess, totalFrames, progress, log)
		if err != nil {
			return err
		}
	}
	if len(samples) > totalFrames*audio.Channels {
		samples = samples[:totalFrames*audio.Channels]
	}
	audio.ApplyEdgeFades(samples, audio.SampleRate/2)

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	progress("encoding MP3")
	if err := EncodeMP3(ctx, samples, outPath); err != nil {
		return err
	}
	log.Info("export finished", "event", "export_done", "path", outPath, "minutes", minutes)
	progress("export complete: " + outPath)
	return nil
}

// renderNoise synthesizes the requested noise directly.
func (r *Renderer) renderNoise(sess *session.Session, totalFrames int) []int16 {
	gen := audio.NewNoiseGenerator(audio.ParseNoiseColor(sess.NoiseColor), 0.3)
	return gen.Generate(totalFrames)
}

// renderMusic generates enough tracks and crossfade-joins them.
func (r *Renderer) renderMusic(ctx context.Context, sess *session.Session, totalFrames int, progress func(string), log *slog.Logger) ([]int16, error) {
	if r.Engine == nil {
		return nil, fmt.Errorf("music engine unavailable; only noise sessions can be exported right now")
	}
	fadeFrames := int(r.CrossfadeSeconds * audio.SampleRate)
	trackFrames := r.TrackSeconds * audio.SampleRate
	if trackFrames <= fadeFrames {
		return nil, fmt.Errorf("track length must exceed the crossfade")
	}
	// Each extra track adds (track - fade) frames.
	n := 1 + (totalFrames-trackFrames+trackFrames-fadeFrames-1)/(trackFrames-fadeFrames)
	if n < 1 {
		n = 1
	}
	var tracks [][]int16
	for i := 0; i < n; i++ {
		if r.Gate != nil {
			if err := r.Gate(ctx); err != nil {
				return nil, err
			}
		}
		progress(fmt.Sprintf("generating track %d of %d", i+1, n))
		spec := r.Builder.BuildSpec(ctx, sess, r.TrackSeconds)
		start := time.Now()
		track, err := r.Engine.Generate(ctx, spec)
		if err != nil {
			return nil, fmt.Errorf("generating track %d/%d: %w", i+1, n, err)
		}
		log.Info("export track generated", "event", "export_track",
			"index", i+1, "total", n, "elapsed_seconds", time.Since(start).Seconds())
		tracks = append(tracks, track.Samples)
	}
	return audio.CrossfadeJoin(tracks, fadeFrames), nil
}

// EncodeMP3 pipes PCM once through ffmpeg/libmp3lame.
func EncodeMP3(ctx context.Context, samples []int16, outPath string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "s16le", "-ar", fmt.Sprint(audio.SampleRate), "-ac", fmt.Sprint(audio.Channels),
		"-i", "-",
		"-codec:a", "libmp3lame", "-q:a", "2",
		outPath,
	)
	cmd.Stdin = bytes.NewReader(audio.SamplesToBytes(samples))
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg mp3 encode: %w: %s", err, bytes.TrimSpace(errBuf.Bytes()))
	}
	return nil
}

// DefaultPath builds the default export file name inside dir.
func DefaultPath(dir, sessionName string, minutes int) string {
	stamp := time.Now().Format("20060102-150405")
	return filepath.Join(dir, fmt.Sprintf("%s-%dmin-%s.mp3", sessionName, minutes, stamp))
}
