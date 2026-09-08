// howl package — the desktop binary, with the icon and the metadata the OS
// needs to show it as an application.
//
// This does not build anything. `go build` builds; the desktop binary is a
// nested module and only its own go.mod knows how. What is missing from a bare
// binary is not compilation, it is the twenty kilobytes of platform metadata
// that decides whether the Dock shows your icon or a blank sheet of paper:
//
//	macOS   a .app bundle — Info.plist, an .icns, and the binary inside it
//	Linux   a .desktop entry, PNGs in the hicolor theme, and an installer
//	        that puts both where the desktop environment looks
//
// Every value that changes between releases is a flag, because it already
// lives in the caller's Makefile next to the binary name. Everything static is
// a file in desktop/packaging/, because a file location is how the rest of
// this framework carries configuration and a manifest describing three files
// is longer than the three files.
//
// The escape hatch is the same in both directions: drop a real Info.plist or
// app.desktop into desktop/packaging/ and it is used verbatim. That is the
// answer to LSMinimumSystemVersion, URL schemes, document types, MIME
// associations and everything else this would otherwise grow a flag for.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func packageApp(args []string) error {
	fset := flag.NewFlagSet("package", flag.ExitOnError)
	dir := fset.String("dir", ".", "project root")
	bin := fset.String("bin", "", "the desktop binary to package (required) — build it first with go build")
	name := fset.String("name", "", "display name, e.g. \"App Kit\"; defaults to the binary's file name")
	id := fset.String("id", "", "bundle identifier, e.g. com.example.app; defaults to a reverse-DNS form of the module path")
	version := fset.String("version", "0.1.0", "version string, shown in Get Info and in the .desktop entry")
	icon := fset.String("icon", "", "source icon PNG; defaults to <assets>/icon.png")
	assets := fset.String("assets", "", "packaging assets directory; defaults to <dir>/desktop/packaging")
	out := fset.String("out", "", "output directory; defaults to <dir>/dist")
	goos := fset.String("os", runtime.GOOS, "target platform: darwin or linux")
	fset.Parse(args)

	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if *bin == "" {
		return fmt.Errorf("-bin is required: howl package does not build, it packages a binary you built")
	}
	binPath, err := filepath.Abs(*bin)
	if err != nil {
		return err
	}
	info, err := os.Stat(binPath)
	if err != nil {
		return fmt.Errorf("-bin: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("-bin: %s is a directory", binPath)
	}
	if err := checkFormat(binPath, *goos); err != nil {
		return err
	}

	assetDir := *assets
	if assetDir == "" {
		assetDir = filepath.Join(root, "desktop", "packaging")
	}
	iconPath := *icon
	if iconPath == "" {
		iconPath = filepath.Join(assetDir, "icon.png")
	}
	outDir := *out
	if outDir == "" {
		outDir = filepath.Join(root, "dist")
	}

	exec := filepath.Base(binPath)
	display := *name
	if display == "" {
		display = exec
	}
	bundleID := *id
	if bundleID == "" {
		bundleID = defaultBundleID(root, slugify(display))
	}

	src, err := loadIcon(iconPath)
	if err != nil {
		return err
	}

	app := appInfo{
		Root: root, Assets: assetDir, Out: outDir, Bin: binPath,
		Exec: exec, Name: display, Slug: slugify(display),
		ID: bundleID, Version: *version, Icon: src,
	}

	switch *goos {
	case "darwin":
		return bundleDarwin(app)
	case "linux":
		return bundleLinux(app)
	}
	return fmt.Errorf("-os: %q is not packaged here; darwin and linux are", *goos)
}

// checkFormat refuses to package a binary built for another platform.
//
// The mistake this catches is not exotic, it is the first thing anyone tries:
// `make package` with GOOS=linux set, which cross-compiles nothing here
// because the desktop binary needs cgo and WebKitGTK, and quietly wraps the
// macOS binary in a Linux install tree. The result installs, appears in the
// launcher with the right icon, and does nothing when clicked.
//
// Four bytes are enough to tell: ELF says so in ASCII, and Mach-O is one of
// four magics depending on width and endianness.
func checkFormat(path, goos string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var head [4]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return fmt.Errorf("-bin: %s is too small to be a binary", path)
	}
	be := binary.BigEndian.Uint32(head[:])
	var actual string
	switch {
	case string(head[:]) == "\x7fELF":
		actual = "linux"
	case be == 0xfeedface || be == 0xfeedfacf || be == 0xcefaedfe || be == 0xcffaedfe:
		actual = "darwin"
	case be == 0xcafebabe || be == 0xbebafeca:
		actual = "darwin" // a universal binary, which is still a Mach-O
	default:
		return nil // not a format we recognise; not our business to refuse it
	}
	if actual != goos {
		return fmt.Errorf("-bin: %s is a %s binary and -os is %s.\n"+
			"The desktop binary needs cgo and the platform's webview, so it cannot be\n"+
			"cross-compiled from here: build it on %s and package it there",
			filepath.Base(path), actual, goos, goos)
	}
	return nil
}

