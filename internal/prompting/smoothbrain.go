package prompting

import (
	"context"
	"fmt"
	"time"
)

// Smoothbrain is the quick one-shot lyric writer: a single short
// prompt with no craft rules, no plan and no revision. It is kept
// selectable for comparison against the newer generators.
type Smoothbrain struct{}

const smoothbrainSystem = `You write short song lyrics for an AI music model.
Write 8-14 short lines: a verse and a chorus. Mark sections with [verse]
and [chorus] on their own lines. Simple, singable, %s. Output ONLY the
lyrics, no title, no explanations.`

// Name implements LyricsGenerator.
func (*Smoothbrain) Name() string { return "smoothbrain" }

// Blurb implements LyricsGenerator.
func (*Smoothbrain) Blurb() string {
	return "a quick one-shot writer; simple rhymes, no revision"
}

// Timeout implements LyricsGenerator. Generous enough to cover a cold
// model load before the single write.
func (*Smoothbrain) Timeout() time.Duration { return 90 * time.Second }

// Generate implements LyricsGenerator with the legacy single call.
// Thinking is pinned off: on reasoning models the default thinking
// phase sometimes swallows the whole reply, and this writer's identity
// is the quick single shot.
func (*Smoothbrain) Generate(ctx context.Context, llm LLM, req LyricsRequest) (string, error) {
	theme := req.Theme
	if theme == "" {
		theme = "matching the mood of the music"
	}
	user := "Music style: " + req.Style + "\nLyrics theme: " + theme
	noThink := false
	system := fmt.Sprintf(smoothbrainSystem, req.WriteIn())
	return llm.ChatWith(ctx, system, user, ChatOpts{Think: &noThink})
}
