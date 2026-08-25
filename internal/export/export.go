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

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/prompting"
	"iar/internal/session"
)

// MaxMinutes caps a single export.
const MaxMinutes = 180

// MP3Options tunes the MP3 encode.
type MP3Options struct {
	// Quality is libmp3lame VBR quality: 0 best to 9 smallest.
	Quality int
	// Title, Artist, Album, Subtitle and Comment become ID3 tags when
	// non-empty (Subtitle lands in the TIT3 frame).
	Title    string
	Artist   string
	Album    string
	Subtitle string
	Comment  string
}

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
	// MP3Quality is the libmp3lame VBR quality (0 best .. 9 smallest).
	MP3Quality int
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
	if err := EncodeMP3(ctx, samples, outPath, MP3Options{
		Quality: r.MP3Quality,
		Title:   sess.Describe(),
		Artist:  "Infinite AI Radio",
	}); err != nil {
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

// EncodeMP3 pipes PCM once through ffmpeg/libmp3lame. The file is
// written under a temporary name and renamed into place when complete,
// so anything listing the directory never sees a half-written MP3.
func EncodeMP3(ctx context.Context, samples []int16, outPath string, opts MP3Options) error {
	if opts.Quality < 0 || opts.Quality > 9 {
		opts.Quality = 0
	}
	tmpPath := filepath.Join(filepath.Dir(outPath), "."+filepath.Base(outPath)+".part")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "s16le", "-ar", fmt.Sprint(audio.SampleRate), "-ac", fmt.Sprint(audio.Channels),
		"-i", "-",
		"-f", "mp3", "-codec:a", "libmp3lame", "-q:a", fmt.Sprint(opts.Quality),
	}
	for tag, v := range map[string]string{
		"title":   opts.Title,
		"artist":  opts.Artist,
		"album":   opts.Album,
		"TIT3":    opts.Subtitle,
		"comment": opts.Comment,
	} {
		if v != "" {
			args = append(args, "-metadata", tag+"="+v)
		}
	}
	args = append(args, "-id3v2_version", "3", tmpPath)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin = bytes.NewReader(audio.SamplesToBytes(samples))
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("ffmpeg mp3 encode: %w: %s", err, bytes.TrimSpace(errBuf.Bytes()))
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// EncodeMP3Bytes encodes PCM to an in-memory MP3 (used to serve tracks
// over HTTP without touching the disk or the playback path).
func EncodeMP3Bytes(ctx context.Context, samples []int16, opts MP3Options) ([]byte, error) {
	if opts.Quality < 0 || opts.Quality > 9 {
		opts.Quality = 0
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", fmt.Sprint(audio.SampleRate), "-ac", fmt.Sprint(audio.Channels),
		"-i", "-",
		"-f", "mp3", "-codec:a", "libmp3lame", "-q:a", fmt.Sprint(opts.Quality),
	}
	for tag, v := range map[string]string{
		"title":   opts.Title,
		"artist":  opts.Artist,
		"album":   opts.Album,
		"TIT3":    opts.Subtitle,
		"comment": opts.Comment,
	} {
		if v != "" {
			args = append(args, "-metadata", tag+"="+v)
		}
	}
	args = append(args, "-id3v2_version", "3", "-")
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin = bytes.NewReader(audio.SamplesToBytes(samples))
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg mp3 encode: %w: %s", err, bytes.TrimSpace(errBuf.Bytes()))
	}
	return out.Bytes(), nil
}

// DefaultPath builds the default export file name inside dir.
func DefaultPath(dir, sessionName string, minutes int) string {
	stamp := time.Now().Format("20060102-150405")
	return filepath.Join(dir, fmt.Sprintf("%s-%dmin-%s.mp3", sessionName, minutes, stamp))
}
