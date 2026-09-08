package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unpackBits is the decoder the encoder is checked against. It exists only
// here: nothing in howl reads an icns, so a bug in packBits would otherwise
// only show up as a 16px icon that looks wrong on someone's Dock.
func unpackBits(in []byte, want int) []byte {
	out := make([]byte, 0, want)
	for i := 0; i < len(in) && len(out) < want; {
		n := in[i]
		i++
		if n&0x80 != 0 {
			count := int(n&0x7f) + 3
			for j := 0; j < count; j++ {
				out = append(out, in[i])
			}
			i++
			continue
		}
		count := int(n) + 1
		out = append(out, in[i:i+count]...)
		i += count
	}
	return out
}

func TestPackBitsRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		{1},
		{1, 1, 1},
		{1, 2, 3, 4, 5},
		bytes.Repeat([]byte{0xff}, 300),         // longer than one 130-byte run
		bytes.Repeat([]byte{1, 2, 3}, 100),      // never three in a row
		append(bytes.Repeat([]byte{7}, 200), 1), // a run then a literal
	}
	for i, in := range cases {
		got := unpackBits(packBits(in), len(in))
		if !bytes.Equal(got, in) {
			t.Errorf("case %d: round trip gave %d bytes, want %d", i, len(got), len(in))
		}
	}
}

func TestPackBitsLiteralLength(t *testing.T) {
	// A literal is capped at 128 bytes, and the count is stored biased by one.
	// Emitting 128 as the byte 128 would be read back as a repeat.
	in := make([]byte, 200)
	for i := range in {
		in[i] = byte(i)
	}
	out := packBits(in)
	if out[0] != 127 {
		t.Fatalf("first literal header = %d, want 127 (128 bytes)", out[0])
	}
	if !bytes.Equal(unpackBits(out, len(in)), in) {
		t.Fatal("128-byte literal did not round trip")
	}
}

func solidIcon(size int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for i := 0; i < size*size; i++ {
		copy(img.Pix[i*4:], []byte{c.R, c.G, c.B, c.A})
	}
	return img
}

func TestResizeKeepsColour(t *testing.T) {
	src := solidIcon(1024, color.NRGBA{0x0E, 0xA5, 0xE9, 0xFF})
	got := resize(src, 16)
	if got.Rect.Dx() != 16 {
		t.Fatalf("size %d, want 16", got.Rect.Dx())
	}
	if p := got.Pix[:4]; p[0] != 0x0E || p[1] != 0xA5 || p[2] != 0xE9 || p[3] != 0xFF {
		t.Errorf("a flat colour downscaled to %v", p)
	}
}

// The bug this pins is a dark rim on every downscaled icon: averaging straight
// NRGBA mixes the RGB of fully transparent pixels — usually black — into the
// visible ones.
func TestResizeDoesNotDarkenTransparentEdges(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			i := src.PixOffset(x, y)
			if x < 32 {
				copy(src.Pix[i:], []byte{0xFF, 0xFF, 0xFF, 0xFF}) // opaque white
			} else {
				copy(src.Pix[i:], []byte{0x00, 0x00, 0x00, 0x00}) // transparent black
			}
		}
	}
	got := resize(src, 32) // each destination pixel covers a 2x2 source block
	// Column 15 is entirely inside the white half; column 16 entirely outside.
	i := got.PixOffset(15, 10)
	if got.Pix[i] != 0xFF || got.Pix[i+3] != 0xFF {
		t.Errorf("opaque white became %v", got.Pix[i:i+4])
	}
	i = got.PixOffset(16, 10)
	if got.Pix[i+3] != 0x00 {
		t.Errorf("transparent pixel gained alpha %d", got.Pix[i+3])
	}
}

