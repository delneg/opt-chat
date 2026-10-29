package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func decodeSize(t *testing.T, b []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height, format
}

// Images go to the model at most imageEdge a side and imageBytes; one that
// fits is left alone, byte for byte.
func TestFitImage(t *testing.T) {
	t.Parallel()
	small := pngBytes(100, 50)
	if b, ext, err := fitImage(small); err != nil || ext != "png" || !bytes.Equal(b, small) {
		t.Fatalf("a small image changed: %s, %v", ext, err)
	}
	b, ext, err := fitImage(pngBytes(3000, 1500))
	if w, h, f := decodeSize(t, b); err != nil || ext != "png" || f != "png" || w != imageEdge || h != imageEdge/2 {
		t.Fatalf("a wide screenshot: %dx%d %s %s, %v", w, h, f, ext, err)
	}
	// Noise compresses badly: as PNG it stays too big, so it goes as JPEG.
	noise := image.NewNRGBA(image.Rect(0, 0, 1100, 1100))
	rand.NewChaCha8([32]byte{}).Read(noise.Pix)
	for i := 3; i < len(noise.Pix); i += 4 {
		noise.Pix[i] = 0 // and transparent: JPEG has none, so it shows on white
	}
	var big bytes.Buffer
	png.Encode(&big, noise)
	if big.Len() <= imageBytes {
		t.Fatalf("test image only %d bytes", big.Len())
	}
	b, ext, err = fitImage(big.Bytes())
	if err != nil || ext != "jpeg" || len(b) > imageBytes {
		t.Fatalf("a big PNG: %s, %d bytes, %v", ext, len(b), err)
	}
	img, _ := jpeg.Decode(bytes.NewReader(b))
	if r, g, bl, _ := img.At(10, 10).RGBA(); r < 0xe000 || g < 0xe000 || bl < 0xe000 {
		t.Fatalf("transparency is not white: %v", img.At(10, 10))
	}
	if _, _, err := fitImage(pngHeader(30000, 30000)); err == nil || !strings.Contains(err.Error(), "too big") {
		t.Fatalf("a 900-megapixel image: %v", err)
	}
	if _, _, err := fitImage([]byte("not an image")); err == nil {
		t.Fatal("text taken for an image")
	}
}

// A photo's EXIF orientation is applied: the model sees it upright, as the
// phone showed it, whatever reader it has.
func TestOrientation(t *testing.T) {
	t.Parallel()
	img := image.NewRGBA(image.Rect(0, 0, 30, 20))
	for i := range img.Pix {
		img.Pix[i] = 0xff // white
	}
	img.Set(0, 0, color.RGBA{255, 0, 0, 255}) // red at the top left, as stored
	for y := range 4 {
		for x := range 4 {
			img.Set(x, y, color.RGBA{255, 0, 0, 255})
		}
	}
	var raw bytes.Buffer
	jpeg.Encode(&raw, img, &jpeg.Options{Quality: 95})
	for _, tc := range []struct {
		o          uint16
		w, h, x, y int // the red corner, shown
	}{{1, 30, 20, 0, 0}, {3, 30, 20, 29, 19}, {6, 20, 30, 19, 0}, {8, 20, 30, 0, 29}} {
		b := withOrientation(raw.Bytes(), tc.o)
		if got := orientation(b); got != int(tc.o) {
			t.Fatalf("orientation %d read as %d", tc.o, got)
		}
		fit, _, err := fitImage(b)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := image.Decode(bytes.NewReader(fit))
		if err != nil {
			t.Fatal(err)
		}
		if w, h := out.Bounds().Dx(), out.Bounds().Dy(); w != tc.w || h != tc.h {
			t.Fatalf("orientation %d: %dx%d", tc.o, w, h)
		}
		if r, g, _, _ := out.At(tc.x, tc.y).RGBA(); r < 0xc000 || g > 0x6000 {
			t.Fatalf("orientation %d: no red at %d,%d", tc.o, tc.x, tc.y)
		}
	}
	if orientation([]byte("\xff\xd8\xff")) != 1 || orientation(nil) != 1 {
		t.Fatal("garbage has an orientation")
	}
}