type appInfo struct {
	Root    string
	Assets  string
	Out     string
	Bin     string
	Exec    string // the file name inside the bundle; also the X11 WM_CLASS on Linux
	Name    string
	Slug    string
	ID      string
	Version string
	Icon    *image.NRGBA
}

// ---------------------------------------------------------------------------
// macOS
// ---------------------------------------------------------------------------

func bundleDarwin(a appInfo) error {
	root := filepath.Join(a.Out, a.Name+".app")
	contents := filepath.Join(root, "Contents")

	// Replace rather than merge. A bundle assembled over a previous one keeps
	// whatever the last build left in Resources, and a stale icon that
	// LaunchServices has already cached is close to undebuggable.
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	for _, d := range []string{filepath.Join(contents, "MacOS"), filepath.Join(contents, "Resources")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	if err := copyFile(a.Bin, filepath.Join(contents, "MacOS", a.Exec), 0o755); err != nil {
		return err
	}

	plist, custom, err := readOverride(filepath.Join(a.Assets, "Info.plist"))
	if err != nil {
		return err
	}
	if !custom {
		plist = []byte(infoPlist(a))
	}
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), plist, 0o644); err != nil {
		return err
	}

	// Classic and still read by some of Launch Services' older paths. Eight
	// bytes, and its absence is one of the things that makes a bundle "not an
	// application" without saying so.
	if err := os.WriteFile(filepath.Join(contents, "PkgInfo"), []byte("APPL????"), 0o644); err != nil {
		return err
	}

	icns, err := encodeICNS(a.Icon)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(contents, "Resources", "AppIcon.icns"), icns, 0o644); err != nil {
		return err
	}

	fmt.Printf("howl package: %s\n", root)
	fmt.Printf("  identifier  %s\n  version     %s\n  icon        %d bytes, %d sizes\n",
		a.ID, a.Version, len(icns), len(icnsSizes))
	if custom {
		fmt.Printf("  Info.plist  from %s\n", filepath.Join(a.Assets, "Info.plist"))
	}
	fmt.Printf("\nDrag it to /Applications. Unsigned: the first launch needs right-click ▸ Open,\n" +
		"or Gatekeeper refuses it with a message about an unidentified developer.\n")
	return nil
}

// infoPlist is the smallest plist macOS treats as a real application.
//
// NSHighResolutionCapable is not optional in practice: without it the window
// is drawn into a 1x backing store and scaled up, so a WKWebView that reports
// devicePixelRatio 2 renders through a blur.
func infoPlist(a appInfo) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	pairs := [][2]string{
		{"CFBundleInfoDictionaryVersion", "6.0"},
		{"CFBundlePackageType", "APPL"},
		{"CFBundleName", a.Name},
		{"CFBundleDisplayName", a.Name},
		{"CFBundleExecutable", a.Exec},
		{"CFBundleIdentifier", a.ID},
		{"CFBundleIconFile", "AppIcon"},
		{"CFBundleShortVersionString", a.Version},
		{"CFBundleVersion", a.Version},
		{"LSMinimumSystemVersion", "11.0"},
	}
	for _, p := range pairs {
		fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", p[0], xmlEscape(p[1]))
	}
	b.WriteString("\t<key>NSHighResolutionCapable</key>\n\t<true/>\n</dict>\n</plist>\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// Linux
// ---------------------------------------------------------------------------

// hicolorSizes are the sizes the freedesktop theme spec expects to find. 24 is
// there for panels that still ask for it; 512 for the GNOME app grid, which
// picks the largest available and scales down rather than up.
var hicolorSizes = []int{16, 24, 32, 48, 64, 128, 256, 512}

func bundleLinux(a appInfo) error {
	root := filepath.Join(a.Out, a.Slug)
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

	if err := copyFile(a.Bin, filepath.Join(root, a.Exec), 0o755); err != nil {
		return err
	}

	entry, custom, err := readOverride(filepath.Join(a.Assets, "app.desktop"))
	if err != nil {
		return err
	}
	if !custom {
		entry = []byte(desktopEntry(a))
	}
	if err := os.WriteFile(filepath.Join(root, a.Slug+".desktop"), entry, 0o644); err != nil {
		return err
	}

	for _, size := range hicolorSizes {
		dir := filepath.Join(root, "icons", "hicolor", fmt.Sprintf("%dx%d", size, size), "apps")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		data, err := encodePNG(resize(a.Icon, size))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, a.Slug+".png"), data, 0o644); err != nil {
			return err
		}
	}

	if err := os.WriteFile(filepath.Join(root, "install.sh"), []byte(installScript(a)), 0o755); err != nil {
		return err
	}

	fmt.Printf("howl package: %s\n", root)
	fmt.Printf("  identifier  %s\n  version     %s\n  icons       %v\n", a.ID, a.Version, hicolorSizes)
	if custom {
		fmt.Printf("  .desktop    from %s\n", filepath.Join(a.Assets, "app.desktop"))
	}
	fmt.Printf("\nInstall with ./install.sh (PREFIX=%s by default, or PREFIX=/usr/local as root).\n",
		"$HOME/.local")
	fmt.Printf("libwebkit2gtk-4.1 is a runtime dependency; the binary is not static.\n")
	return nil
}

