package prompting

import (
	"math/rand/v2"
	"strings"
)

// This file implements the vocal-language catalogue: the languages a
// listener configured for sung vocals, and the per-track choice made
// from the ones currently switched on. With nothing configured the
// music engine keeps picking whatever language it likes, which is the
// behaviour every earlier version had.

// Language is one configured vocal language.
type Language struct {
	// Name is the listener's own wording ("English", "Bisaya
	// (Cebuano)") and is what the lyric writer is told to write in.
	Name string
	// Code is the music engine's language tag for this language, empty
	// when there is none. The tag is not only an accent: on the tracks
	// the engine writes the words for itself, it is the tag that
	// decides which language they are written in.
	Code string
}

// Engine reports whether the music engine has a tag for this language.
func (l Language) Engine() bool { return l.Code != "" }

// maxLanguages bounds a configured catalogue.
const maxLanguages = 32

// engineVoices are the languages the music engine has a tag for, paired
// with the tag, in the wording the interface offers them.
var engineVoices = []struct{ Name, Code string }{
	{"Arabic", "ar"}, {"Azerbaijani", "az"}, {"Bengali", "bn"},
	{"Bulgarian", "bg"}, {"Cantonese", "yue"}, {"Catalan", "ca"},
	{"Chinese", "zh"}, {"Croatian", "hr"}, {"Czech", "cs"},
	{"Danish", "da"}, {"Dutch", "nl"}, {"English", "en"},
	{"Finnish", "fi"}, {"French", "fr"}, {"German", "de"},
	{"Greek", "el"}, {"Haitian Creole", "ht"}, {"Hebrew", "he"},
	{"Hindi", "hi"}, {"Hungarian", "hu"}, {"Icelandic", "is"},
	{"Indonesian", "id"}, {"Italian", "it"}, {"Japanese", "ja"},
	{"Korean", "ko"}, {"Latin", "la"}, {"Lithuanian", "lt"},
	{"Malay", "ms"}, {"Nepali", "ne"}, {"Norwegian", "no"},
	{"Persian", "fa"}, {"Polish", "pl"}, {"Portuguese", "pt"},
	{"Punjabi", "pa"}, {"Romanian", "ro"}, {"Russian", "ru"},
	{"Sanskrit", "sa"}, {"Serbian", "sr"}, {"Slovak", "sk"},
	{"Spanish", "es"}, {"Swahili", "sw"}, {"Swedish", "sv"},
	{"Tagalog", "tl"}, {"Tamil", "ta"}, {"Telugu", "te"},
	{"Thai", "th"}, {"Turkish", "tr"}, {"Ukrainian", "uk"},
	{"Urdu", "ur"}, {"Vietnamese", "vi"},
}

// EngineLanguages are the languages the music engine has a tag for, in
// the wording the interface offers them. Anything else is still allowed
// - the lyrics get written in it, and the engine either borrows a close
// relative's voice or sings them untagged - so this list is a set of
// suggestions, not a restriction.
var EngineLanguages []Language

// languageAliases are the other names people write for the same
// languages; the canonical names above are matched automatically.
var languageAliases = map[string]string{
	"azeri": "az", "bangla": "bn", "bahasa indonesia": "id",
	"brazilian portuguese": "pt", "castilian": "es", "deutsch": "de",
	"espanol": "es", "farsi": "fa", "filipino": "tl", "francais": "fr",
	"haitian": "ht", "mandarin": "zh", "nederlands": "nl",
}

// unlistedVoices are tags the music engine accepts even though its own
// list does not advertise them, keyed the same way as languageCodes.
// The engine never checks a requested tag against that list; it hands it
// to the model that writes the words, which knows more languages than
// the list names.
//
// Cebuano is here because it is the only tag that produces Cebuano.
// Asking for the closest advertised language, Tagalog, produces Tagalog
// words - a different language, not an accent - and leaving the tag off
// produces whatever the model feels like. Only "ceb" produces Cebuano.
var unlistedVoices = map[string]string{
	"cebuano": "ceb", "bisaya": "ceb", "binisaya": "ceb", "visayan": "ceb",
}

