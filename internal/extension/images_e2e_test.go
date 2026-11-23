package extension_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nexora/internal/extension"
)

func testImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), 120, 255})
		}
	}
	return img
}

func pngBytes(t *testing.T, w, h int) []byte {
	var b bytes.Buffer
	must(t, png.Encode(&b, testImage(w, h)))
	return b.Bytes()
}

// tiffEntry is one IFD entry; data longer than 4 bytes is stored out of line.
type tiffEntry struct {
	tag, typ uint16
	count    uint32
	data     []byte
}

func ifdSize(es []tiffEntry) int {
	n := 2 + 12*len(es) + 4
	for _, e := range es {
		if len(e.data) > 4 {
			n += len(e.data) + len(e.data)%2
		}
	}
	return n
}

func encodeIFD(es []tiffEntry, start int) []byte {
	le := binary.LittleEndian
	var head, extra bytes.Buffer
	binary.Write(&head, le, uint16(len(es)))
	dataAt := start + 2 + 12*len(es) + 4
	for _, e := range es {
		binary.Write(&head, le, e.tag)
		binary.Write(&head, le, e.typ)
		binary.Write(&head, le, e.count)
		if len(e.data) <= 4 {
			v := make([]byte, 4)
			copy(v, e.data)
			head.Write(v)
		} else {
			binary.Write(&head, le, uint32(dataAt+extra.Len()))
			extra.Write(e.data)
			if len(e.data)%2 == 1 {
				extra.WriteByte(0)
			}
		}
	}
	binary.Write(&head, le, uint32(0)) // no next IFD
	return append(head.Bytes(), extra.Bytes()...)
}

func u32(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }

func rationals(vals ...[2]uint32) []byte {
	var b []byte
	for _, v := range vals {
		b = append(b, u32(v[0])...)
		b = append(b, u32(v[1])...)
	}
	return b
}

// photoWithExif is a JPEG with camera, date taken and a GPS location
// (New Delhi), like a phone photo.
func photoWithExif(t *testing.T, w, h int) []byte {
	var j bytes.Buffer
	must(t, jpeg.Encode(&j, testImage(w, h), &jpeg.Options{Quality: 90}))
	ascii := func(s string) []byte { return append([]byte(s), 0) }
	const ascii2, long, rational = 2, 4, 5

	exifIFD := []tiffEntry{{0x9003, ascii2, 20, ascii("2026:03:14 09:26:53")}}
	gpsIFD := []tiffEntry{
		{0x0001, ascii2, 2, ascii("N")},
		{0x0002, rational, 3, rationals([2]uint32{28, 1}, [2]uint32{36, 1}, [2]uint32{50, 1})},
		{0x0003, ascii2, 2, ascii("E")},
		{0x0004, rational, 3, rationals([2]uint32{77, 1}, [2]uint32{12, 1}, [2]uint32{30, 1})},
	}
	ifd0 := []tiffEntry{
		{0x010F, ascii2, 10, ascii("NexoraCam")},
		{0x0110, ascii2, 5, ascii("NX-1")},
		{0x8769, long, 1, u32(0)}, // Exif IFD pointer, filled below
		{0x8825, long, 1, u32(0)}, // GPS IFD pointer
	}
	off0 := 8
	offExif := off0 + ifdSize(ifd0)
	offGPS := offExif + ifdSize(exifIFD)
	ifd0[2].data, ifd0[3].data = u32(uint32(offExif)), u32(uint32(offGPS))

	tiff := append([]byte("II*\x00"), u32(uint32(off0))...)
	tiff = append(tiff, encodeIFD(ifd0, off0)...)
	tiff = append(tiff, encodeIFD(exifIFD, offExif)...)
	tiff = append(tiff, encodeIFD(gpsIFD, offGPS)...)

	app1 := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, byte((len(app1) + 2) >> 8), byte(len(app1) + 2)}
	data := j.Bytes()
	return append(append(append([]byte{}, data[:2]...), append(seg, app1...)...), data[2:]...)
}

