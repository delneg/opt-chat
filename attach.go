package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Attachments are files the user sends with a message: dropped, pasted or
// picked in a page (uploaded), or named by a path typed in it. Each is kept
// in files/, named by its content, and the message gets a mark for it:
//
//	<attachment name="shot.png" path="/…/files/<hash>.png" size="1.2 MB"/>
//
// Images and PDFs also go to the model as content: they are the message's
// images. A text file's mark holds its text, up to a budget; any other file
// is a path the tools can read.

const (
	imageEdge  = 2000      // pixels: the longest side an image keeps; the API's limit with many images
	imageBytes = 3_750_000 // and the most bytes, 5 MB in base64
	pdfBytes   = 20 << 20  // a bigger PDF goes as a path: a request holds 32 MB, and base64 adds a third
	imageMem   = 400 << 20 // bytes an image takes decoded: a bigger one is a file, never decoded
	imageFile  = 200 << 20 // bytes: a bigger image file is a file, never read whole
	textMax    = 2 << 20   // a bigger file is never read as text
	inlineMax  = 25_000    // characters of text files one message holds, with its own text
	uploadMax  = 2 << 30
)

type attachment struct {
	Name string `json:"name"` // files/<hash>.<ext>
	Orig string `json:"orig"` // the file's own name
	Kind string `json:"kind"` // image, pdf, text or file
	Size int64  `json:"size"`
	At   string `json:"at,omitzero"` // its place in the message: the placeholder the page put there, as "[Image #1]"

	path string // typed: the path it was named by, which its mark follows
}

var (
	safeName = regexp.MustCompile(`^files/[0-9a-f]{32}(\.[a-z0-9]{1,10})?$`)
	extOK    = regexp.MustCompile(`^[a-z0-9]{1,10}$`)
	attMark  = regexp.MustCompile(`(?s)\n*<attachment name="[^"]*" path="[^"]*" size="[^"]*"(?:/>|>\n.*?\n</attachment>)`)
)

// save stores a file from r, named orig, processed for the model: an image
// made to fit, a HEIC photo turned into a JPEG.
func (c *Chat) save(r io.Reader, orig string) (attachment, error) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(orig), "."))
	if !extOK.MatchString(ext) {
		ext = ""
	}
	tmp, name, size, err := c.spool(io.LimitReader(r, uploadMax+1), ext)
	if err != nil {
		return attachment{}, err
	}
	defer os.Remove(tmp) // unless kept by its name
	if size > uploadMax {
		return attachment{}, fmt.Errorf("%s is over %s", orig, humanSize(uploadMax))
	}
	at := attachment{Name: name, Orig: filepath.Base(orig), Size: size}
	if at.Kind, err = c.classify(&at, tmp); err != nil {
		return attachment{}, err
	}
	if at.Name == name { // as it came; an image made to fit is kept as such alone
		err = c.keep(tmp, name)
	}
	return at, err
}

// classify finds what at is, its content at path, turning an image into one
// the model takes when it must; at.Name and at.Size follow.
func (c *Chat) classify(at *attachment, path string) (string, error) {
	head := make([]byte, 512)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	n, _ := io.ReadFull(f, head)
	f.Close()
	head = head[:n]
	mime := http.DetectContentType(head)
	switch {
	case isHEIC(head) && at.Size <= imageFile:
		b, err := heicToJPEG(path)
		if err != nil {
			return "file", nil // kept as a file, which the tools may still read
		}
		return c.saveImageFit(at, b)
	case slices.Contains([]string{"image/png", "image/jpeg", "image/gif", "image/webp"}, mime):
		if at.Size > imageFile || !decodable(path) {
			return "file", nil // too big to shrink: decoded, it could take gigabytes
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return c.saveImageFit(at, b)
	case bytes.HasPrefix(head, []byte("%PDF-")) && at.Size <= pdfBytes:
		return "pdf", nil
	case at.Size <= textMax && !bytes.Contains(head, []byte{0}):
		b, err := os.ReadFile(path)
		if err == nil && utf8.Valid(b) {
			return "text", nil
		}
	}
	return "file", nil
}

// decodable reports whether the image at path is one fitImage takes: its
// header read alone.
func decodable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	return err == nil && fits(cfg) == nil
}