func TestEncodeICNSStructure(t *testing.T) {
	data, err := encodeICNS(solidIcon(1024, color.NRGBA{1, 2, 3, 255}))
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != "icns" {
		t.Fatalf("magic %q", data[:4])
	}
	if n := binary.BigEndian.Uint32(data[4:8]); int(n) != len(data) {
		t.Fatalf("header length %d, file %d", n, len(data))
	}

	seen := map[string]bool{}
	for off := 8; off < len(data); {
		typ := string(data[off : off+4])
		size := int(binary.BigEndian.Uint32(data[off+4 : off+8]))
		if size < 8 || off+size > len(data) {
			t.Fatalf("%s: length %d runs past the end", typ, size)
		}
		payload := data[off+8 : off+size]
		seen[typ] = true

		switch typ {
		case "ic04", "ic05":
			// PNG here is the trap: structurally valid, and macOS reads these
			// two slots as JPEG 2000, so the icon decodes to noise.
			if string(payload[:4]) != "ARGB" {
				t.Errorf("%s payload starts %q, want ARGB", typ, payload[:4])
			}
		default:
			if !bytes.HasPrefix(payload, []byte("\x89PNG\r\n\x1a\n")) {
				t.Errorf("%s payload is not a PNG", typ)
			}
		}
		off += size
	}

	for _, e := range icnsSizes {
		if !seen[e.Type] {
			t.Errorf("%s missing from the file", e.Type)
		}
	}
	if seen["icp4"] || seen["icp5"] {
		t.Error("icp4/icp5 are the JPEG 2000 slots; iconutil decodes a PNG there as noise")
	}
}

func TestEncodeARGBRoundTrip(t *testing.T) {
	img := solidIcon(16, color.NRGBA{0x11, 0x22, 0x33, 0x44})
	data := encodeARGB(img)
	if string(data[:4]) != "ARGB" {
		t.Fatalf("magic %q", data[:4])
	}
	// Four planes, 256 pixels each, in A R G B order.
	rest := data[4:]
	want := []byte{0x44, 0x11, 0x22, 0x33}
	for plane := 0; plane < 4; plane++ {
		got := unpackBits(rest, 256)
		if len(got) != 256 {
			t.Fatalf("plane %d decoded to %d bytes", plane, len(got))
		}
		for _, b := range got {
			if b != want[plane] {
				t.Fatalf("plane %d has %#x, want %#x", plane, b, want[plane])
			}
		}
		// Each plane is one run of 256, coded as two 130/126-byte runs.
		rest = rest[consumed(rest, 256):]
	}
}

