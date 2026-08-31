package components

// virtual_media_test.go guards the upload file picker's accept filter.
//
// The server has accepted compressed images since streamio.DecompressingReader
// landed: an upload is sniffed by magic bytes and inflated in place, so
// "ubuntu-24.04.img.xz" stages as "ubuntu-24.04.img". The picker never
// learned. accept=".iso,.img" greys the file out in the OS dialog, so the
// capability existed and could not be reached — the same two-halves-in-two-
// files drift that left the firmware Upload button permanently disabled.
//
// So the filter is derived from streamio.CompressionExtensions() rather than
// spelled out here, and these tests hold the derivation to the decoder: add a
// codec to pkg/streamio and the picker follows, or these fail.
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

	"github.com/pi-bmc/nanokvm-app/pkg/streamio"
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
	for _, ext := range streamio.CompressionExtensions() {
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
	for _, ext := range streamio.CompressionExtensions() {
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

	for _, ext := range streamio.CompressionExtensions() {
		if !strings.Contains(pattern, strings.TrimPrefix(ext, ".")) {
			t.Errorf("the phase label's pattern %q does not match %q, so an upload "+
				"the picker now allows reports no extraction phase", pattern, ext)
		}
	}
}

// ── the Existing tab's split button ─────────────────────────────────────
//
// Mount and Delete share one file selector. They are two separate buttons
// rather than one whose verb is rewritten, because htmx captures a verb
// attribute when it processes an element: mutating hx-post afterwards does
// nothing without htmx.process(), and the power toggle already shipped once
// posting the previous action while the label said the new one. Two static
// buttons, one hidden, cannot drift that way — whichever is visible is the
// only one whose request exists.

func renderVMAdd(t *testing.T, files []string) string {
	t.Helper()
	return renderToString(t, func(w *strings.Builder) error {
		return VMAddBody(files).Render(context.Background(), w)
	})
}

// buttonTag returns the markup of the element carrying id.
func buttonTag(t *testing.T, html, id string) string {
	t.Helper()
	re := regexp.MustCompile(`<[a-zA-Z]+[^>]*\bid="` + regexp.QuoteMeta(id) + `"[^>]*>`)
	m := re.FindString(html)
	if m == "" {
		t.Fatalf("no element with id %q in:\n%s", id, html)
	}
	return m
}

func TestMediaActionsAreTwoStaticButtons(t *testing.T) {
	html := renderVMAdd(t, []string{"alpine.iso"})

	mount := buttonTag(t, html, "vm-mount-submit")
	del := buttonTag(t, html, "vm-delete-submit")

	if !strings.Contains(mount, `hx-post="/ui/media/insert"`) {
		t.Errorf("mount button does not post to /ui/media/insert: %s", mount)
	}
	if !strings.Contains(del, `hx-post="/ui/media/delete"`) {
		t.Errorf("delete button does not post to /ui/media/delete: %s", del)
	}
	// Each carries its own verb; neither is a shared button reprogrammed at
	// runtime, which htmx would ignore.
	if strings.Contains(mount, "/ui/media/delete") || strings.Contains(del, "/ui/media/insert") {
		t.Error("the two actions share a request target")
	}
}

// The gate the request asked for: Delete cannot be pressed until it has been
// chosen from the chevron menu.
func TestDeleteStartsHidden(t *testing.T) {
	html := renderVMAdd(t, []string{"alpine.iso"})

	del := buttonTag(t, html, "vm-delete-submit")
	if !strings.Contains(del, "hidden") {
		t.Errorf("the delete button renders visible, so a stray click destroys an "+
			"image with no deliberate step in between: %s", del)
	}
	if mount := buttonTag(t, html, "vm-mount-submit"); strings.Contains(mount, " hidden") {
		t.Errorf("mount is hidden by default; the panel opens with no usable action: %s", mount)
	}
	// Hidden by ATTRIBUTE, not a utility class: preflight's
	// [hidden]{display:none!important} beats the button's own inline-flex,
	// where a `hidden` class loses to it on stylesheet order.
	if strings.Contains(del, `class="hidden`) || strings.Contains(del, ` hidden "`) {
		t.Error("delete hidden via a class, which the button's inline-flex overrides")
	}
}

func TestDeleteIsMarkedDestructive(t *testing.T) {
	html := renderVMAdd(t, []string{"alpine.iso"})

	del := buttonTag(t, html, "vm-delete-submit")
	if !strings.Contains(del, "destructive") {
		t.Errorf("the delete button is styled like Mount; the only thing separating "+
			"a mount from an unlink would be the word: %s", del)
	}
}

// Nothing may submit the enclosing form on its own. A native submit would run
// whichever action the form names regardless of which button is showing —
// exactly the label-says-one-thing-does-another failure the split avoids.
func TestExistingFormCannotSelfSubmit(t *testing.T) {
	html := renderVMAdd(t, []string{"alpine.iso"})

	formRe := regexp.MustCompile(`(?s)<form[^>]*id="vm-existing-form".*?</form>`)
	form := formRe.FindString(html)
	if form == "" {
		t.Fatalf("vm-existing-form not found in:\n%s", html)
	}
	if strings.Contains(form, `type="submit"`) {
		t.Error("a submit button inside the form can fire the form's own action")
	}
	openTag := regexp.MustCompile(`<form[^>]*id="vm-existing-form"[^>]*>`).FindString(form)
	if strings.Contains(openTag, "hx-post") {
		t.Errorf("the form carries its own verb, which would run whichever action it "+
			"names no matter which button is visible: %s", openTag)
	}
}

// The chevron menu is the only way into delete mode, so both actions have to
// be reachable from it — including switching back.
func TestActionMenuOffersBothActions(t *testing.T) {
	html := renderVMAdd(t, []string{"alpine.iso"})

	for _, action := range []string{"mount", "delete"} {
		if !strings.Contains(html, `data-vm-action="`+action+`"`) {
			t.Errorf("no menu item selects %q", action)
		}
	}
	// The trigger is icon-only, so its accessible name has to come from an
	// aria-label — an unlabelled button is announced as just "button", and
	// this one is the only route to Delete.
	trigger := buttonTag(t, html, "vm-action-menu")
	if !strings.Contains(trigger, "aria-label=") {
		t.Errorf("the action menu trigger has no accessible name: %s", trigger)
	}
	if !strings.Contains(trigger, `aria-haspopup="menu"`) {
		t.Errorf("the trigger is not wired as a menu button: %s", trigger)
	}
}

// The ids the toggle script reaches for. Renaming one half silently strands
// the other: the menu would open, the click would do nothing, and Delete
// would stay unreachable.
func TestActionToggleScriptMatchesTheMarkup(t *testing.T) {
	src, err := TemplFiles.ReadFile("virtual_media.js")
	if err != nil {
		t.Fatalf("read virtual_media.js: %v", err)
	}
	js := string(src)

	for _, ref := range []string{"vm-mount-submit", "vm-delete-submit", "data-vm-action"} {
		if !strings.Contains(js, ref) {
			t.Errorf("virtual_media.js never mentions %q, so the chevron menu cannot "+
				"switch the button it is supposed to switch", ref)
		}
	}
}

// Two easily-tidied-away classes hold the Delete button's edges in the group.
//
// ButtonGroup strips the left border off every child after the first, a rule
// written for siblings that share the screen; Mount and Delete are
// alternatives, so the one that shows is always the first element. And the
// divider between an action and the chevron is a 1px TRANSPARENT gutter
// (bg-clip-padding), which only reads against a bright fill — Mount is solid
// primary, the destructive variant's bg-destructive/10 is all but black, so
// the seam vanished and the chevron looked unattached to anything.
func TestDeleteButtonKeepsItsEdgesInTheGroup(t *testing.T) {
	html := renderVMAdd(t, []string{"alpine.iso"})
	del := buttonTag(t, html, "vm-delete-submit")

	if !strings.Contains(del, "border-l!") {
		t.Errorf("no left-border override: ButtonGroup's [&>[data-slot]~[data-slot]]"+
			":border-l-0 leaves Delete a pixel short on the side Mount has one, "+
			"so the group's left edge shifts when the mode changes: %s", del)
	}
	if strings.Contains(del, "border-transparent") {
		t.Errorf("the delete button's border is transparent, so the gutter that "+
			"separates it from the chevron has nothing to show against: %s", del)
	}
	if !strings.Contains(del, "border-destructive/") {
		t.Errorf("no visible border colour; the chevron reads as detached: %s", del)
	}
}
