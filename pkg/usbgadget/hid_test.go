package usbgadget

import (
	"bytes"
	"strconv"
	"testing"
)

// The exact `echo -ne` report-descriptor escape strings from the original
// packaging/etc/init.d/S03usbdev shell script. Parsing these independently and
// comparing to the Go descriptors proves the migration is byte-for-byte
// faithful. Raw string literals so Go does not interpret the \x escapes.
const (
	s03Keyboard = `\x05\x01\x09\x06\xa1\x01\x05\x07\x19\xe0\x29\xe7\x15\x00\x25\x01\x75\x01\x95\x08\x81\x02\x95\x01\x75\x08\x81\x03\x95\x05\x75\x01\x05\x08\x19\x01\x29\x05\x91\x02\x95\x01\x75\x03\x91\x03\x95\x06\x75\x08\x15\x00\x25\x65\x05\x07\x19\x00\x29\x65\x81\x00\xc0`
	s03Mouse    = `\x5\x1\x9\x2\xa1\x1\x9\x1\xa1\x0\x5\x9\x19\x1\x29\x3\x15\x0\x25\x1\x95\x3\x75\x1\x81\x2\x95\x1\x75\x5\x81\x3\x5\x1\x9\x30\x9\x31\x9\x38\x15\x81\x25\x7f\x75\x8\x95\x3\x81\x6\xc0\xc0`
	s03Touchpad = `\x05\x01\x09\x02\xa1\x01\x09\x01\xa1\x00\x05\x09\x19\x01\x29\x03\x15\x00\x25\x01\x95\x03\x75\x01\x81\x02\x95\x01\x75\x05\x81\x01\x05\x01\x09\x30\x09\x31\x15\x00\x26\xff\x7f\x35\x00\x46\xff\x7f\x75\x10\x95\x02\x81\x02\x05\x01\x09\x38\x15\x81\x25\x7f\x35\x00\x45\x00\x75\x08\x95\x01\x81\x06\xc0\xc0`
)

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// parseEscaped decodes a shell `echo -ne` string of \xNN / \xN byte escapes.
// It reads up to two hex digits after each \x (greedy); this is unambiguous
// because every value in the source is a \x escape, so a single-digit escape is
// always followed by a backslash or end-of-string.
func parseEscaped(s string) []byte {
	var out []byte
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == 'x' {
			j := i + 2
			hex := ""
			for len(hex) < 2 && j < len(s) && isHexDigit(s[j]) {
				hex += string(s[j])
				j++
			}
			v, _ := strconv.ParseUint(hex, 16, 8)
			out = append(out, byte(v))
			i = j
			continue
		}
		i++
	}
	return out
}

func TestReportDescriptorsMatchS03usbdev(t *testing.T) {
	cases := []struct {
		name string
		desc []byte
		s03  string
		want int
	}{
		{"keyboard", keyboardReportDesc, s03Keyboard, 63},
		{"mouse", mouseReportDesc, s03Mouse, 52},
		{"touchpad", touchpadReportDesc, s03Touchpad, 74},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.desc) != tc.want {
				t.Fatalf("%s descriptor length = %d, want %d", tc.name, len(tc.desc), tc.want)
			}
			parsed := parseEscaped(tc.s03)
			if !bytes.Equal(tc.desc, parsed) {
				t.Fatalf("%s descriptor differs from S03usbdev bytes\n go:  %x\n s03: %x", tc.name, tc.desc, parsed)
			}
		})
	}
}

func TestHIDSpecs(t *testing.T) {
	specs := hidSpecs()
	want := []struct {
		name         string
		protocol     int
		reportLength int
		desc         []byte
	}{
		{"hid.GS0", 1, 8, keyboardReportDesc},
		{"hid.GS1", 2, 4, mouseReportDesc},
		{"hid.GS2", 2, 6, touchpadReportDesc},
	}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d", len(specs), len(want))
	}
	for i, w := range want {
		if specs[i].name != w.name || specs[i].protocol != w.protocol || specs[i].reportLength != w.reportLength {
			t.Errorf("spec %d = %+v, want name=%s protocol=%d len=%d", i, specs[i], w.name, w.protocol, w.reportLength)
		}
		if !bytes.Equal(specs[i].reportDesc, w.desc) {
			t.Errorf("spec %d report descriptor mismatch", i)
		}
	}
}
