package player

import (
	"context"
	"fmt"
	"time"

	"bgm/internal/engine"
	"bgm/internal/export"
	"bgm/internal/session"
)

// Export renders minutes of the current session's sound to an MP3 without
// disrupting playback: it runs in the background and only generates while
// the playback queue is healthy. The returned string acknowledges the
// request; completion is reported through Events.
func (o *Orchestrator) Export(minutes int, outPath, exportsDir string) string {
	if minutes < 1 || minutes > export.MaxMinutes {
		return fmt.Sprintf("export length must be between 1 and %d minutes", export.MaxMinutes)
	}
	o.mu.Lock()
	if o.exporting != "" {
		o.mu.Unlock()
		return "an export is already running (" + o.exporting + ")"
	}
	if o.sess.Mode == session.ModeMusic && (o.eng == nil || !o.eng.Ready()) {
		o.mu.Unlock()
		return "the music engine is not ready yet; try again shortly"
	}
	sess := *o.sess
	sess.Tweaks = append([]session.Entry(nil), o.sess.Tweaks...)
	o.exporting = fmt.Sprintf("%d min", minutes)
	o.mu.Unlock()

	if outPath == "" {
		outPath = export.DefaultPath(exportsDir, sess.Name, minutes)
	}
	ctx := o.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		r := &export.Renderer{
			Engine:           o.exportEngine(),
			Builder:          o.builder,
			TrackSeconds:     o.cfg.TrackSeconds,
			CrossfadeSeconds: o.cfg.CrossfadeSeconds,
			Gate:             o.exportGate,
			Log:              o.log,
			Progress: func(line string) {
				o.log.Info("export progress", "event", "export_progress", "line", line)
			},
		}
		err := r.Render(ctx, &sess, minutes, outPath)
		o.mu.Lock()
		o.exporting = ""
		o.mu.Unlock()
		if err != nil {
			o.log.Error("export failed", "event", "export_failed", "error", err.Error())
			o.emit("export failed: " + err.Error())
			return
		}
		o.emit("export ready: " + outPath)
	}()
	return fmt.Sprintf("exporting %d minutes in the background to %s", minutes, outPath)
}

// exportEngine returns the engine wrapped so export generations share the
// same serialization as the playback worker.
func (o *Orchestrator) exportEngine() engine.Engine {
	if o.eng == nil {
		return nil
	}
	return &gatedEngine{o: o}
}

// gatedEngine serializes Generate calls with the playback worker.
type gatedEngine struct{ o *Orchestrator }

func (g *gatedEngine) Name() string { return g.o.eng.Name() }
func (g *gatedEngine) Ready() bool  { return g.o.eng.Ready() }

func (g *gatedEngine) Generate(ctx context.Context, spec engine.Spec) (*engine.Track, error) {
	g.o.genMu.Lock()
	defer g.o.genMu.Unlock()
	return g.o.eng.Generate(ctx, spec)
}

// exportGate blocks until the playback queue is full enough that an export
// generation cannot cause an underrun.
func (o *Orchestrator) exportGate(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		o.mu.Lock()
		healthy := o.sess.Mode == session.ModeNoise ||
			len(o.queue) >= o.cfg.BufferTracks ||
			o.eng == nil || !o.eng.Ready()
		o.mu.Unlock()
		if healthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
