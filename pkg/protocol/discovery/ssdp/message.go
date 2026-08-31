// Package ssdp parses M-SEARCH requests and builds SSDP responses and
// NOTIFY announcements. It is deliberately socket-free: the responder that
// actually listens on the UPnP multicast group lives elsewhere, so this
// package can be exercised with plain unit tests instead of a live network.
package ssdp

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"strconv"
)

// SearchTargets this responder answers.
const (
	TargetAll        = "ssdp:all"
	TargetRootDevice = "upnp:rootdevice"
	TargetRedfish    = "urn:dmtf-org:service:redfish-rest:1"

	multicastAddr = "239.255.255.250:1900"
)

// RedfishTargetVersioned returns "urn:dmtf-org:service:redfish-rest:1:<minor>".
func RedfishTargetVersioned(minor int) string {
	return fmt.Sprintf("%s:%d", TargetRedfish, minor)
}

// Search is a parsed M-SEARCH we are willing to answer.
type Search struct {
	ST string
	MX int // seconds; already clamped to [0, 5]
}

// ParseSearch returns a Search when the datagram is an M-SEARCH with
// MAN: "ssdp:discover" and an ST this responder serves, and ok=false for
// everything else — a UPnP segment is full of traffic that is not for us.
func ParseSearch(raw []byte, minor int) (Search, bool) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return Search{}, false
	}
	if req.Method != "M-SEARCH" {
		return Search{}, false
	}
	// The quotes are part of the SSDP-defined header value, not HTTP quoting
	// syntax, so they are compared literally.
	if req.Header.Get("MAN") != `"ssdp:discover"` {
		return Search{}, false
	}

	st := req.Header.Get("ST")
	if !isServedTarget(st, minor) {
		return Search{}, false
	}

	return Search{ST: st, MX: clampMX(req.Header.Get("MX"))}, true
}

func isServedTarget(st string, minor int) bool {
	switch st {
	case TargetAll, TargetRootDevice, TargetRedfish, RedfishTargetVersioned(minor):
		return true
	default:
		return false
	}
}

// clampMX enforces the [0, 5] second ceiling: a client asking us to hold off
// for a minute before answering does not get to, and a missing or malformed
// MX defaults to the minimum useful delay rather than being rejected outright.
func clampMX(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 1
	}
	if n < 0 {
		return 0
	}
	if n > 5 {
		return 5
	}
	return n
}

// usn builds the USN header value shared by Response and Notify.
func usn(uuid, target string) string {
	return "uuid:" + uuid + "::" + target
}

// Response builds the unicast 200 OK answer. Built with an explicit
// bytes.Buffer rather than http.Response.Write, which emits headers (and a
// status line format) DMTF's reference client does not expect.
func Response(uuid, location string, minor, maxAge int) []byte {
	target := RedfishTargetVersioned(minor)

	var b bytes.Buffer
	b.WriteString("HTTP/1.1 200 OK\r\n")
	fmt.Fprintf(&b, "CACHE-CONTROL: max-age=%d\r\n", maxAge)
	b.WriteString("EXT:\r\n")
	// LOCATION is what generic UPnP browsers read; AL is what DMTF's own
	// client reads, and it reads nothing else. Both carry the same value.
	fmt.Fprintf(&b, "LOCATION: %s\r\n", location)
	fmt.Fprintf(&b, "AL: %s\r\n", location)
	fmt.Fprintf(&b, "ST: %s\r\n", target)
	fmt.Fprintf(&b, "USN: %s\r\n", usn(uuid, target))
	b.WriteString("\r\n")
	return b.Bytes()
}

// Notify builds a multicast NOTIFY. alive=false produces ssdp:byebye, which
// omits AL/LOCATION: a device announcing it is leaving has nothing to direct
// clients to.
func Notify(uuid, location string, minor, maxAge int, alive bool) []byte {
	target := RedfishTargetVersioned(minor)

	var b bytes.Buffer
	b.WriteString("NOTIFY * HTTP/1.1\r\n")
	fmt.Fprintf(&b, "HOST: %s\r\n", multicastAddr)
	fmt.Fprintf(&b, "NT: %s\r\n", target)
	if alive {
		fmt.Fprintf(&b, "CACHE-CONTROL: max-age=%d\r\n", maxAge)
		b.WriteString("NTS: ssdp:alive\r\n")
		fmt.Fprintf(&b, "LOCATION: %s\r\n", location)
		fmt.Fprintf(&b, "AL: %s\r\n", location)
	} else {
		b.WriteString("NTS: ssdp:byebye\r\n")
	}
	fmt.Fprintf(&b, "USN: %s\r\n", usn(uuid, target))
	b.WriteString("\r\n")
	return b.Bytes()
}