// withOrientation puts an EXIF block with orientation o into a JPEG.
func withOrientation(b []byte, o uint16) []byte {
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08\x00\x01")
	tiff = append(tiff, 0x01, 0x12, 0x00, 0x03, 0, 0, 0, 1)
	tiff = binary.BigEndian.AppendUint16(tiff, o)
	tiff = append(tiff, 0, 0, 0, 0, 0, 0)
	seg := append([]byte("Exif\x00\x00"), tiff...)
	out := append([]byte{0xff, 0xd8, 0xff, 0xe1}, byte((len(seg)+2)>>8), byte(len(seg)+2))
	out = append(out, seg...)
	return append(out, b[2:]...)
}

// pngHeader is the start of a PNG of w x h pixels: enough to read its size.
func pngHeader(w, h uint32) []byte {
	chunk := []byte("IHDR")
	chunk = binary.BigEndian.AppendUint32(chunk, w)
	chunk = binary.BigEndian.AppendUint32(chunk, h)
	chunk = append(chunk, 8, 0, 0, 0, 0) // 8-bit gray
	b := []byte("\x89PNG\r\n\x1a\n")
	b = binary.BigEndian.AppendUint32(b, 13)
	b = append(b, chunk...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(chunk))
}

// What a file is, from its content: the name only gives the extension.
func TestSaveClassify(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	for _, f := range []struct {
		name string
		body []byte
		kind string
		ext  string
	}{
		{"Shot.PNG", pngBytes(8, 8), "image", ".png"},
		{"wrongly.jpg", pngBytes(8, 8), "image", ".png"}, // content wins
		{"report.pdf", []byte("%PDF-1.4\n%fake"), "pdf", ".pdf"},
		{"notes.txt", []byte("hello\nworld\n"), "text", ".txt"},
		{"main.go", []byte("package main\n"), "text", ".go"},
		{"blob.bin", []byte("ab\x00cd"), "file", ".bin"},
		{"latin1.txt", []byte("caf\xe9"), "file", ".txt"},
		{"no-ext", []byte("plain"), "text", ""},
		{"odd.ex t!", []byte("plain"), "text", ""},
		{"scan.png", append(pngHeader(20000, 20000), make([]byte, 1<<20)...), "file", ".png"}, // never read whole
	} {
		at, err := c.save(bytes.NewReader(f.body), f.name)
		if err != nil || at.Kind != f.kind || filepath.Ext(at.Name) != f.ext || at.Orig != f.name || !safeName.MatchString(at.Name) {
			t.Errorf("%s: %+v, %v", f.name, at, err)
		}
	}
	// An image made to fit is kept alone: what came is gone.
	before, _ := os.ReadDir(filepath.Join(c.dir, "files"))
	big, err := c.save(bytes.NewReader(pngBytes(3000, 1500)), "big.png")
	after, _ := os.ReadDir(filepath.Join(c.dir, "files"))
	if err != nil || len(after) != len(before)+1 {
		t.Fatalf("%+v, %v: %d files, then %d", big, err, len(before), len(after))
	}
	// The same bytes in the log already, as a tool's image: they stay.
	shared, _ := c.saveImage(pngBytes(2500, 1500))
	if at, err := c.save(bytes.NewReader(pngBytes(2500, 1500)), "again.png"); err != nil || at.Name == shared {
		t.Fatalf("again: %+v, %v", at, err)
	}
	if _, err := os.Stat(filepath.Join(c.dir, shared)); err != nil {
		t.Fatalf("a file in the log was removed: %v", err)
	}
	if fits(image.Config{ColorModel: color.NRGBA64Model, Width: 8000, Height: 8000}) == nil {
		t.Fatal("a 16-bit 64-megapixel image would be decoded")
	}
	// A page's attachment is checked against the file, never trusted.
	img, _ := c.save(bytes.NewReader(pngBytes(8, 8)), "a.png")
	got, err := c.uploaded([]attachment{{Name: img.Name, Orig: "a.png", Kind: "text"}})
	if err != nil || got[0].Kind != "image" {
		t.Fatalf("uploaded: %+v, %v", got, err)
	}
	for _, bad := range []string{"files/../web.json", "web.json", "files/0123456789abcdef0123456789abcdef.png"} {
		if _, err := c.uploaded([]attachment{{Name: bad}}); err == nil {
			t.Errorf("uploaded took %q", bad)
		}
	}
}

