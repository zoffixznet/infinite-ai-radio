package session

import "reflect"

// PromptSpec is the structured steering state of a music session.
// Steering inputs mutate it with real semantics ("less guitars" removes
// guitars from the positive side and adds them to Negatives) and one
// renderer turns it into the caption and request fields the engine
// honours. The zero value is an empty spec.
type PromptSpec struct {
	// Genre is ordered; the first entry anchors the style.
	Genre []string `json:"genre,omitempty"`
	// Instruments maps an instrument to its emphasis weight (1..3).
	// Negated instruments live in Negatives, never here.
	Instruments map[string]int `json:"instruments,omitempty"`
	// Mood holds mood and energy words.
	Mood []string `json:"mood,omitempty"`
	// BPM is the requested tempo (0 = unset). Rendered into both the
	// caption and the engine's bpm field.
	BPM int `json:"bpm,omitempty"`
	// TempoWords holds tempo descriptions when no BPM is set.
	TempoWords []string `json:"tempo_words,omitempty"`
	// KeyScale is an engine-valid key ("C major", "F# minor"); empty
	// leaves the key to the model.
	KeyScale string `json:"key_scale,omitempty"`
	// TimeSignature is "2", "3", "4" or "6" (beats per bar).
	TimeSignature string `json:"time_signature,omitempty"`
	// VocalLanguage is an ISO language code for sung vocals.
	VocalLanguage string `json:"vocal_language,omitempty"`
	// LanguagePinned reports that the listener named this language by
	// hand ("sing in french"). Only a pin outranks the configured
	// vocal-language list; a language a preset happens to carry is a
	// default the list is free to replace.
	LanguagePinned bool `json:"language_pinned,omitempty"`
	// VocalStyle holds vocal delivery words ("soft female vocals").
	VocalStyle []string `json:"vocal_style,omitempty"`
	// Production holds production and texture words.
	Production []string `json:"production,omitempty"`
	// Extra holds free-text positives that fit no other slot.
	Extra []string `json:"extra,omitempty"`
	// Negatives lists what the music must avoid. Rendered into the
	// engine's negative prompt, never into the caption.
	Negatives []string `json:"negatives,omitempty"`
}

// Clone returns a deep copy.
func (p *PromptSpec) Clone() *PromptSpec {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Genre = append([]string(nil), p.Genre...)
	cp.Mood = append([]string(nil), p.Mood...)
	cp.TempoWords = append([]string(nil), p.TempoWords...)
	cp.VocalStyle = append([]string(nil), p.VocalStyle...)
	cp.Production = append([]string(nil), p.Production...)
	cp.Extra = append([]string(nil), p.Extra...)
	cp.Negatives = append([]string(nil), p.Negatives...)
	if p.Instruments != nil {
		cp.Instruments = make(map[string]int, len(p.Instruments))
		for k, v := range p.Instruments {
			cp.Instruments[k] = v
		}
	}
	return &cp
}

// Equal reports whether two specs describe the same sound. Nil and the
// zero value are equal.
func (p *PromptSpec) Equal(q *PromptSpec) bool {
	a, b := p, q
	if a == nil {
		a = &PromptSpec{}
	}
	if b == nil {
		b = &PromptSpec{}
	}
	na, nb := *a.Clone(), *b.Clone()
	if len(na.Instruments) == 0 {
		na.Instruments = nil
	}
	if len(nb.Instruments) == 0 {
		nb.Instruments = nil
	}
	norm := func(s *PromptSpec) {
		for _, f := range []*[]string{&s.Genre, &s.Mood, &s.TempoWords, &s.VocalStyle, &s.Production, &s.Extra, &s.Negatives} {
			if len(*f) == 0 {
				*f = nil
			}
		}
	}
	norm(&na)
	norm(&nb)
	return reflect.DeepEqual(na, nb)
}