// desktopEntry is the launcher.
//
// StartupWMClass is the line that gets left out and then costs an afternoon.
// Without it the running window is never matched to the entry that launched
// it, so the taskbar shows a second, iconless item beside the pinned one. GTK
// sets WM_CLASS from g_get_prgname, which is the executable's base name — so
// this must be the file name, not the slug and not the display name.
func desktopEntry(a appInfo) string {
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=%s
Exec=%s
Icon=%s
Terminal=false
Categories=Utility;
StartupWMClass=%s
X-Howl-Version=%s
`, a.Name, a.Exec, a.Slug, a.Exec, a.Version)
}

// installScript copies into the XDG directories and then rebuilds the two
// caches. Both refreshes are guarded: neither tool is guaranteed to exist, and
// on the desktops that do ship them a skipped refresh means the entry is there
// and invisible until the next login.
//
// Exec is rewritten to the absolute installed path rather than relying on
// PREFIX/bin being on PATH — a .desktop launched by the shell does not inherit
// the login shell's PATH edits, so a bare name works from a terminal and fails
// from the app grid.
func installScript(a appInfo) string {
	return fmt.Sprintf(`#!/bin/sh
# Installs %s into the XDG directories, or removes it with --uninstall.
set -eu

PREFIX="${PREFIX:-$HOME/.local}"
here="$(cd "$(dirname "$0")" && pwd)"

apps="$PREFIX/share/applications"
icons="$PREFIX/share/icons/hicolor"
bin="$PREFIX/bin"

if [ "${1:-}" = "--uninstall" ]; then
  rm -f "$bin/%s" "$apps/%s.desktop"
  find "$icons" -name '%s.png' -delete 2>/dev/null || true
  echo "removed %s from $PREFIX"
else
  mkdir -p "$bin" "$apps"
  install -m 0755 "$here/%s" "$bin/%s"
  sed "s|^Exec=.*|Exec=$bin/%s|" "$here/%s.desktop" > "$apps/%s.desktop"
  chmod 0644 "$apps/%s.desktop"
  for f in "$here"/icons/hicolor/*/apps/*.png; do
    rel="${f#"$here"/icons/hicolor/}"
    mkdir -p "$icons/${rel%%/*.png}"
    install -m 0644 "$f" "$icons/$rel"
  done
  echo "installed %s into $PREFIX"
fi

# Both are best-effort: not every desktop ships them, and on the ones that do,
# skipping the refresh leaves the entry installed and invisible until re-login.
command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$apps" || true
command -v gtk-update-icon-cache   >/dev/null 2>&1 && gtk-update-icon-cache -f -t "$icons" || true
`,
		a.Name,
		a.Exec, a.Slug, a.Slug, a.Name,
		a.Exec, a.Exec,
		a.Exec, a.Slug, a.Slug, a.Slug,
		a.Name)
}

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

// icnsSizes is the entry table, and it is exactly what Apple's iconutil emits
// from a full .iconset — checked by building one and reading the chunks back,
// not inferred from the format notes.
//
// Two things about it are not guessable. The names are not a size ordering:
// ic11 is 32px (16@2x) and ic10 is 1024px (512@2x). And the two smallest sizes
// are not PNG — they are ic04/ic05 carrying raw ARGB. The icp4/icp5 slots that
// look like the obvious home for a 16px and a 32px PNG are a dead end: a PNG
// written there produces a structurally valid file that iconutil itself
// decodes into noise, because those slots predate PNG icons and are read as
// JPEG 2000. The first version of this shipped that way and the round-trip is
// what caught it.
var icnsSizes = []struct {
	Type string
	Size int
	ARGB bool
}{
	{"ic04", 16, true},
	{"ic05", 32, true},
	{"ic07", 128, false},
	{"ic08", 256, false},
	{"ic09", 512, false},
	{"ic10", 1024, false},
	{"ic11", 32, false},
	{"ic12", 64, false},
	{"ic13", 256, false},
	{"ic14", 512, false},
}

// encodeICNS writes the container by hand, which is ~30 lines and removes the
// dependency on iconutil and sips — both macOS-only, so shelling out to them
// would mean a .app that can only be built on a Mac.
//
// The format: "icns", the total length, then a flat run of entries, each an
// OSType, its own length including the 8-byte header, and a PNG. There is an
// optional 'TOC ' entry, which nothing requires.
func encodeICNS(src *image.NRGBA) ([]byte, error) {
	var body bytes.Buffer
	for _, e := range icnsSizes {
		img := resize(src, e.Size)
		var data []byte
		var err error
		if e.ARGB {
			data = encodeARGB(img)
		} else {
			data, err = encodePNG(img)
		}
		if err != nil {
			return nil, err
		}
		body.WriteString(e.Type)
		binary.Write(&body, binary.BigEndian, uint32(len(data)+8)) //nolint:errcheck // bytes.Buffer
		body.Write(data)
	}
	var out bytes.Buffer
	out.WriteString("icns")
	binary.Write(&out, binary.BigEndian, uint32(body.Len()+8)) //nolint:errcheck
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// encodeARGB writes the uncompressed-icon format the ic04 and ic05 slots use:
// the magic "ARGB", then the alpha, red, green and blue planes one after the
// other, each run-length coded on its own.
//
// Channel-at-a-time is the whole reason the coding pays: a flat background is
// one long run per plane, where interleaved ARGB pixels would break every run
// after four bytes. The colour planes are straight, not premultiplied — the
// alpha plane is a separate mask here rather than a factor already applied.
func encodeARGB(img *image.NRGBA) []byte {
	n := img.Rect.Dx() * img.Rect.Dy()
	planes := make([][]byte, 4)
	for c := range planes {
		planes[c] = make([]byte, 0, n)
	}
	for i := 0; i < n; i++ {
		p := img.Pix[i*4:]
		planes[0] = append(planes[0], p[3]) // A
		planes[1] = append(planes[1], p[0]) // R
		planes[2] = append(planes[2], p[1]) // G
		planes[3] = append(planes[3], p[2]) // B
	}
	out := []byte("ARGB")
	for _, plane := range planes {
		out = append(out, packBits(plane)...)
	}
	return out
}

// packBits is Apple's icon variant of the run-length coding, which is not the
// same as the PackBits in the TIFF spec: a repeat is 0x80|(count-3) with count
// in 3..130, and a literal is (count-1) with count in 1..128. Using TIFF's
// bias here produces a file that decodes to the right length and the wrong
// pixels.
func packBits(data []byte) []byte {
	var out []byte
	for i := 0; i < len(data); {
		run := 1
		for i+run < len(data) && data[i+run] == data[i] && run < 130 {
			run++
		}
		if run >= 3 {
			out = append(out, byte(0x80|(run-3)), data[i])
			i += run
			continue
		}
		// A literal ends where the next run of three begins, so the coder does
		// not emit a 128-byte literal straddling a run it could have coded.
		start := i
		for i < len(data) && i-start < 128 {
			if i+2 < len(data) && data[i] == data[i+1] && data[i+1] == data[i+2] {
				break
			}
			i++
		}
		out = append(out, byte(i-start-1))
		out = append(out, data[start:i]...)
	}
	return out
}

// loadIcon reads the source PNG and normalises it to NRGBA.
//
// Square is enforced rather than corrected: every target here is square, so a
// rectangular source is either letterboxed or squashed, and choosing silently
// on the author's behalf produces an icon that looks subtly wrong at every
// size with nothing to point at.
func loadIcon(path string) (*image.NRGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("icon: %w (a 1024x1024 PNG; -icon points somewhere else)", err)
	}
	defer f.Close()

	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("icon %s: %w", path, err)
	}
	b := img.Bounds()
	if b.Dx() != b.Dy() {
		return nil, fmt.Errorf("icon %s is %dx%d: it must be square", path, b.Dx(), b.Dy())
	}
	if b.Dx() < 512 {
		return nil, fmt.Errorf("icon %s is %dpx: 1024 is wanted, and below 512 the large "+
			"macOS variants would be upscaled", path, b.Dx())
	}

	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := out.PixOffset(x, y)
			// RGBA() is alpha-premultiplied 16-bit; NRGBA is not premultiplied.
			if a == 0 {
				continue
			}
			out.Pix[i+0] = uint8(r * 0xffff / a >> 8)
			out.Pix[i+1] = uint8(g * 0xffff / a >> 8)
			out.Pix[i+2] = uint8(bl * 0xffff / a >> 8)
			out.Pix[i+3] = uint8(a >> 8)
		}
	}
	return out, nil
}

// resize is a box filter: every destination pixel is the average of the source
// rectangle it covers. For downscaling — which is all this does — that is the
// correct answer and not an approximation of one, and it beats the bilinear
// sampling most resizers reach for, which reads four pixels out of the 64 that
// a 1024→128 step should be averaging and aliases the rest away.
//
// The averaging happens in premultiplied space. Averaging straight NRGBA pulls
// the colour of fully transparent pixels into the result, and a transparent
// pixel's RGB is usually black — which is where the dark fringe around a
// downscaled icon's rounded corners comes from.
func resize(src *image.NRGBA, size int) *image.NRGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	if sw == size && sh == size {
		return src
	}
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		y0, y1 := y*sh/size, (y+1)*sh/size
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < size; x++ {
			x0, x1 := x*sw/size, (x+1)*sw/size
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a float64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					i := src.PixOffset(sx, sy)
					af := float64(src.Pix[i+3]) / 255
					r += float64(src.Pix[i+0]) * af
					g += float64(src.Pix[i+1]) * af
					b += float64(src.Pix[i+2]) * af
					a += float64(src.Pix[i+3])
				}
			}
			n := float64((y1 - y0) * (x1 - x0))
			alpha := a / n
			o := dst.PixOffset(x, y)
			if alpha > 0 {
				f := 255 / (alpha * n)
				dst.Pix[o+0] = clamp8(r * f)
				dst.Pix[o+1] = clamp8(g * f)
				dst.Pix[o+2] = clamp8(b * f)
			}
			dst.Pix[o+3] = clamp8(alpha)
		}
	}
	return dst
}

func clamp8(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	}
	return uint8(v + 0.5)
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	// Icons are written once and read forever, and the .icns carries ten of
	// them, so the slow encoder is the right trade.
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Odds and ends
// ---------------------------------------------------------------------------

// readOverride reports whether the author supplied their own file, and returns
// it if so. Missing is the normal case, not an error.
func readOverride(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func copyFile(from, to string, mode os.FileMode) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// defaultBundleID derives a reverse-DNS identifier from the module path, so
// two howl apps on one machine never collide.
//
// It is printed on every run because it is worth seeing: Launch Services keys
// its cache on this string, so an app installed under one identifier and then
// rebuilt under another leaves a ghost that still claims the old icon.
func defaultBundleID(root, slug string) string {
	mod := moduleOf(root)
	if mod == "" {
		return "com.example." + slug
	}
	parts := strings.Split(mod, "/")
	host := strings.Split(parts[0], ".")
	var rev []string
	for i := len(host) - 1; i >= 0; i-- {
		rev = append(rev, host[i])
	}
	rev = append(rev, parts[1:]...)
	for i, p := range rev {
		rev[i] = strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
				return r
			}
			return '-'
		}, p)
	}
	return strings.Join(rev, ".")
}

func moduleOf(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// slugify is the file-name form: the .desktop base name, the icon name in the
// hicolor theme and the output directory. The three must agree — the entry's
// Icon= key is looked up by exactly this name.
//
// Every character outside [a-z0-9] is a separator, including accented letters.
// That is not transliteration and does not pretend to be: "Ünïcode" becomes
// "n-code", which is visibly wrong and therefore fixed with -name, where
// dropping the accents would produce a plausible slug for a different word.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