// consumed reports how many input bytes unpackBits needed to produce want
// output bytes, so the next plane can be found.
func consumed(in []byte, want int) int {
	n := 0
	for i := 0; i < len(in) && n < want; {
		h := in[i]
		if h&0x80 != 0 {
			n += int(h&0x7f) + 3
			i += 2
		} else {
			c := int(h) + 1
			n += c
			i += 1 + c
		}
		if n >= want {
			return i
		}
	}
	return len(in)
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"App Kit":      "app-kit",
		"  App  Kit  ": "app-kit",
		"App-Kit":      "app-kit",
		// Not transliteration: anything outside [a-z0-9] is a separator, so
		// an accented name breaks into pieces rather than being silently
		// collapsed into a different word.
		"Ünïcode 2": "n-code-2",
		"app_kit":   "app-kit",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultBundleID(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/mirairoad/app-kit\n\ngo 1.25.0\n"), 0o644) //nolint:errcheck
	if got := defaultBundleID(dir, "app-kit"); got != "com.github.mirairoad.app-kit" {
		t.Errorf("got %q", got)
	}
	if got := defaultBundleID(t.TempDir(), "thing"); got != "com.example.thing" {
		t.Errorf("with no go.mod: got %q", got)
	}
}

func TestCheckFormatRejectsCrossPlatform(t *testing.T) {
	dir := t.TempDir()
	elf := filepath.Join(dir, "linux-bin")
	os.WriteFile(elf, append([]byte("\x7fELF"), make([]byte, 64)...), 0o755) //nolint:errcheck
	if err := checkFormat(elf, "linux"); err != nil {
		t.Errorf("ELF for linux: %v", err)
	}
	err := checkFormat(elf, "darwin")
	if err == nil {
		t.Fatal("ELF packaged as a .app was accepted")
	}
	if !strings.Contains(err.Error(), "cross-compiled") {
		t.Errorf("unhelpful message: %v", err)
	}

	macho := filepath.Join(dir, "mac-bin")
	os.WriteFile(macho, append([]byte{0xcf, 0xfa, 0xed, 0xfe}, make([]byte, 64)...), 0o755) //nolint:errcheck
	if err := checkFormat(macho, "darwin"); err != nil {
		t.Errorf("Mach-O for darwin: %v", err)
	}
	if checkFormat(macho, "linux") == nil {
		t.Error("Mach-O packaged for linux was accepted")
	}
}

// The two bundles, end to end, from a real PNG on disk.
func TestPackageWritesBothBundles(t *testing.T) {
	dir := t.TempDir()
	assets := filepath.Join(dir, "packaging")
	os.MkdirAll(assets, 0o755) //nolint:errcheck

	var buf bytes.Buffer
	png.Encode(&buf, solidIcon(1024, color.NRGBA{0x0E, 0xA5, 0xE9, 0xFF})) //nolint:errcheck
	os.WriteFile(filepath.Join(assets, "icon.png"), buf.Bytes(), 0o644)    //nolint:errcheck

	bin := filepath.Join(dir, "demo-desktop")
	os.WriteFile(bin, append([]byte{0xcf, 0xfa, 0xed, 0xfe}, make([]byte, 64)...), 0o755) //nolint:errcheck

	icon, err := loadIcon(filepath.Join(assets, "icon.png"))
	if err != nil {
		t.Fatal(err)
	}
	app := appInfo{
		Root: dir, Assets: assets, Out: filepath.Join(dir, "dist"), Bin: bin,
		Exec: "demo-desktop", Name: "Demo App", Slug: "demo-app",
		ID: "com.example.demo", Version: "2.0.0", Icon: icon,
	}

	if err := bundleDarwin(app); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"Demo App.app/Contents/Info.plist",
		"Demo App.app/Contents/PkgInfo",
		"Demo App.app/Contents/MacOS/demo-desktop",
		"Demo App.app/Contents/Resources/AppIcon.icns",
	} {
		if _, err := os.Stat(filepath.Join(app.Out, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	plist, _ := os.ReadFile(filepath.Join(app.Out, "Demo App.app/Contents/Info.plist"))
	for _, want := range []string{
		"<string>com.example.demo</string>",
		"<string>demo-desktop</string>", // must match the file in MacOS/
		"<string>AppIcon</string>",
		"NSHighResolutionCapable",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("Info.plist has no %s", want)
		}
	}
	if info, _ := os.Stat(filepath.Join(app.Out, "Demo App.app/Contents/MacOS/demo-desktop")); info != nil {
		if info.Mode().Perm()&0o111 == 0 {
			t.Error("the bundled binary is not executable")
		}
	}

	if err := bundleLinux(app); err != nil {
		t.Fatal(err)
	}
	entry, _ := os.ReadFile(filepath.Join(app.Out, "demo-app/demo-app.desktop"))
	// Without StartupWMClass the running window never matches its launcher
	// entry, and the taskbar shows a second, iconless item beside it.
	if !strings.Contains(string(entry), "StartupWMClass=demo-desktop") {
		t.Errorf(".desktop is missing StartupWMClass:\n%s", entry)
	}
	if !strings.Contains(string(entry), "Icon=demo-app") {
		t.Error("Icon= must be the slug the hicolor PNGs are named after")
	}
	for _, size := range hicolorSizes {
		p := filepath.Join(app.Out, "demo-app/icons/hicolor",
			strings.Join([]string{itoa(size), "x", itoa(size)}, ""), "apps/demo-app.png")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %dpx icon", size)
		}
	}
	if info, _ := os.Stat(filepath.Join(app.Out, "demo-app/install.sh")); info == nil || info.Mode().Perm()&0o111 == 0 {
		t.Error("install.sh is not executable")
	}
}

// An author-supplied Info.plist is used verbatim: it is the escape hatch for
// everything this tool deliberately has no flag for.
func TestPackageUsesSuppliedPlist(t *testing.T) {
	dir := t.TempDir()
	assets := filepath.Join(dir, "packaging")
	os.MkdirAll(assets, 0o755)                                                           //nolint:errcheck
	os.WriteFile(filepath.Join(assets, "Info.plist"), []byte("MINE"), 0o644)             //nolint:errcheck
	bin := filepath.Join(dir, "demo")                                                    //
	os.WriteFile(bin, append([]byte{0xcf, 0xfa, 0xed, 0xfe}, make([]byte, 8)...), 0o755) //nolint:errcheck

	app := appInfo{
		Root: dir, Assets: assets, Out: filepath.Join(dir, "dist"), Bin: bin,
		Exec: "demo", Name: "Demo", Slug: "demo", ID: "com.example.demo",
		Version: "1.0.0", Icon: solidIcon(1024, color.NRGBA{1, 2, 3, 255}),
	}
	if err := bundleDarwin(app); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(app.Out, "Demo.app/Contents/Info.plist"))
	if string(got) != "MINE" {
		t.Errorf("supplied Info.plist was regenerated: %q", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