// fits says whether an image decodes in imageMem.
func fits(cfg image.Config) error {
	per := 4 // bytes a pixel: RGBA, CMYK; YCbCr, as a JPEG decodes, less
	switch cfg.ColorModel {
	case color.RGBA64Model, color.NRGBA64Model:
		per = 8
	case color.Gray16Model:
		per = 2
	case color.GrayModel:
		per = 1
	}
	if int64(cfg.Width)*int64(cfg.Height)*int64(per) > imageMem {
		return fmt.Errorf("%dx%d pixels: too big to shrink", cfg.Width, cfg.Height)
	}
	return nil
}

// fitting lets two images at a time be decoded: each can take imageMem.
var fitting = make(chan struct{}, 2)

// saveImageFit stores the image b made to fit, as at.
func (c *Chat) saveImageFit(at *attachment, b []byte) (string, error) {
	fitting <- struct{}{}
	fit, ext, err := fitImage(b)
	<-fitting
	if err != nil {
		return "file", nil // not an image after all
	}
	name, size, err := c.saveFile(bytes.NewReader(fit), ext)
	if err != nil {
		return "", err
	}
	at.Name, at.Size = name, size
	return "image", nil
}

// isHEIC reads an ISO-BMFF file's brand: HEIC and HEIF, as iPhones take them.
func isHEIC(head []byte) bool {
	if len(head) < 12 || string(head[4:8]) != "ftyp" {
		return false
	}
	return slices.Contains([]string{"heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1"}, string(head[8:12]))
}

// heicToJPEG converts with macOS's sips: Go reads no HEIC.
func heicToJPEG(path string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "opt-heic-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "out.jpg")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if b, err := exec.CommandContext(ctx, "sips", "-s", "format", "jpeg", path, "--out", out).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sips: %v: %s", err, lastLine(string(b)))
	}
	return os.ReadFile(out)
}

// fitImage returns b, or b made to fit the API: at most imageEdge pixels a
// side and imageBytes, as PNG or JPEG, and upright: a photo's EXIF
// orientation is applied, as only some readers do.
func fitImage(b []byte) ([]byte, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, "", err
	}
	if err := fits(cfg); err != nil {
		return nil, "", err
	}
	turn := 1
	if format == "jpeg" {
		turn = orientation(b)
	}
	if cfg.Width <= imageEdge && cfg.Height <= imageEdge && len(b) <= imageBytes && turn == 1 {
		return b, map[string]string{"jpeg": "jpeg", "png": "png", "gif": "gif", "webp": "webp"}[format], nil
	}
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, "", err
	}
	for edge := imageEdge; ; edge = edge * 3 / 4 {
		img := orient(scale(src, edge), turn)
		var out bytes.Buffer
		if format == "png" || format == "gif" {
			if err := png.Encode(&out, img); err == nil && out.Len() <= imageBytes {
				return out.Bytes(), "png", nil
			}
		}
		for _, q := range []int{85, 70} {
			out.Reset()
			if err := jpeg.Encode(&out, onWhite(img), &jpeg.Options{Quality: q}); err != nil {
				return nil, "", err
			}
			if out.Len() <= imageBytes || edge < 200 {
				return out.Bytes(), "jpeg", nil
			}
		}
	}
}

// scale shrinks img so its longest side is at most edge.
func scale(img image.Image, edge int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= edge && h <= edge {
		return img
	}
	if w >= h {
		w, h = edge, max(1, h*edge/w)
	} else {
		w, h = max(1, w*edge/h), edge
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	return dst
}

// orientation is a JPEG's EXIF orientation, 1 to 8; 1, as it is, when it
// has none.
func orientation(b []byte) int {
	for i := 2; i+4 <= len(b) && b[i] == 0xff; {
		marker, n := b[i+1], int(b[i+2])<<8|int(b[i+3])
		if marker == 0xda || n < 2 || i+2+n > len(b) { // the image data: no EXIF before it
			break
		}
		seg := b[i+4 : i+2+n]
		if marker == 0xe1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + n
	}
	return 1
}

func tiffOrientation(t []byte) int {
	var order binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	ifd := int(order.Uint32(t[4:8]))
	if ifd < 8 || ifd+2 > len(t) {
		return 1
	}
	for e, n := ifd+2, int(order.Uint16(t[ifd:])); n > 0 && e+12 <= len(t); e, n = e+12, n-1 {
		if order.Uint16(t[e:]) == 0x0112 { // Orientation, a SHORT
			if o := int(order.Uint16(t[e+8:])); o >= 1 && o <= 8 {
				return o
			}
			return 1
		}
	}
	return 1
}

// orient turns img as EXIF orientation o says to show it: 2 to 8 flip or
// turn it (6 a quarter turn clockwise, as a phone held upright takes it).
func orient(img image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return img
	}
	src := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
	draw.Draw(src, src.Bounds(), img, img.Bounds().Min, draw.Src)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	if o >= 5 { // a quarter turn: the sides swap
		w, h = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, w-1-x
			case 7:
				sx, sy = h-1-y, w-1-x
			case 8:
				sx, sy = h-1-y, x
			}
			copy(dst.Pix[dst.PixOffset(x, y):][:4], src.Pix[src.PixOffset(sx, sy):][:4])
		}
	}
	return dst
}

