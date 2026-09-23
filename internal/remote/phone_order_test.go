package remote

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// jsFunction lifts one named function's source out of the phone app, so
// its logic can be run as it ships rather than from a copy kept beside
// the test. Strings and comments are skipped while matching braces.
func jsFunction(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "function "+name+"(")
	if start < 0 {
		t.Fatalf("no function %s in app.js", name)
	}
	open := strings.Index(src[start:], "{")
	if open < 0 {
		t.Fatalf("function %s has no body", name)
	}
	depth := 0
	for i := start + open; i < len(src); i++ {
		switch c := src[i]; c {
		case '"', '\'':
			for i++; i < len(src) && src[i] != c; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
			} else if i+1 < len(src) && src[i+1] == '*' {
				end := strings.Index(src[i+2:], "*/")
				if end < 0 {
					t.Fatalf("unterminated comment in %s", name)
				}
				i += end + 3
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("function %s never closes", name)
	return ""
}

// The radio lists the stream's own songs first - the play queue, then the
// songs waiting on disk - and spares from its library after them. A
// banked song goes out under its own id, so the song a phone is playing
// can turn up among the spares: the radio's playing and previous songs
// land there the moment the speakers start them. A phone that read its
// place off that row decided it was at the end of the stream, stopped
// fetching fresh songs, and walked backwards through ones the radio had
// already played. It must read its place off the stream's order alone.
func TestPhoneKeepsItsPlaceInTheStreamNotAmongTheSpares(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	raw, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	var fns []string
	for _, name := range []string{"pfTossed", "pfPlayable", "pfOrdered", "pfOrderedId",
		"pfListedPlaying", "pfAhead", "pfSpares", "pfNextDownload", "pfNextId"} {
		fns = append(fns, jsFunction(t, src, name))
	}
	harness := `
var pf;
// rows("C D | B A"): stream rows before the bar, spares after it.
function rows(spec) {
  var parts = spec.split("|"), out = [];
  (parts[0] || "").trim().split(/\s+/).filter(Boolean).forEach(function (id) { out.push({id: id, kind: "queue"}); });
  (parts[1] || "").trim().split(/\s+/).filter(Boolean).forEach(function (id) { out.push({id: id, kind: "library"}); });
  return out;
}
function set(list) { var o = {}; list.split(/\s+/).filter(Boolean).forEach(function (id) { o[id] = {title: ""}; }); return o; }
function state(r, have, playing, seen) {
  pf = {rows: rows(r), have: set(have), playingId: playing, seen: {}, bad: {}, tossed: {}, wrapped: false};
  Object.keys(set(seen)).forEach(function (id) { pf.seen[id] = true; });
}
function id(row) { return row ? row.id : null; }
` + strings.Join(fns, "\n") + `
var out = {};
// Ahead of the speakers, holding B C D and an A it took earlier; the
// radio has just started B, so B and A are now spares at the end.
state("C D E F | B A Z Y", "B C D A", "B", "B");
out.ahead = {listed: pfListedPlaying(), ahead: pfAhead(), next: pfNextId("B"),
  fetch3: id(pfNextDownload(3, false)), fetch2: id(pfNextDownload(2, false))};
// Steered while playing P: the new sound is the stream, P a spare.
state("N2 N3 | N1 M2 M1 P Q", "P", "P", "P");
out.steer = {fetch: id(pfNextDownload(3, false))};
// Off the network, cycling its own store: no listing to read a place off.
state("", "X Y Z", "Y", "X Y Z");
out.offline = {next: pfNextId("Y")};
// The ordinary case, in step with the stream.
state("B C D | Q", "B C", "B", "B");
out.inStep = {next: pfNextId("B"), ahead: pfAhead(), fetch2: id(pfNextDownload(2, false)), fetch1: id(pfNextDownload(1, false))};
// Economical level with a spare already held: spares only pad up to depth.
state("B | X Y", "B X", "B", "B");
out.eco = {fetch1: id(pfNextDownload(1, false)), fetch2: id(pfNextDownload(2, false))};
console.log(JSON.stringify(out));
`
	cmd := exec.Command(node, "-e", harness)
	outRaw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outRaw)
	}
	var got struct {
		Ahead struct {
			Listed bool    `json:"listed"`
			Ahead  int     `json:"ahead"`
			Next   *string `json:"next"`
			Fetch3 *string `json:"fetch3"`
			Fetch2 *string `json:"fetch2"`
		} `json:"ahead"`
		Steer   struct{ Fetch *string } `json:"steer"`
		Offline struct{ Next *string }  `json:"offline"`
		InStep  struct {
			Next   *string `json:"next"`
			Ahead  int     `json:"ahead"`
			Fetch2 *string `json:"fetch2"`
			Fetch1 *string `json:"fetch1"`
		} `json:"inStep"`
		Eco struct {
			Fetch1 *string `json:"fetch1"`
			Fetch2 *string `json:"fetch2"`
		} `json:"eco"`
	}
	if err := json.Unmarshal(outRaw, &got); err != nil {
		t.Fatalf("harness output %q: %v", outRaw, err)
	}
	str := func(p *string) string {
		if p == nil {
			return "<none>"
		}
		return *p
	}

	if got.Ahead.Listed {
		t.Error("the playing song, listed only as a spare, was read as a place in the stream")
	}
	if n := str(got.Ahead.Next); n != "C" {
		t.Errorf("after B the phone plays %s, want C - the head of the stream, not a song the radio played", n)
	}
	if got.Ahead.Ahead != 2 {
		t.Errorf("ahead = %d, want 2 (C and D): spares it has already played are not ahead of it", got.Ahead.Ahead)
	}
	if f := str(got.Ahead.Fetch3); f != "E" {
		t.Errorf("with room for three it fetches %s, want E - a fresh song, not an old spare", f)
	}
	if f := str(got.Ahead.Fetch2); f != "<none>" {
		t.Errorf("with two fresh songs held and room for two it fetches %s, want nothing", f)
	}
	if f := str(got.Steer.Fetch); f != "N2" {
		t.Errorf("after a steer it fetches %s, want N2 - the new sound, not the old one", f)
	}
	if n := str(got.Offline.Next); n != "Z" {
		t.Errorf("offline it goes from Y to %s, want Z: the store still cycles in its own order", n)
	}
	if n := str(got.InStep.Next); n != "C" || got.InStep.Ahead != 1 {
		t.Errorf("in step: next %s ahead %d, want C and 1", n, got.InStep.Ahead)
	}
	if str(got.InStep.Fetch2) != "D" || str(got.InStep.Fetch1) != "<none>" {
		t.Errorf("in step: fetch at depth 2 = %s, at depth 1 = %s; want D and nothing",
			str(got.InStep.Fetch2), str(got.InStep.Fetch1))
	}
	if str(got.Eco.Fetch1) != "<none>" || str(got.Eco.Fetch2) != "Y" {
		t.Errorf("spares: fetch at depth 1 = %s, at depth 2 = %s; want nothing and Y",
			str(got.Eco.Fetch1), str(got.Eco.Fetch2))
	}
}
