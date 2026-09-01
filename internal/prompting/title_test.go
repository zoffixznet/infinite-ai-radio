package prompting

import "testing"

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