func TestImagesExtension(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "trip"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "trip", "photo.jpg"), photoWithExif(t, 200, 100), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "trip", "tall.png"), pngBytes(t, 60, 120), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "trip", "broken.jpg"), []byte("not an image"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "trip", "phone.webp"), []byte("RIFF....WEBP"), 0o644))

	im := builtin(t, "images", extension.Values{"folders": []any{dir}})
	var info map[string]any
	must(t, json.Unmarshal(im.do("info", obj{"path": "trip/photo.jpg"}), &info))
	if info["format"] != "jpeg" || info["width"] != 200.0 || info["height"] != 100.0 || info["camera"] != "NexoraCam NX-1" ||
		!strings.HasPrefix(info["takenAt"].(string), "2026-03-14") || info["hasLocation"] != true || info["location"] != nil {
		t.Fatalf("info: %v", info)
	}
	if out := string(im.do("exif", obj{"path": "trip/photo.jpg"})); strings.Contains(out, "atitude") || !strings.Contains(out, "NexoraCam") {
		t.Fatalf("GPS must stay hidden by default: %s", out)
	}

	// Edits never touch the original and pick free names.
	before, _ := os.ReadFile(filepath.Join(dir, "trip", "photo.jpg"))
	out := string(im.do("edit", obj{"path": "trip/photo.jpg", "ops": []obj{{"op": "resize", "width": 50}}}))
	if !strings.Contains(out, "photo-edited.jpg") || !strings.Contains(out, `"width":50,"height":25`) {
		t.Fatalf("resize: %s", out)
	}
	out = string(im.do("edit", obj{"path": "trip/photo.jpg", "ops": []obj{{"op": "crop", "x": 0, "y": 0, "width": 80, "height": 40}, {"op": "rotate", "degrees": 90}}, "format": "png"}))
	if !strings.Contains(out, "photo-edited.png") || !strings.Contains(out, `"width":40,"height":80`) {
		t.Fatalf("crop+rotate+convert: %s", out)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "trip", "photo.jpg")); !bytes.Equal(before, after) {
		t.Fatal("the original must not change")
	}
	im.bad("already exists", "edit", obj{"path": "trip/photo.jpg", "ops": []obj{{"op": "greyscale"}}, "to": "trip/tall.png"})
	im.bad("needs", "edit", obj{"path": "trip/photo.jpg", "ops": []obj{{"op": "crop", "x": 0}}})
	im.bad("Can't decode the image", "edit", obj{"path": "trip/phone.webp"}) // WebP is supported; this one is corrupt
	im.bad("outside the trusted folders", "info", obj{"path": "/etc/hostname"})

	// Metadata stripping: the clean copy has no camera, date or location.
	im.do("removeMetadata", obj{"path": "trip/photo.jpg"})
	var clean map[string]any // fresh map: Unmarshal into a used one keeps old keys
	must(t, json.Unmarshal(im.do("info", obj{"path": "trip/photo-clean.jpg"}), &clean))
	if clean["hasLocation"] != false || clean["camera"] != nil || clean["takenAt"] != nil {
		t.Fatalf("metadata not removed: %v", clean)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "trip", "photo-clean.jpg")); bytes.Contains(data, []byte("Exif")) || bytes.Contains(data, []byte("NexoraCam")) {
		t.Fatal("the clean file still contains EXIF bytes")
	}

	// Everything written must still be a valid image (the metadata stripping
	// rewrites file bytes).
	for _, name := range []string{"photo-edited.jpg", "photo-edited.png", "photo-clean.jpg"} {
		f, err := os.Open(filepath.Join(dir, "trip", name))
		must(t, err)
		img, _, err := image.Decode(f)
		f.Close()
		if err != nil || img.Bounds().Dx() == 0 {
			t.Fatalf("%s is not a valid image: %v", name, err)
		}
	}

	// Thumbnails skip what they can't read.
	var th struct {
		Created int
		Skipped []string
	}
	must(t, json.Unmarshal(im.do("thumbnails", obj{"folder": "trip", "size": 32}), &th))
	if th.Created < 2 || len(th.Skipped) != 2 || !strings.Contains(strings.Join(th.Skipped, " "), "broken.jpg") || !strings.Contains(strings.Join(th.Skipped, " "), "phone.webp") {
		t.Fatalf("thumbnails: %+v", th)
	}

	// find by orientation, location and date
	if out := string(im.do("find", obj{"folder": "trip", "orientation": "portrait"})); !strings.Contains(out, "tall.png") || strings.Contains(out, "photo.jpg") {
		t.Fatalf("find portrait: %s", out)
	}
	if out := string(im.do("find", obj{"withLocation": true, "takenAfter": "2026-03-01"})); !strings.Contains(out, "trip/photo.jpg") || strings.Contains(out, "clean") {
		t.Fatalf("find located: %s", out)
	}
}

func TestImagesRevealsLocationOnlyWhenAllowed(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "photo.jpg"), photoWithExif(t, 40, 30), 0o644))
	im := builtin(t, "images", extension.Values{"folders": []any{dir}, "showLocation": true})
	var info struct {
		Location struct{ Latitude, Longitude float64 }
	}
	must(t, json.Unmarshal(im.do("info", obj{"path": "photo.jpg"}), &info))
	if info.Location.Latitude < 28.6 || info.Location.Latitude > 28.62 || info.Location.Longitude < 77.2 || info.Location.Longitude > 77.21 {
		t.Fatalf("location: %+v", info.Location)
	}
}
