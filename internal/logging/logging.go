// Package logging sets up the application's structured logger: JSON lines to
// a file in the data directory, optionally mirrored to stderr.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// Setup opens (appending) the log file at path, rotating it whenever it
// would pass maxBytes, and installs a JSON slog logger writing to it.
// When verbose is true, log lines are also mirrored to stderr in text
// form. The returned closer flushes and closes the file.
func Setup(path string, maxBytes int64, verbose bool) (*slog.Logger, io.Closer, error) {
	f, err := OpenRotating(path, maxBytes)
	if err != nil {
		return nil, nil, err
	}
	var handler slog.Handler = slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})
	if verbose {
		handler = &teeHandler{
			primary:   handler,
			secondary: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}),
		}
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, f, nil
}

// teeHandler forwards every record to two handlers.
type teeHandler struct {
	primary   slog.Handler
	secondary slog.Handler
}

func (t *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return t.primary.Enabled(ctx, level) || t.secondary.Enabled(ctx, level)
}

func (t *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	err1 := t.primary.Handle(ctx, r.Clone())
	err2 := t.secondary.Handle(ctx, r)
	if err1 != nil {
		return err1
	}
	return err2
}

func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{primary: t.primary.WithAttrs(attrs), secondary: t.secondary.WithAttrs(attrs)}
}

func (t *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{primary: t.primary.WithGroup(name), secondary: t.secondary.WithGroup(name)}
}
