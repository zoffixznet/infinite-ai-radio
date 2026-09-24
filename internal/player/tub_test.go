package player

import (
	"context"
	"testing"
	"time"
)

// Feeding a song into playback no longer deletes it: the store marks
// it taken and keeps it for a player that has not caught up. The level
// - what nobody has taken - is what the generator fills against; the
// listing offers untaken songs as the stream and the taken ones as
// spares; and a song already fed can still be served from disk, so a
// save reaches it as the file it was.
func TestAFedSongStaysInTheStoreForOtherPlayers(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.cfg.Buffer.Songs = 72
	ids := []string{"t-1790000000000-0031", "t-1790000000000-0032", "t-1790000000000-0033"}
	for i, id := range ids {
		renderSong(t, o, i+1, id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.feedLoop(ctx)
	waitFor(t, 20*time.Second, "two songs fed", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return len(o.queue) == phasedPrefetch
	})
	cancel()

	if n, _ := o.Buffer.Level(0); n != 1 {
		t.Fatalf("level = %d after two songs were fed, want 1", n)
	}
	if n, _ := o.Buffer.TrackStats(0); n != 3 {
		t.Fatalf("%d songs on disk after two were fed, want all 3", n)
	}
	if got := o.Buffer.Cursor(playerCursor); got != "e00000000-00000002" {
		t.Fatalf("the player's cursor = %q, want the second song", got)
	}
	// Fed, and still a file: the save copies it as it is.
	if _, ok := o.TrackFile(ids[0]); !ok {
		t.Fatal("a fed song has no file to serve")
	}

	// Listed once each: the two in memory as the stream, the third as
	// the stream from disk, none of them twice as a spare.
	rows := listing(t, o)
	if len(rows) != 3 || rows[ids[0]] != "queue" || rows[ids[2]] != "queue" {
		t.Fatalf("listing while fed = %v", rows)
	}

	// Played and gone from memory: the taken songs are the spares now.
	o.mu.Lock()
	o.queue = nil
	o.mu.Unlock()
	rows = listing(t, o)
	if len(rows) != 3 || rows[ids[0]] != "library" || rows[ids[1]] != "library" || rows[ids[2]] != "queue" {
		t.Fatalf("listing after play = %v", rows)
	}
}

// The store keeps only so many taken songs: past the depth, feeding one
// more trims the oldest.
func TestTheOldestTakenSongsAreTrimmedAsTheStoreFills(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.cfg.Buffer.Songs = 3
	for seq := 1; seq <= 6; seq++ {
		renderSong(t, o, seq, "t-1790000000000-004"+string(rune('0'+seq)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.feedLoop(ctx)
	// Drain the prefetch as a player would, until five songs are taken.
	waitFor(t, 30*time.Second, "five songs taken", func() bool {
		o.mu.Lock()
		if len(o.queue) > 0 {
			o.queue = o.queue[1:]
		}
		o.mu.Unlock()
		o.kickGen()
		return o.Buffer.Cursor(playerCursor) == "e00000000-00000005"
	})
	cancel()
	n, _ := o.Buffer.TrackStats(0)
	level, _ := o.Buffer.Level(0)
	if level != 1 || n != 4 {
		t.Fatalf("after five takes with three kept: %d on disk, level %d; want 4 and 1", n, level)
	}
	if _, ok := o.Buffer.TrackPath(0, "e00000000-00000001"); ok {
		t.Fatal("the oldest taken song was not trimmed")
	}
	if _, ok := o.Buffer.TrackPath(0, "e00000000-00000003"); !ok {
		t.Fatal("a kept taken song was trimmed")
	}
}
