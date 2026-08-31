package acestep

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestWaitForPredecessorsReturnsImmediatelyWhenNone(t *testing.T) {
	start := time.Now()
	waitForPredecessors(context.Background(), time.Minute, time.Hour, discardLog(),
		func() []int { return nil })
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("waited despite no predecessors")
	}
}

func TestWaitForPredecessorsWaitsUntilGone(t *testing.T) {
	calls := 0
	waitForPredecessors(context.Background(), time.Minute, time.Millisecond, discardLog(),
		func() []int {
			calls++
			if calls < 4 {
				return []int{12345}
			}
			return nil
		})
	if calls < 4 {
		t.Fatalf("lister called %d times; want at least 4", calls)
	}
}

func TestWaitForPredecessorsGivesUpAfterTimeout(t *testing.T) {
	start := time.Now()
	waitForPredecessors(context.Background(), 20*time.Millisecond, time.Millisecond, discardLog(),
		func() []int { return []int{12345} })
	if e := time.Since(start); e > 5*time.Second {
		t.Fatalf("wait ran %s; want bounded by the timeout", e)
	}
}

func TestWaitForPredecessorsStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	waitForPredecessors(ctx, time.Minute, time.Hour, discardLog(),
		func() []int { return []int{12345} })
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("cancelled context did not stop the wait")
	}
}