// languageCodes is every recognized name mapped to the engine's tag.
var languageCodes = map[string]string{}

func init() {
	for _, v := range engineVoices {
		EngineLanguages = append(EngineLanguages, Language{Name: v.Name, Code: v.Code})
		languageCodes[strings.ToLower(v.Name)] = v.Code
	}
	for name, code := range languageAliases {
		languageCodes[name] = code
	}
}

// LanguageCode resolves a language name to the engine's tag, or "" when
// the engine has none. A bare tag ("ru") resolves to itself, and a name
// that qualifies itself in brackets is tried both ways, so "Bisaya
// (Cebuano)" and "Norwegian (Bokmal)" both do the sensible thing.
func LanguageCode(name string) string {
	parts := languageNameParts(name)
	for _, part := range parts {
		if code, ok := languageCodes[part]; ok {
			return code
		}
		if validLanguages[part] {
			return part
		}
	}
	for _, part := range parts {
		if code, ok := unlistedVoices[part]; ok {
			return code
		}
	}
	return ""
}

// languageNameParts yields the lookup keys for a written language name,
// most specific first: the whole name flattened, then each part of a
// name that qualifies itself with brackets, a slash or a comma.
func languageNameParts(name string) []string {
	name = strings.ToLower(strings.TrimSpace(name))
	flat := strings.Map(func(r rune) rune {
		if r == '(' || r == ')' {
			return ' '
		}
		return r
	}, name)
	parts := []string{strings.Join(strings.Fields(flat), " ")}
	if i := strings.IndexByte(name, '('); i >= 0 {
		if j := strings.IndexByte(name[i:], ')'); j > 1 {
			// The qualifier in brackets is usually the precise one.
			parts = append(parts, strings.TrimSpace(name[i+1:i+j]), strings.TrimSpace(name[:i]))
		}
	}
	for _, sep := range []string{"/", ","} {
		if strings.Contains(name, sep) {
			for _, p := range strings.Split(name, sep) {
				parts = append(parts, strings.TrimSpace(p))
			}
		}
	}
	return parts
}

// ParseLanguages turns configured names into the catalogue, dropping
// blanks and duplicates and keeping the configured order.
func ParseLanguages(names []string) []Language {
	var out []Language
	seen := map[string]bool{}
	for _, raw := range names {
		name := strings.Join(strings.Fields(raw), " ")
		if name == "" || len(name) > 40 {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] || len(out) >= maxLanguages {
			continue
		}
		seen[key] = true
		out = append(out, Language{Name: name, Code: LanguageCode(name)})
	}
	return out
}

// EnabledLanguages narrows a catalogue to the languages a session sings
// in. A language the session says nothing about counts as on, so adding
// one to the configuration starts using it straight away.
func EnabledLanguages(catalogue []Language, picked map[string]bool) []Language {
	var out []Language
	for _, l := range catalogue {
		if on, ok := picked[l.Name]; ok && !on {
			continue
		}
		out = append(out, l)
	}
	return out
}

// pickLanguage chooses the language for one track: an independent
// uniform draw from the languages currently switched on. The same
// language can follow itself - that is what drawing at random means,
// and it is what a list of languages is understood to promise.
func pickLanguage(langs []Language) (Language, bool) {
	if len(langs) == 0 {
		return Language{}, false
	}
	return langs[rand.IntN(len(langs))], true
}

// LanguageName is the readable name for an engine tag, used when a
// steering input ("sing in french") pinned the language instead of the
// catalogue. It falls back to the tag itself.
func LanguageName(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" {
		return ""
	}
	for _, l := range EngineLanguages {
		if l.Code == code {
			return l.Name
		}
	}
	return code
}
