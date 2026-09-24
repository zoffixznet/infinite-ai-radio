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

// The radio lists its whole store in the order the songs were made,
// the ones other listeners have already taken first. A phone reads its
// place off that order: it plays the songs it holds in listing order,
// downloads the first one after the playing song that it lacks, and
// starts a fresh bank from the top - the oldest kept song - so it
// meets what the others have heard before it takes anything new.
func TestPhoneKeepsItsPlaceInTheStore(t *testing.T) {
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
	for _, name := range []string{"pfTossed", "pfPlayable", "pfListed", "pfAhead", "pfNextDownload", "pfNextId"} {
		fns = append(fns, jsFunction(t, src, name))
	}
	harness := `
var pf;
function rows(spec) {
  return spec.trim().split(/\s+/).filter(Boolean).map(function (id) { return {id: id}; });
}
function set(list) { var o = {}; list.split(/\s+/).filter(Boolean).forEach(function (id) { o[id] = {title: ""}; }); return o; }
function state(r, have, playing, seen) {
  pf = {rows: rows(r), have: set(have), playingId: playing, seen: {}, bad: {}, tossed: {}, wrapped: false};
  Object.keys(set(seen)).forEach(function (id) { pf.seen[id] = true; });
}
function id(row) { return row ? row.id : null; }
` + strings.Join(fns, "\n") + `
var out = {};
// In step with the store: playing B, holding C, D free to take.
state("A B C D E", "B C", "B", "A B");
out.inStep = {listed: pfListed("B"), next: pfNextId("B"), ahead: pfAhead(),
  fetch2: id(pfNextDownload(2, false)), fetch1: id(pfNextDownload(1, false))};
// A fresh bank: nothing playing, nothing held; it starts from the top.
state("A B C D", "", null, "");
out.fresh = {fetch: id(pfNextDownload(3, false))};
// The radio let the playing song go: every listed song is ahead.
state("C D E", "B C D", "B", "B");
out.letGo = {listed: pfListed("B"), ahead: pfAhead(), next: pfNextId("B"), fetch3: id(pfNextDownload(3, false))};
// Off the network, cycling its own store: no listing to read a place off.
state("", "X Y Z", "Y", "X Y Z");
out.offline = {next: pfNextId("Y")};
// A skipped song is never fetched again.
state("A B C", "A", "A", "A");
pf.tossed["B"] = {title: ""};
out.tossed = {fetch: id(pfNextDownload(3, false))};
console.log(JSON.stringify(out));
`
	cmd := exec.Command(node, "-e", harness)
	outRaw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outRaw)
	}
	var got struct {
		InStep struct {
			Listed bool    `json:"listed"`
			Next   *string `json:"next"`
			Ahead  int     `json:"ahead"`
			Fetch2 *string `json:"fetch2"`
			Fetch1 *string `json:"fetch1"`
		} `json:"inStep"`
		Fresh struct{ Fetch *string } `json:"fresh"`
		LetGo struct {
			Listed bool    `json:"listed"`
			Ahead  int     `json:"ahead"`
			Next   *string `json:"next"`
			Fetch3 *string `json:"fetch3"`
		} `json:"letGo"`
		Offline struct{ Next *string }  `json:"offline"`
		Tossed  struct{ Fetch *string } `json:"tossed"`
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
	if !got.InStep.Listed || str(got.InStep.Next) != "C" || got.InStep.Ahead != 1 {
		t.Errorf("in step: listed %v, next %s, ahead %d; want listed, C and 1", got.InStep.Listed, str(got.InStep.Next), got.InStep.Ahead)
	}
	if str(got.InStep.Fetch2) != "D" || str(got.InStep.Fetch1) != "<none>" {
		t.Errorf("in step: fetch at depth 2 = %s, at depth 1 = %s; want D and nothing",
			str(got.InStep.Fetch2), str(got.InStep.Fetch1))
	}
	if f := str(got.Fresh.Fetch); f != "A" {
		t.Errorf("a fresh bank fetches %s first, want A - the oldest song the radio keeps", f)
	}
	if got.LetGo.Listed || got.LetGo.Ahead != 2 || str(got.LetGo.Next) != "C" || str(got.LetGo.Fetch3) != "E" {
		t.Errorf("after the radio let the playing song go: listed %v, ahead %d, next %s, fetch %s; want unlisted, 2, C, E",
			got.LetGo.Listed, got.LetGo.Ahead, str(got.LetGo.Next), str(got.LetGo.Fetch3))
	}
	if n := str(got.Offline.Next); n != "Z" {
		t.Errorf("offline it goes from Y to %s, want Z: the store still cycles in its own order", n)
	}
	if f := str(got.Tossed.Fetch); f != "C" {
		t.Errorf("with B skipped it fetches %s, want C", f)
	}
}