// HEIC, as iPhones and macOS take photos, becomes a JPEG (with macOS's sips).
func TestHEIC(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sips"); err != nil {
		t.Skip("no sips")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.png")
	os.WriteFile(src, pngBytes(64, 48), 0o600)
	heic := filepath.Join(dir, "photo.heic")
	if out, err := exec.Command("sips", "-s", "format", "heic", src, "--out", heic).CombinedOutput(); err != nil {
		t.Skipf("sips writes no HEIC here: %s", out)
	}
	b, _ := os.ReadFile(heic)
	if !isHEIC(b[:12]) {
		t.Fatalf("not taken for HEIC: %q", b[:12])
	}
	c, _ := fakeChat(t, t.TempDir(), 100)
	at, err := c.save(bytes.NewReader(b), "photo.heic")
	if err != nil || at.Kind != "image" || filepath.Ext(at.Name) != ".jpeg" {
		t.Fatalf("HEIC: %+v, %v", at, err)
	}
	jb, _ := os.ReadFile(filepath.Join(c.dir, at.Name))
	if w, h, f := decodeSize(t, jb); w != 64 || h != 48 || f != "jpeg" {
		t.Fatalf("converted: %dx%d %s", w, h, f)
	}
}

// A message with attachments: images and PDFs as content, text inlined
// within its budget, the rest as paths; the marks come off for the input's
// history.
func TestCompose(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	save := func(name string, body []byte) attachment {
		at, err := c.save(bytes.NewReader(body), name)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	img := save(`my "shot" <1>.png`, pngBytes(4, 4))
	pdf := save("r.pdf", []byte("%PDF-1.4\nx"))
	txt := save("a.txt", []byte("line one\nline two\n"))
	huge := save("huge.txt", []byte(strings.Repeat("z", inlineMax+1)))
	zip := save("b.zip", []byte("PK\x03\x04\x00\x00"))
	m := c.compose("look", []attachment{img, pdf, txt, huge, zip})
	if !slices.Equal(m.images, []string{img.Name, pdf.Name}) || !slices.Equal(m.files, []string{img.Orig, "r.pdf", "a.txt", "huge.txt", "b.zip"}) {
		t.Fatalf("images %q, files %q", m.images, m.files)
	}
	full := m.full()
	for _, want := range []string{
		"look\n\n<attachment name=\"my &#34;shot&#34; &lt;1&gt;.png\" path=\"" + filepath.Join(c.dir, img.Name) + "\" size=\"",
		"<attachment name=\"a.txt\" path=\"" + filepath.Join(c.dir, txt.Name) + "\" size=\"18 B\">\nline one\nline two\n</attachment>",
		"<attachment name=\"huge.txt\" path=\"" + filepath.Join(c.dir, huge.Name) + "\" size=\"25.0 KB\"/>",
		"<attachment name=\"b.zip\" path=\"" + filepath.Join(c.dir, zip.Name) + "\" size=\"6 B\"/>",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("message lacks %q:\n%s", want, full)
		}
	}
	if got := stripAttachments(full); got != "look" {
		t.Fatalf("stripAttachments = %q", got)
	}
	if got := c.compose("", []attachment{zip}).full(); !strings.HasPrefix(got, "<attachment") {
		t.Fatalf("attachments alone: %q", got)
	}
	// A typed path is saved like an upload.
	p := filepath.Join(t.TempDir(), "typed.txt")
	os.WriteFile(p, []byte("typed"), 0o600)
	atts, err := c.attachPaths([]string{p, t.TempDir()})
	if err == nil || len(atts) != 1 || atts[0].Kind != "text" || atts[0].Orig != "typed.txt" {
		t.Fatalf("attachPaths: %+v, %v", atts, err)
	}
	if b := c.fileBlocks([]string{pdf.Name, img.Name}); b[0].Type != "document" || b[0].Source.MediaType != "application/pdf" || b[1].Type != "image" {
		t.Fatalf("fileBlocks: %v %v", b[0].Type, b[1].Type)
	}
}

