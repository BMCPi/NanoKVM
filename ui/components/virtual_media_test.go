package components

// virtual_media_test.go guards the upload file picker's accept filter.
//
// The server has accepted compressed images since utils.DecompressingReader
// landed: an upload is sniffed by magic bytes and inflated in place, so
// "ubuntu-24.04.img.xz" stages as "ubuntu-24.04.img". The picker never
// learned. accept=".iso,.img" greys the file out in the OS dialog, so the
// capability existed and could not be reached — the same two-halves-in-two-
// files drift that left the firmware Upload button permanently disabled.
//
// So the filter is derived from utils.CompressionExtensions() rather than
// spelled out here, and these tests hold the derivation to the decoder: add a
// codec to pkg/utils and the picker follows, or these fail.
//
// Capsules are the deliberate exception. StageCapsule io.Copy's the body onto
// the FAT volume with no decoder in the path, so a .cap.gz would be staged
// verbatim and the host would reject a capsule that is really a gzip member.
// TestCapsuleUploadDoesNotOfferCompressedExtensions holds that line.

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

var acceptAttrRe = regexp.MustCompile(`accept="([^"]*)"`)

// acceptValues returns every accept filter in the markup, split into tokens.
func acceptValues(t *testing.T, html string) [][]string {
	t.Helper()

	matches := acceptAttrRe.FindAllStringSubmatch(html, -1)
	out := make([][]string, 0, len(matches))
	for _, m := range matches {
		toks := strings.Split(m[1], ",")
		for i := range toks {
			toks[i] = strings.TrimSpace(toks[i])
		}
		out = append(out, toks)
	}
	return out
}

func hasToken(toks []string, want string) bool {
	for _, tok := range toks {
		if strings.EqualFold(tok, want) {
			return true
		}
	}
	return false
}

func TestMediaUploadAcceptsCompressedImages(t *testing.T) {
	html := renderToString(t, func(w *strings.Builder) error {
		return VMAddBody(nil).Render(context.Background(), w)
	})

	filters := acceptValues(t, html)
	if len(filters) == 0 {
		t.Fatal("no accept filter on the media upload form")
	}
	accept := filters[0]

	// The uncompressed forms must survive: this widens the filter, it does
	// not replace it.
	for _, want := range []string{".iso", ".img"} {
		if !hasToken(accept, want) {
			t.Errorf("accept lost %q: %v", want, accept)
		}
	}

	// And every codec the server can actually inflate.
	for _, ext := range utils.CompressionExtensions() {
		if !hasToken(accept, ext) {
			t.Errorf("accept omits %q, so the OS dialog greys out an image the "+
				"server would have decompressed happily: %v", ext, accept)
		}
	}
}

// The picker is a convenience, never a check — the server sniffs magic bytes
// and does not care what the filename claims. Pinning that here so nobody
// later "hardens" the accept list into something a legitimate upload trips on.
func TestMediaUploadFilterIsNotAGate(t *testing.T) {
	html := renderToString(t, func(w *strings.Builder) error {
		return VMAddBody(nil).Render(context.Background(), w)
	})

	if strings.Contains(html, `required`) && strings.Contains(html, `pattern=`) {
		t.Error("the upload input validates the filename client-side; " +
			"format detection is the server's job and is done on content")
	}
}

// Capsules are staged byte-for-byte. Offering a compressed extension here
// would let an operator pick a file the BMC writes to the capsule volume
// unchanged, and the host would reject it at the next boot with nothing in
// the UI explaining why.
func TestCapsuleUploadDoesNotOfferCompressedExtensions(t *testing.T) {
	html := renderFirmwarePanel(t, SettingsFirmware{VolumeReady: true, Presented: true})

	filters := acceptValues(t, html)
	if len(filters) == 0 {
		t.Fatal("no accept filter on the capsule upload form")
	}
	accept := filters[0]

	if !hasToken(accept, ".cap") {
		t.Errorf("accept lost .cap: %v", accept)
	}
	for _, ext := range utils.CompressionExtensions() {
		if hasToken(accept, ext) {
			t.Errorf("accept offers %q, but StageCapsule copies the body verbatim — "+
				"a compressed capsule would be staged still compressed: %v", ext, accept)
		}
	}
}

// The in-flight label reads the chosen filename to say "Uploading &
// extracting (xz)…". It is cosmetic, but a codec the picker now allows and
// the label does not recognise reports a plain "Uploading…" for a transfer
// that is about to spend most of its time decompressing.
func TestUploadPhaseLabelKnowsEveryCompressionExtension(t *testing.T) {
	src, err := TemplFiles.ReadFile("virtual_media.js")
	if err != nil {
		t.Fatalf("read virtual_media.js: %v", err)
	}

	re := regexp.MustCompile(`COMPRESSED_EXT\s*=\s*/([^/]*)/`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatal("COMPRESSED_EXT not found in virtual_media.js")
	}
	pattern := string(m[1])

	for _, ext := range utils.CompressionExtensions() {
		if !strings.Contains(pattern, strings.TrimPrefix(ext, ".")) {
			t.Errorf("the phase label's pattern %q does not match %q, so an upload "+
				"the picker now allows reports no extraction phase", pattern, ext)
		}
	}
}
