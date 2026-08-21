package ui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"iar/internal/player"
)

// RunPlain drives the stream with a plain line-based interface: events and
// responses go to w, commands are read line by line from r. It is the
// interface used over pipes and in scripts, and works with nothing but
// typed words and Enter.
func RunPlain(ctx context.Context, c *Controller, r io.Reader, w io.Writer) error {
	fmt.Fprintln(w, "Infinite AI Radio: type steering text or commands; 'help' lists them, 'presets' lists presets; 'quit' exits.")

	// Print stream events as they arrive.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-c.O.Events():
				if !ok {
					return
				}
				fmt.Fprintln(w, "* "+ev.Text)
			}
		}
	}()

	// Keep startup progress visible until music plays: first line right
	// away, then every two seconds while a startup phase is active.
	go func() {
		printProgress(w, c.O.Status())
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			printProgress(w, c.O.Status())
		}
	}()

	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		readErr <- scanner.Err()
		close(lines)
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-lines:
			if !ok {
				// Input closed (end of script/pipe): keep playing
				// until the context ends.
				if err := <-readErr; err != nil {
					return err
				}
				<-ctx.Done()
				return nil
			}
			resp, quit := c.Handle(line)
			if resp != "" {
				for _, l := range strings.Split(resp, "\n") {
					fmt.Fprintln(w, "  "+l)
				}
			}
			if quit {
				return nil
			}
		}
	}
}

// printProgress writes one startup progress line when a phase is active.
func printProgress(w io.Writer, st player.Status) {
	if st.Phase == "" || st.Phase == "playing" {
		return
	}
	line := fmt.Sprintf("... %s - %s elapsed", st.Phase, st.PhaseElapsed.Round(time.Second))
	if st.PhaseExpected > 0 {
		line += fmt.Sprintf(" (usually ~%s)", st.PhaseExpected.Round(time.Second))
	}
	if st.PhaseSlow {
		line += " - taking longer than usual, see 'iar doctor'"
	}
	fmt.Fprintln(w, line)
}
