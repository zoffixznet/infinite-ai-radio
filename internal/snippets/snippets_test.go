package snippets

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"iar/internal/audio"
	"iar/internal/export"
)

func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", Untagged},
		{"   ", Untagged},
		{"gym", "gym"},
		{"Gym Grind", "gym_grind"},
		{"late-night/lofi", "late_night_lofi"},
		{"___", Untagged},
		{"__edge__", "edge"},
		{"ünïcode tag!", "n_code_tag"},
		{"../../etc/passwd", "etc_passwd"},
		{"..", Untagged},
		{".", Untagged},
		{"/", Untagged},
		{"tag/../../x", "tag_______x"},
		{"C:\\Windows\\System32", "c__windows_system32"},
		{"a b c d e f g h i j k l m n o p q r s t u v w x y z", "a_b_c_d_e_f_g_h_i_j_k_l_m_n_o_p_q_r_s_t"},
		{strings.Repeat("x", 50), strings.Repeat("x", 40)},
		{strings.Repeat("x", 39) + "_y", strings.Repeat("x", 39)},
		{"MiXeD_CaSe_123", "mixed_case_123"},
	}
	for _, tc := range cases {
		if got := Slug(tc.in); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := Slug(tc.in); !slugRe.MatchString(got) {
			t.Errorf("Slug(%q) = %q is not a valid path component", tc.in, got)
		}
	}
}

func TestValidRef(t *testing.T) {
	good := [][2]string{
		{"untagged", "20260821-120000-track.mp3"},
		{"gym_grind", "a.mp3"},
	}
	bad := [][2]string{
		{"..", "a.mp3"}, {"gym", "../a.mp3"}, {"gym", "..mp3"}, {"Gym", "a.mp3"},
		{"gym", ".hidden.mp3"}, {"gym", "a.wav"}, {"gym", "a/b.mp3"}, {"", "a.mp3"}, {"gym", ""},
		{strings.Repeat("x", 41), "a.mp3"},
	}
	for _, g := range good {
		if !ValidRef(g[0], g[1]) {
			t.Errorf("ValidRef(%q, %q) = false", g[0], g[1])
		}
	}
	for _, b := range bad {
		if ValidRef(b[0], b[1]) {
			t.Errorf("ValidRef(%q, %q) = true", b[0], b[1])
		}
	}
}

func TestFileNameAndPath(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 34, 56, 0, time.UTC)
	if got := FileName("Dark techno, driving bass!", now); got != "20260821-123456-dark-techno-driving-bass.mp3" {
		t.Fatalf("FileName = %q", got)
	}
	if got := FileName("", now); got != "20260821-123456-track.mp3" {
		t.Fatalf("FileName(empty) = %q", got)
	}
	long := FileName(strings.Repeat("abc ", 40), now)
	if len(long) > len("20260821-123456-")+60+len(".mp3") {
		t.Fatalf("FileName not bounded: %q", long)
	}
	p := Path("/snips", "Gym Grind", "calm piano", now)
	if p != filepath.Join("/snips", "gym_grind", "20260821-123456-calm-piano.mp3") {
		t.Fatalf("Path = %q", p)
	}
}

func TestMigrateFlatLayout(t *testing.T) {
	dir := t.TempDir()
	// Nothing to do on a missing or empty directory.
	if n, err := Migrate(filepath.Join(dir, "missing")); err != nil || n != 0 {
		t.Fatalf("Migrate(missing) = %d, %v", n, err)
	}
	os.WriteFile(filepath.Join(dir, "old-1.mp3"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "old-2.mp3"), []byte("y"), 0o644)
	os.WriteFile(filepath.Join(dir, ".partial.mp3"), []byte("z"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "gym"), 0o755)
	os.WriteFile(filepath.Join(dir, "gym", "keep.mp3"), []byte("k"), 0o644)

	n, err := Migrate(dir)
	if err != nil || n != 2 {
		t.Fatalf("Migrate = %d, %v", n, err)
	}
	for _, want := range []string{"untagged/old-1.mp3", "untagged/old-2.mp3", "gym/keep.mp3", "notes.txt", ".partial.mp3"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("after migrate, %s missing", want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "old-1.mp3")); err == nil {
		t.Error("flat file left behind")
	}
	// Idempotent.
	if n, err := Migrate(dir); err != nil || n != 0 {
		t.Fatalf("second Migrate = %d, %v", n, err)
	}
}

// id3v2 builds a minimal ID3v2.3 tag with UTF-16 (BOM) text frames, the
// shape ffmpeg writes.
func id3v2(frames map[string]string) []byte {
	var body []byte
	for id, text := range frames {
		var data []byte
		data = append(data, 1, 0xFF, 0xFE) // UTF-16 with little-endian BOM
		for _, r := range text {
			data = append(data, byte(r), byte(r>>8))
		}
		frame := []byte(id)
		frame = binary.BigEndian.AppendUint32(frame, uint32(len(data)))
		frame = append(frame, 0, 0)
		frame = append(frame, data...)
		body = append(body, frame...)
	}
	body = append(body, make([]byte, 16)...) // padding
	size := len(body)
	hdr := []byte{'I', 'D', '3', 3, 0, 0,
		byte(size>>21) & 0x7f, byte(size>>14) & 0x7f, byte(size>>7) & 0x7f, byte(size) & 0x7f}
	return append(hdr, body...)
}

