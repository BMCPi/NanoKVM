package usbgadget

// HID report descriptors for the two human-interface functions the gadget
// exposes: hid.GS0, a strict boot-protocol keyboard, and hid.GS1, the two
// pointer collections (relative mouse + absolute pointer) multiplexed over
// one interface with Report IDs (HID 1.11 §5.6/§8.1).
//
// The keyboard stays its own unmodified 8-byte boot function on purpose:
// pre-boot environments (EDK2 UsbKbDxe, U-Boot) force boot protocol and
// assume exactly 8 bytes, and a Report ID would make every keystroke a
// 9-byte packet they misparse — typing in UEFI setup is a core KVM duty.
// The mice have no such duty (boot-mouse support in firmware is marginal
// and our pre-boot pointer story is the absolute device, which boot
// protocol cannot describe anyway), so they share an interface freely.
//
// The three per-collection descriptors below are the historical S03usbdev
// byte sequences, unchanged (hid_test.go pins them); the combined mouse
// descriptor is derived by inserting a Report ID item after each
// Collection(Application) open. The report IDs and prefixed payload shapes
// are shared contract with pkg/hid.

// keyboardReportDesc: boot-protocol keyboard, 8-byte reports (modifier byte,
// reserved byte, 6 keycodes). protocol=1, report_length=8.
var keyboardReportDesc = []byte{
	0x05, 0x01, 0x09, 0x06, 0xa1, 0x01, 0x05, 0x07,
	0x19, 0xe0, 0x29, 0xe7, 0x15, 0x00, 0x25, 0x01,
	0x75, 0x01, 0x95, 0x08, 0x81, 0x02, 0x95, 0x01,
	0x75, 0x08, 0x81, 0x03, 0x95, 0x05, 0x75, 0x01,
	0x05, 0x08, 0x19, 0x01, 0x29, 0x05, 0x91, 0x02,
	0x95, 0x01, 0x75, 0x03, 0x91, 0x03, 0x95, 0x06,
	0x75, 0x08, 0x15, 0x00, 0x25, 0x65, 0x05, 0x07,
	0x19, 0x00, 0x29, 0x65, 0x81, 0x00, 0xc0,
}

// mouseReportDesc: relative mouse, 4-byte reports (buttons, dx, dy, wheel).
// protocol=2, report_length=4.
var mouseReportDesc = []byte{
	0x05, 0x01, 0x09, 0x02, 0xa1, 0x01, 0x09, 0x01,
	0xa1, 0x00, 0x05, 0x09, 0x19, 0x01, 0x29, 0x03,
	0x15, 0x00, 0x25, 0x01, 0x95, 0x03, 0x75, 0x01,
	0x81, 0x02, 0x95, 0x01, 0x75, 0x05, 0x81, 0x03,
	0x05, 0x01, 0x09, 0x30, 0x09, 0x31, 0x09, 0x38,
	0x15, 0x81, 0x25, 0x7f, 0x75, 0x08, 0x95, 0x03,
	0x81, 0x06, 0xc0, 0xc0,
}

// touchpadReportDesc: absolute pointer, 6-byte reports (buttons, 16-bit X,
// 16-bit Y, wheel). protocol=2, report_length=6.
var touchpadReportDesc = []byte{
	0x05, 0x01, 0x09, 0x02, 0xa1, 0x01, 0x09, 0x01,
	0xa1, 0x00, 0x05, 0x09, 0x19, 0x01, 0x29, 0x03,
	0x15, 0x00, 0x25, 0x01, 0x95, 0x03, 0x75, 0x01,
	0x81, 0x02, 0x95, 0x01, 0x75, 0x05, 0x81, 0x01,
	0x05, 0x01, 0x09, 0x30, 0x09, 0x31, 0x15, 0x00,
	0x26, 0xff, 0x7f, 0x35, 0x00, 0x46, 0xff, 0x7f,
	0x75, 0x10, 0x95, 0x02, 0x81, 0x02, 0x05, 0x01,
	0x09, 0x38, 0x15, 0x81, 0x25, 0x7f, 0x35, 0x00,
	0x45, 0x00, 0x75, 0x08, 0x95, 0x01, 0x81, 0x06,
	0xc0, 0xc0,
}

// Report IDs within the combined mouse function. Shared contract with pkg/hid.
const (
	ReportIDRelMouse byte = 1
	ReportIDAbsMouse byte = 2
)

// withReportID inserts a Report ID item after the 6-byte collection prefix
// (Usage Page, Usage, Collection(Application)) each descriptor opens with.
func withReportID(desc []byte, id byte) []byte {
	out := make([]byte, 0, len(desc)+2)
	out = append(out, desc[:6]...)
	out = append(out, 0x85, id) // Report ID (id)
	out = append(out, desc[6:]...)
	return out
}

// combinedMouseReportDesc is the descriptor of the shared pointer function.
var combinedMouseReportDesc = func() []byte {
	var out []byte
	out = append(out, withReportID(mouseReportDesc, ReportIDRelMouse)...)
	out = append(out, withReportID(touchpadReportDesc, ReportIDAbsMouse)...)
	return out
}()

// hidSpec is one HID function's configfs shape (its report-descriptor + boot
// attributes). subclass (BIOS mode) and wakeup_on_write are applied separately
// from the gadget config.
type hidSpec struct {
	name         string // configfs function dir, e.g. "hid.GS0"
	protocol     int
	reportLength int
	reportDesc   []byte
}

// hidSpecs returns the ordered HID functions: the boot keyboard and the
// combined pointer function. The mouse function's report_length is its
// largest report including the ID byte (absolute: 1+6).
func hidSpecs() []hidSpec {
	return []hidSpec{
		{name: hidKeyboardFuncName, protocol: 1, reportLength: 8, reportDesc: keyboardReportDesc},
		{name: hidPointerFuncName, protocol: 2, reportLength: 7, reportDesc: combinedMouseReportDesc},
	}
}
