package main

import (
	"bytes"
	"cmp"
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
	imagePix   = 80e6      // pixels: a bigger image is a file, never decoded (4 bytes each)
	textMax    = 2 << 20   // a bigger file is never read as text
	inlineMax  = 25_000    // characters of text files one message holds, with its own text
	uploadMax  = 2 << 30
)

type attachment struct {
	Name string `json:"name"` // files/<hash>.<ext>
	Orig string `json:"orig"` // the file's own name
	Kind string `json:"kind"` // image, pdf, text or file
	Size int64  `json:"size"`
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
	name, size, err := c.saveFile(io.LimitReader(r, uploadMax+1), ext)
	if err != nil {
		return attachment{}, err
	}
	if size > uploadMax {
		os.Remove(filepath.Join(c.dir, name))
		return attachment{}, fmt.Errorf("%s is over %s", orig, humanSize(uploadMax))
	}
	at := attachment{Name: name, Orig: filepath.Base(orig), Size: size}
	at.Kind, err = c.classify(&at)
	return at, err
}

// classify finds what at is, turning an image into one the model takes when
// it must; at.Name and at.Size follow.
func (c *Chat) classify(at *attachment) (string, error) {
	path := filepath.Join(c.dir, at.Name)
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
	case isHEIC(head):
		b, err := heicToJPEG(path)
		if err != nil {
			return "file", nil // kept as a file, which the tools may still read
		}
		return c.saveImageFit(at, b)
	case slices.Contains([]string{"image/png", "image/jpeg", "image/gif", "image/webp"}, mime):
		if !decodable(path) {
			return "file", nil // too big to shrink: read whole, it could take gigabytes
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
	return err == nil && cfg.Width*cfg.Height <= imagePix
}

func (c *Chat) saveImageFit(at *attachment, b []byte) (string, error) {
	fit, ext, err := fitImage(b)
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
	if b, err := exec.Command("sips", "-s", "format", "jpeg", path, "--out", out).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sips: %v: %s", err, lastLine(string(b)))
	}
	return os.ReadFile(out)
}

// fitImage returns b, or b made to fit the API: at most imageEdge pixels a
// side and imageBytes, as PNG or JPEG.
func fitImage(b []byte) ([]byte, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, "", err
	}
	if cfg.Width*cfg.Height > imagePix {
		return nil, "", fmt.Errorf("%dx%d pixels: too big to shrink", cfg.Width, cfg.Height)
	}
	if cfg.Width <= imageEdge && cfg.Height <= imageEdge && len(b) <= imageBytes {
		return b, map[string]string{"jpeg": "jpeg", "png": "png", "gif": "gif", "webp": "webp"}[format], nil
	}
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, "", err
	}
	for edge := imageEdge; ; edge = edge * 3 / 4 {
		img := scale(src, edge)
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
		if at.Kind, err = c.classify(&at); err != nil {
			return nil, err
		}
		out = append(out, at)
	}
	return out, nil
}

// compose makes the user's message: their text, and a mark for each attachment.
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
		case "text":
			b, err := os.ReadFile(path)
			s := strings.TrimSuffix(string(b), "\n")
			if n := utf8.RuneCountInString(s); err == nil && n <= budget && !strings.Contains(s, "</attachment>") {
				budget -= n
				marks = append(marks, mark+">\n"+s+"\n</attachment>")
				continue
			}
		}
		marks = append(marks, mark+"/>")
	}
	m.extra = strings.Join(marks, "\n")
	return m
}

// stripAttachments is a message's text without its attachments' marks.
func stripAttachments(s string) string { return strings.TrimSpace(attMark.ReplaceAllString(s, "")) }

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
	pathToken = regexp.MustCompile(`'[^']+'|"[^"]+"|(?:\\.|[^\s'"\\])+`) // as a terminal drops a path: quoted, or with escaped spaces
	escaped   = regexp.MustCompile(`\\(.)`)
)

// filePaths finds the files a message names by path: absolute, or from ~/.
func filePaths(text string) []string {
	var out []string
	home, _ := os.UserHomeDir()
	for _, tok := range pathToken.FindAllString(text, -1) {
		p := strings.TrimRight(escaped.ReplaceAllString(tok, "$1"), ".,;:!?)") // a path ending a sentence
		if tok[0] == '\'' || tok[0] == '"' {
			p = tok[1 : len(tok)-1]
		}
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = filepath.Join(home, rest)
		}
		if !filepath.IsAbs(p) || slices.Contains(out, p) {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Size() <= uploadMax {
			out = append(out, p)
		}
	}
	return out
}
