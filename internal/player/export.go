package player

import (
	"context"
	"fmt"
	"time"

	"iar/internal/engine"
	"iar/internal/export"
	"iar/internal/session"
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
	phased := o.phasedEnabled()
	if o.sess.Mode == session.ModeMusic && o.eng == nil {
		o.mu.Unlock()
		return "the music engine is not available"
	}
	if o.sess.Mode == session.ModeMusic && !phased && !o.eng.Ready() {
		// The phased engine may be deliberately hibernated; the export
		// wakes it below. The fused path keeps the old behavior.
		o.mu.Unlock()
		return "the music engine is not ready yet; try again shortly"
	}
	sess := *o.sess
	sess.Tweaks = append([]session.Entry(nil), o.sess.Tweaks...)
	o.exporting = fmt.Sprintf("%d min", minutes)
	o.mu.Unlock()

	if outPath == "" {
		outPath = export.DefaultPath(exportsDir, sess.Name, minutes, 0)
	}
	ctx := o.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		// The same predicate the pipeline itself uses: a configured
		// buffer is not a phased run unless the engine can plan and
		// render (with --engine noise there is no engine at all).
		if o.phasedEnabled() {
			// Wake the hibernated engine and hold it awake for the
			// export; the cycle loop skips hibernation while an export
			// runs, and the next cycle decides afterwards.
			o.setEngineActive(true)
			if !o.waitEngineReady(ctx) {
				o.mu.Lock()
				o.exporting = ""
				o.mu.Unlock()
				o.log.Error("export failed", "event", "export_failed", "error", "engine did not become ready")
				o.emit("export failed: the music engine did not become ready")
				o.kickGen()
				return
			}
		}
		r := &export.Renderer{
			Engine:           o.exportEngine(),
			Builder:          o.builder,
			TrackSeconds:     o.cfg.TrackSeconds,
			CrossfadeSeconds: o.cfg.CrossfadeSeconds,
			MP3Quality:       o.cfg.MP3Quality,
			Gate:             o.exportGate,
			Log:              o.log,
			Progress: func(line string) {
				o.log.Info("export progress", "event", "export_progress", "line", line)
			},
		}
		err := r.Render(ctx, &sess, export.Request{Minutes: minutes, OutPath: outPath})
		o.mu.Lock()
		o.exporting = ""
		o.mu.Unlock()
		// Let the cycle loop reassess: it hibernates the engine if no
		// generation work is due.
		o.kickGen()
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
		mode := o.sess.Mode
		queued := len(o.queue)
		epoch := o.epoch
		o.mu.Unlock()
		healthy := mode == session.ModeNoise || o.eng == nil
		if !healthy && o.phasedEnabled() {
			// Phased playback feeds from disk; the export may run as
			// long as a comfortable margin of rendered audio remains.
			healthy = o.bufferedSeconds(epoch) >= float64(o.cfg.Buffer.RenderLowMinutes)*60/2
		} else if !healthy {
			healthy = queued >= o.cfg.BufferTracks || !o.eng.Ready()
		}
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
