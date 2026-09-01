package prompting

import (
	"fmt"
	"testing"
)

// The deterministic fallback is what a track is called until the helper
// answers, so it has to read as a name rather than as the front of a
// sentence. Prompts written as prose used to become "An Aggressive And
// High-Energy": an article at the head and whatever word the 32-column
// cut happened to land on at the tail.
func TestTrackTitleReadsAsAName(t *testing.T) {
	for _, tc := range []struct {
		prompt   string
		title    string
		subtitle string
	}{
		{
			"An aggressive and high-energy nu-metal track driven by heavily distorted guitars, punchy drums",
			"Aggressive And High-energy", "punchy drums",
		},
		{
			"nu-metal, alternative metal, aggressive, heavy groove",
			"Nu-metal", "alternative metal, aggressive, heavy groove",
		},
		{
			"A clean arpeggiated electric guitar riff with a slight chorus, warm pads",
			"Clean Arpeggiated Electric", "warm pads",
		},
		{"dark techno", "Dark Techno", ""},
		{"", "Generated Track", ""},
	} {
		title, subtitle := TrackTitle(tc.prompt)
		if title != tc.title {
			t.Errorf("TrackTitle(%.40q) title = %q, want %q", tc.prompt, title, tc.title)
		}
		if subtitle != tc.subtitle {
			t.Errorf("TrackTitle(%.40q) subtitle = %q, want %q", tc.prompt, subtitle, tc.subtitle)
		}
	}
}

func TestNameFromPhrase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"An aggressive and high-energy nu-metal track", "aggressive and high-energy"},
		{"The track begins with a contemplative spoken word", "track begins"},
		{"dark techno", "dark techno"},
		// Never strip a phrase down to nothing, however little is left.
		{"the", "the"},
		{"a and", "and"},
	} {
		if got := nameFromPhrase(tc.in, 32); got != tc.want {
			t.Errorf("nameFromPhrase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A song's name is asked for when the song is planned and read back
// when it is rendered, which with the default six-hour planning horizon
// is well over a hundred songs later. The cache used to empty itself
// once it held 64 entries, so by the time a song reached the speakers
// its name had been thrown away and every track fell back to the
// prompt-derived name - a whole station of songs all called "Nu-Metal".
func TestSongNamesSurviveAFullCache(t *testing.T) {
	b := NewBuilder(nil, nil)
	const key = "song:0-1"
	b.store("n|"+key, `{"title":"Apoy Sa Dibdib","subtitle":"nu-metal, driving"}`)

	// Plan a long way ahead of the render, as the buffer really does.
	for i := 0; i < cacheCapacity-1; i++ {
		b.store(fmt.Sprintf("n|song:0-%d", i+2), `{"title":"Filler","subtitle":"x"}`)
	}

	title, subtitle, ok := b.TitleForKey(key)
	if !ok {
		t.Fatal("the song's name was evicted before its song was rendered")
	}
	if title != "Apoy Sa Dibdib" || subtitle != "nu-metal, driving" {
		t.Fatalf("TitleForKey = %q / %q", title, subtitle)
	}
}

// The cache is still bounded: past its capacity it drops the oldest
// entry rather than growing without limit.
func TestHelperCacheEvictsOldestFirst(t *testing.T) {
	b := NewBuilder(nil, nil)
	for i := 0; i < cacheCapacity+10; i++ {
		b.store(fmt.Sprintf("n|song:0-%d", i), `{"title":"T","subtitle":"s"}`)
	}
	if got := len(b.cache); got > cacheCapacity {
		t.Fatalf("cache grew to %d entries, capacity is %d", got, cacheCapacity)
	}
	if _, _, ok := b.TitleForKey("song:0-0"); ok {
		t.Error("the oldest entry should have been evicted")
	}
	if _, _, ok := b.TitleForKey(fmt.Sprintf("song:0-%d", cacheCapacity+9)); !ok {
		t.Error("the newest entry should still be cached")
	}
}