// onWhite flattens transparency, which JPEG lacks, onto white.
func onWhite(img image.Image) image.Image {
	dst := image.NewRGBA(img.Bounds())
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Over)
	return dst
}

// attachPaths saves the files at paths, as typed in a message.
func (c *Chat) attachPaths(paths []string) ([]attachment, error) {
	var atts []attachment
	var errs []error
	for _, p := range paths {
		at, err := func() (attachment, error) {
			f, err := os.Open(p)
			if err != nil {
				return attachment{}, err
			}
			defer f.Close()
			if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
				return attachment{}, fmt.Errorf("%s is not a file", p)
			}
			return c.save(f, p)
		}()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		at.path = p
		atts = append(atts, at)
	}
	return atts, errors.Join(errs...)
}

// uploaded checks attachments a page names, as it got them from /upload,
// against the files: a page names only files it uploaded.
func (c *Chat) uploaded(atts []attachment) ([]attachment, error) {
	var out []attachment
	for _, at := range atts {
		if !safeName.MatchString(at.Name) {
			return nil, fmt.Errorf("no attachment %q", at.Name)
		}
		fi, err := os.Stat(filepath.Join(c.dir, at.Name))
		if err != nil {
			return nil, fmt.Errorf("attachment %s is gone", at.Orig)
		}
		at.Size, at.Orig = fi.Size(), filepath.Base(cmp.Or(at.Orig, at.Name))
		if at.Kind, err = c.classify(&at, filepath.Join(c.dir, at.Name)); err != nil {
			return nil, err
		}
		out = append(out, at)
	}
	return out, nil
}

// compose makes the user's message: their text, and a mark for each
// attachment, where it was put: in place of its placeholder, or after the
// path that named it; at the end when it has no place.
func (c *Chat) compose(text string, atts []attachment) message {
	m := message{kind: "user", text: text}
	budget := inlineMax - utf8.RuneCountInString(text)
	var marks []string
	for _, at := range atts {
		path := filepath.Join(c.dir, at.Name)
		mark := fmt.Sprintf(`<attachment name="%s" path="%s" size="%s"`, attr(at.Orig), attr(path), humanSize(at.Size))
		m.files = append(m.files, at.Orig)
		switch at.Kind {
		case "image", "pdf":
			m.images = append(m.images, at.Name)
			mark += "/>"
		case "text":
			b, err := os.ReadFile(path)
			s := strings.TrimSuffix(string(b), "\n")
			if n := utf8.RuneCountInString(s); err == nil && n <= budget && !strings.Contains(s, "</attachment>") {
				budget -= n
				mark += ">\n" + s + "\n</attachment>"
			} else {
				mark += "/>"
			}
		default:
			if words, ok := transcript(path); ok { // a voice message: its words
				mark += ">\n" + words + "\n</attachment>"
			} else {
				mark += "/>"
			}
		}
		switch {
		case at.At != "":
			m.inline = append(m.inline, placed{at: at.At, mark: mark})
		case at.path != "":
			m.inline = append(m.inline, placed{path: at.path, mark: mark})
		default:
			marks = append(marks, mark)
		}
	}
	m.extra = strings.Join(marks, "\n")
	return m
}

// placed is an attachment's mark with its place in the message's text.
type placed struct {
	at   string // a placeholder, as "[Image #1]": the mark replaces it
	path string // or the file's path: the mark goes after it, where the text names it
	mark string
}