// Marks go where their attachments were put, and the images after their
// marks: "do this <img> not this <img>" reaches the model as written.
func TestPlaced(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	one, _ := c.saveImage([]byte(pngHead))
	two, _ := c.saveImage([]byte(pngHead + "x"))
	atts := []attachment{
		{Name: one, Orig: "one.png", Kind: "image", Size: 1, At: "[Image #1]"},
		{Name: two, Orig: "two.png", Kind: "image", Size: 1, At: "[Image #2]"},
		{Name: one, Orig: "again.png", Kind: "image", Size: 1}, // no place: at the end
	}
	m := c.compose("do this [Image #2] not this [Image #1] ok", atts)
	full := m.full()
	mark := func(name, file string) string {
		return `<attachment name="` + name + `" path="` + filepath.Join(c.dir, file) + `" size="1 B"/>`
	}
	if want := "do this " + mark("two.png", two) + " not this " + mark("one.png", one) + " ok\n\n" + mark("again.png", one); full != want {
		t.Fatalf("full:\n%s\nwant:\n%s", full, want)
	}
	var got []string
	for _, b := range c.content(m) {
		if b.Type == "text" {
			got = append(got, b.Text)
		} else {
			got = append(got, b.Type+":"+b.file)
		}
	}
	want := []string{"do this " + mark("two.png", two), "image:" + two, " not this " + mark("one.png", one), "image:" + one,
		" ok\n\n" + mark("again.png", one), "image:" + one}
	if !slices.Equal(got, want) {
		t.Fatalf("content:\n%q\nwant:\n%q", got, want)
	}
	// An edit that takes a placeholder out moves its mark to the end.
	m.text = "just this [Image #1]"
	if full := m.full(); full != "just this "+mark("one.png", one)+"\n\n"+mark("two.png", two)+"\n"+mark("again.png", one) {
		t.Fatalf("after the edit: %s", full)
	}
}

// A path's mark goes after the whole word naming it: never into a longer
// path, a link, quotes or a block of code.
func TestPlacePaths(t *testing.T) {
	t.Parallel()
	home, _ := os.UserHomeDir()
	for _, c := range []struct {
		text  string
		marks []placed
		want  string
	}{
		{"see /tmp/a.txt and /tmp/a", []placed{{path: "/tmp/a", mark: "<A>"}, {path: "/tmp/a.txt", mark: "<B>"}},
			"see /tmp/a.txt <B> and /tmp/a <A>"},
		{"look at `~/x.png` here", []placed{{path: home + "/x.png", mark: "<A>"}}, "look at `~/x.png` <A> here"},
		{"[it](/tmp/x.png), then", []placed{{path: "/tmp/x.png", mark: "<A>"}}, "[it](/tmp/x.png), <A> then"},
		{"'/tmp/my x.png' ok", []placed{{path: "/tmp/my x.png", mark: "<A>"}}, "'/tmp/my x.png' <A> ok"},
		{`@~/My\ File.png ok`, []placed{{path: home + "/My File.png", mark: "<A>"}}, `@~/My\ File.png <A> ok`},
		{"```\ncat /tmp/x.png\n```\nlook at /tmp/x.png", []placed{{path: "/tmp/x.png", mark: "<A>"}},
			"```\ncat /tmp/x.png\n```\nlook at /tmp/x.png <A>"},
		{"a [Image #1] b [Image #1]", []placed{{at: "[Image #1]", mark: "<A>"}, {at: "[Image #1]", mark: "<B>"}}, "a <A> b <B>"},
	} {
		got, left := place(c.text, c.marks)
		if got != c.want || len(left) > 0 {
			t.Errorf("place(%q) = %q, left %q; want %q", c.text, got, left, c.want)
		}
	}
}
