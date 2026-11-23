package extension_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nexora/internal/extension"
)

// identify asks ImageMagick (an independent decoder) for format and size.
func identify(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("magick", "identify", "-format", "%m %wx%h", path).Output()
	if err != nil {
		t.Fatalf("ImageMagick can't read %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

func TestImagesConvert(t *testing.T) {
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("ImageMagick not installed: needed to create HEIC/WebP/AVIF fixtures")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	must(t, os.MkdirAll(in, 0o755))
	must(t, os.WriteFile(filepath.Join(in, "photo.png"), pngBytes(t, 120, 80), 0o644))
	for _, ext := range []string{"heic", "webp", "avif"} {
		if out, err := exec.Command("magick", filepath.Join(in, "photo.png"), filepath.Join(dir, "src."+ext)).CombinedOutput(); err != nil {
			t.Fatalf("making a %s fixture: %v %s", ext, err, out)
		}
		must(t, os.Rename(filepath.Join(dir, "src."+ext), filepath.Join(in, "shot-"+ext+"."+ext)))
	}
	// A logo with a transparent background.
	logo := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	for y := 10; y < 30; y++ {
		for x := 10; x < 30; x++ {
			logo.Set(x, y, color.NRGBA{200, 30, 30, 255})
		}
	}
	var lb bytes.Buffer
	must(t, png.Encode(&lb, logo))
	must(t, os.WriteFile(filepath.Join(dir, "logo.png"), lb.Bytes(), 0o644))
	must(t, os.WriteFile(filepath.Join(in, "broken.webp"), []byte("RIFF\x00\x00\x00\x00WEBPjunk"), 0o644))

	im := builtin(t, "images", extension.Values{"folders": []any{dir}})
	var res struct {
		Converted []struct{ From, To string }
		Skipped   []struct{ Path, Reason string }
	}

	// Folder -> JPEG: PNG, HEIC, WebP and AVIF all convert; the broken file is skipped.
	must(t, json.Unmarshal(im.do("convert", obj{"path": "in", "to": "jpeg", "outDir": "out"}), &res))
	if len(res.Converted) != 4 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Path, "broken.webp") {
		t.Fatalf("convert folder: %+v", res)
	}
	for _, name := range []string{"photo.jpg", "shot-heic.jpg", "shot-webp.jpg", "shot-avif.jpg"} {
		f, err := os.Open(filepath.Join(dir, "out", name))
		must(t, err)
		img, err := jpeg.Decode(f)
		f.Close()
		if err != nil || img.Bounds().Dx() != 120 || img.Bounds().Dy() != 80 {
			t.Fatalf("%s: %v %v", name, err, img.Bounds())
		}
	}

	// PNG -> WebP and AVIF, checked by ImageMagick.
	im.do("convert", obj{"path": "in/photo.png", "to": "webp"})
	if got := identify(t, filepath.Join(in, "photo.webp")); got != "WEBP 120x80" {
		t.Fatalf("webp output: %s", got)
	}
	im.do("convert", obj{"path": "in/photo.png", "to": "avif", "quality": 60})
	if got := identify(t, filepath.Join(in, "photo.avif")); got != "AVIF 120x80" {
		t.Fatalf("avif output: %s", got)
	}

	// Transparency becomes white in JPEG (not black).
	im.do("convert", obj{"path": "logo.png", "to": "jpeg"})
	f, err := os.Open(filepath.Join(dir, "logo.jpg"))
	must(t, err)
	j, err := jpeg.Decode(f)
	f.Close()
	must(t, err)
	if r, g, b, _ := j.At(2, 2).RGBA(); r>>8 < 245 || g>>8 < 245 || b>>8 < 245 {
		t.Fatalf("transparent corner should be white, got %d,%d,%d", r>>8, g>>8, b>>8)
	}

	// Collisions get a numbered name; same-format inputs are skipped unless resizing.
	must(t, json.Unmarshal(im.do("convert", obj{"path": "logo.png", "to": "jpeg"}), &res))
	if len(res.Converted) != 1 || !strings.HasSuffix(res.Converted[0].To, "logo-2.jpg") {
		t.Fatalf("collision: %+v", res)
	}
	must(t, json.Unmarshal(im.do("convert", obj{"path": "logo.jpg", "to": "jpeg"}), &res))
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != "already jpeg" {
		t.Fatalf("same format: %+v", res)
	}
	if out := string(im.do("convert", obj{"path": "in/shot-heic.heic", "to": "png", "maxSize": 60, "outDir": "small"})); !strings.Contains(out, "shot-heic.png") {
		t.Fatalf("heic -> png: %s", out)
	}
	if got := identify(t, filepath.Join(dir, "small", "shot-heic.png")); got != "PNG 60x40" {
		t.Fatalf("maxSize: %s", got)
	}

	// HEIC can be edited directly (saved as JPEG), and info reads it.
	if out := string(im.do("edit", obj{"path": "in/shot-heic.heic", "ops": []obj{{"op": "greyscale"}}})); !strings.Contains(out, "shot-heic-edited.jpg") {
		t.Fatalf("edit heic: %s", out)
	}
	if out := string(im.do("info", obj{"path": "in/shot-heic.heic"})); !strings.Contains(out, `"format":"heic"`) || !strings.Contains(out, `"width":120`) {
		t.Fatalf("info heic: %s", out)
	}
	im.bad("outside the trusted folders", "convert", obj{"path": "/etc", "to": "png"})
}