// place puts each mark in text, where its placeholder or path first is;
// the marks with no place found are returned, in order. A path is a whole
// word of the text, as filePaths reads them, quotes and brackets and all,
// not in a block of code.
func place(text string, marks []placed) (string, []string) {
	type hit struct {
		i, j int // the placeholder; or i == j, after the path
		mark string
	}
	var hits []hit
	taken := func(i, j int) bool {
		return slices.ContainsFunc(hits, func(h hit) bool { return i < h.j && h.i < j || i == j && i == h.i })
	}
	var words [][]int // the text's words out of blocks of code, as filePaths reads them
	if slices.ContainsFunc(marks, func(p placed) bool { return p.path != "" }) {
		code := fenced.FindAllStringIndex(text, -1)
		for _, w := range pathToken.FindAllStringIndex(text, -1) {
			if !slices.ContainsFunc(code, func(c []int) bool { return w[0] < c[1] && c[0] < w[1] }) {
				words = append(words, w)
			}
		}
	}
	home, _ := os.UserHomeDir()
	var left []string
	for _, p := range marks {
		i, j := -1, -1
		if p.at != "" {
			for from := 0; ; {
				k := strings.Index(text[from:], p.at)
				if k < 0 {
					break
				}
				if k += from; !taken(k, k+len(p.at)) {
					i, j = k, k+len(p.at)
					break
				}
				from = k + 1
			}
		} else {
			for _, w := range words {
				if tokenPath(text[w[0]:w[1]], home) == p.path && !taken(w[1], w[1]) {
					i, j = w[1], w[1]
					break
				}
			}
		}
		if i < 0 {
			left = append(left, p.mark)
			continue
		}
		hits = append(hits, hit{i, j, p.mark})
	}
	slices.SortFunc(hits, func(a, b hit) int { return a.i - b.i })
	var out strings.Builder
	prev := 0
	for _, h := range hits {
		out.WriteString(text[prev:h.i])
		if h.i == h.j { // after a path
			out.WriteString(" ")
		}
		out.WriteString(h.mark)
		prev = h.j
	}
	out.WriteString(text[prev:])
	return out.String(), left
}

// stripAttachments is a message's text without its attachments' marks.
func stripAttachments(s string) string { return strings.TrimSpace(attMark.ReplaceAllString(s, "")) }

var attFrame = regexp.MustCompile(`<attachment name="[^"]*" path="[^"]*" size="[^"]*"/?>|</attachment>`)

// unframed is s with its attachments' marks taken off and what they hold
// kept: a voice message's words, a text file inlined.
func unframed(s string) string { return attFrame.ReplaceAllString(s, " ") }

func attr(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(html.EscapeString(s))
}

func humanSize(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d B", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1f KB", float64(n)/1e3)
	case n < 1_000_000_000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
}

var (
	pathToken = regexp.MustCompile(`'[^']+'|"[^"]+"|(?:\\.|[^\s\p{Zs}'"\\])+`) // as a terminal drops a path: quoted, or with escaped spaces
	escaped   = regexp.MustCompile(`\\(.)`)
	fenced    = regexp.MustCompile("(?ms)^ {0,3}```.*?^ {0,3}```|^ {0,3}~~~.*?^ {0,3}~~~")
)

// hidden is a path in a hidden folder or to a hidden file (~/.ssh,
// ~/.zshrc): naming one does not send it; the page asks first.
func hidden(p string) bool { return strings.Contains(p, "/.") }

// pathsMax is the most files a message attaches by path: more are a paste,
// a log or a listing, not files to send.
const pathsMax = 10

// filePaths finds the files a message names by path: absolute, or from ~/;
// in its words, `code` or a [link](/path), not in a block of code.
func filePaths(text string) []string {
	home, _ := os.UserHomeDir()
	return filePathsFrom(text, home)
}

// filePathsFrom is filePaths, with ~ the folder home.
func filePathsFrom(text, home string) []string {
	var out []string
	for _, tok := range pathToken.FindAllString(fenced.ReplaceAllString(text, ""), -1) {
		p := tokenPath(tok, home)
		if !filepath.IsAbs(p) || slices.Contains(out, p) {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Size() <= uploadMax {
			out = append(out, p)
		}
	}
	if len(out) > pathsMax {
		return nil
	}
	return out
}

// tokenPath is the path a word of a message names, if any: the word
// unquoted, out of a markdown link, brackets or a sentence's end, and from
// ~/ made absolute.
func tokenPath(tok, home string) string {
	p := escaped.ReplaceAllString(tok, "$1")
	if i := strings.LastIndex(p, "]("); i >= 0 { // a markdown link
		p = p[i+2:]
	}
	p = strings.TrimLeft(p, "@([{<`")       // @: a file mentioned, as the page's @ completes it
	p = strings.TrimRight(p, ".,;:!?)]}>`") // a path ending a sentence, or in brackets
	if tok[0] == '\'' || tok[0] == '"' {
		p = tok[1 : len(tok)-1]
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		p = filepath.Join(home, rest)
	}
	return p
}