func TestParseID3AndCBRDurationEstimate(t *testing.T) {
	tag := id3v2(map[string]string{"TIT2": "dark techno", "TALB": "gym_grind"})
	// One MPEG1 Layer III header: 48 kHz, 128 kbps, stereo, no Xing.
	frame := []byte{0xFF, 0xFB, 0x94, 0x00}
	// Pad to 160 kB of "audio" = 10 s at 128 kbps.
	audio := append(frame, make([]byte, 160000-4)...)
	path := filepath.Join(t.TempDir(), "synthetic.mp3")
	os.WriteFile(path, append(tag, audio...), 0o644)

	info, err := ReadInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "dark techno" || info.Album != "gym_grind" {
		t.Fatalf("tags = %+v", info)
	}
	if math.Abs(info.Duration.Seconds()-10) > 0.05 {
		t.Fatalf("estimated duration = %v, want ~10s", info.Duration)
	}
	// UTF-8 and Latin-1 encodings decode too.
	if got := decodeText(append([]byte{3}, "caf\xc3\xa9"...)); got != "café" {
		t.Errorf("utf-8 decode = %q", got)
	}
	if got := decodeText(append([]byte{0}, "caf\xe9\x00"...)); got != "café" {
		t.Errorf("latin-1 decode = %q", got)
	}
}

func ffprobeDuration(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCatalogListsRealEncodedChunks(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	// 3.0 s of a quiet tone, saved under two tags with the tags written
	// to the album field the way the save command does.
	samples := make([]int16, 3*audio.SampleRate*audio.Channels)
	for i := 0; i < len(samples); i += 2 {
		v := int16(3000 * math.Sin(2*math.Pi*440*float64(i/2)/audio.SampleRate))
		samples[i], samples[i+1] = v, v
	}
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.Local)
	older := Path(dir, "Gym Grind", "energetic rock about winning", now)
	newer := Path(dir, "", "calm piano, quiet", now.Add(time.Minute))
	for _, p := range []string{older, newer} {
		os.MkdirAll(filepath.Dir(p), 0o755)
	}
	if err := export.EncodeMP3(ctx, samples, older, export.MP3Options{Title: "energetic rock about winning", Album: "gym_grind"}); err != nil {
		t.Fatal(err)
	}
	if err := export.EncodeMP3(ctx, samples, newer, export.MP3Options{Title: "calm piano, quiet", Subtitle: "calm, gentle", Album: Untagged}); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(older, now, now)
	os.Chtimes(newer, now.Add(time.Minute), now.Add(time.Minute))
	// Litter that must be ignored.
	os.WriteFile(filepath.Join(dir, "untagged", ".tmp.mp3"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "README.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "Bad Tag Dir"), 0o755)

	cat := NewCatalog(dir)
	list, err := cat.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Tag != Untagged || list[0].Title != "calm piano, quiet" || list[0].Subtitle != "calm, gentle" || list[1].Tag != "gym_grind" {
		t.Fatalf("order/tags wrong: %+v", list)
	}
	for _, ch := range list {
		want := ffprobeDuration(t, filepath.Join(dir, ch.Tag, ch.File))
		if math.Abs(ch.Seconds-want) > 0.1 {
			t.Errorf("%s: duration %.3f vs ffprobe %.3f", ch.File, ch.Seconds, want)
		}
		if ch.Bytes <= 0 || ch.Saved.IsZero() {
			t.Errorf("chunk meta incomplete: %+v", ch)
		}
	}
	info, err := ReadInfo(older)
	if err != nil || info.Album != "gym_grind" {
		t.Fatalf("album tag = %+v, %v", info, err)
	}
	// Second listing hits the cache and agrees.
	again, _ := cat.List()
	if len(again) != 2 || again[0] != list[0] {
		t.Fatalf("cached listing differs: %+v", again)
	}

	// Resolution only accepts real chunks.
	if p, ok := cat.Resolve(list[1].Tag, list[1].File); !ok || p != older {
		t.Fatalf("Resolve = %q, %v", p, ok)
	}
	for _, bad := range [][2]string{{"gym_grind", "missing.mp3"}, {"..", "x.mp3"}, {"gym_grind", "../README.txt"}, {"untagged", ".tmp.mp3"}} {
		if _, ok := cat.Resolve(bad[0], bad[1]); ok {
			t.Errorf("Resolve(%q, %q) accepted", bad[0], bad[1])
		}
	}
}
