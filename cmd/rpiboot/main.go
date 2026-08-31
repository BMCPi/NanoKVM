// rpiboot pushes a second-stage boot payload (e.g. U-Boot) into the
// Raspberry Pi 5 BootROM over USB. It talks to the kernel's usbfs
// directly via ioctl(2) so the binary can be cross-compiled for
// riscv64 without CGo or libusb.
//
// This is a riscv64 binary that ships inside the device image itself
// (Makefile's `deploy` target and .goreleaser.yaml both build it for
// linux/riscv64 and package it at system/usr/bin/rpiboot alongside
// NanoKVM-Server) — it runs on the BMC, against the BMC's own USB port
// and the Raspberry Pi 5 compute module physically attached to it, not
// on an operator's workstation. Nothing in this repo or the Yocto build
// tree (nanokvm-build) execs it automatically; no caller was found in
// pkg/ or api/. Its argv therefore comes only from whoever has a local
// shell on the BMC (SSH per pkg/ssh, or the serial console) invoking it
// by hand — never from an HTTP request or any other network input.
package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// Broadcom BootROM VID and PID for BCM2712 (Raspberry Pi 5).
	// The PID changes once the second stage executes.
	piBootVID uint16 = 0x0a5c
	piBootPID uint16 = 0x2712

	// Standard USB Bulk endpoints exposed by the BootROM.
	// IN endpoints have bit 7 set in the address.
	epOutAddr uint8 = 0x01
	epInAddr  uint8 = 0x82

	defaultInterface = 0
)

// usbdevfs ioctl numbers. The BULK ioctl is direction=read|write,
// type='U', nr=2, size=sizeof(usbdevfsBulkTransfer). We compute the
// ioctl number at runtime so the code is correct on both 32- and
// 64-bit Linux (the struct contains a pointer whose width changes).
var (
	usbdevfsClaimInterface   = ior('U', 15, unsafe.Sizeof(uint32(0)))
	usbdevfsReleaseInterface = ior('U', 16, unsafe.Sizeof(uint32(0)))
	usbdevfsBulk             = iowr('U', 2, unsafe.Sizeof(usbdevfsBulkTransfer{}))
)

// usbdevfsBulkTransfer mirrors `struct usbdevfs_bulktransfer` from
// <linux/usbdevice_fs.h>:
//
//	struct usbdevfs_bulktransfer {
//	    unsigned int  ep;
//	    unsigned int  len;
//	    unsigned int  timeout;   /* in milliseconds */
//	    void         *data;
//	};
//
// On 64-bit platforms the C compiler inserts 4 bytes of padding before
// the pointer to align it to 8 bytes. We match that with an explicit
// pad field on 64-bit only.
type usbdevfsBulkTransfer struct {
	Endpoint uint32
	Length   uint32
	Timeout  uint32
	pad      [bulkPad]byte
	Data     uintptr
}

const bulkPad = (^uint(0) >> 32) & 1 * 4 // 4 on 64-bit, 0 on 32-bit

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: %s <payload.bin>", os.Args[0]) //nolint:gosec // G706: os.Args[0] is this BMC-resident binary's own argv[0], set by whoever invoked it from a local shell on the device — not data that arrived over the network, so there is nothing to inject into this process's own stdout
	}
	payloadPath := os.Args[1]

	if runtime.GOOS != "linux" {
		log.Fatalf("rpiboot requires Linux usbfs (got GOOS=%s)", runtime.GOOS)
	}

	log.Println("Scanning for Raspberry Pi 5 in BootROM mode...")

	devPath, err := findUSBDevice(piBootVID, piBootPID)
	if err != nil {
		log.Fatalf("Failed to locate device: %v", err)
	}
	log.Printf("Found BCM2712 BootROM device at %s", devPath)

	fd, err := unix.Open(devPath, unix.O_RDWR, 0)
	if err != nil {
		log.Fatalf("Failed to open %s: %v", devPath, err)
	}
	defer unix.Close(fd)

	if err := claimInterface(fd, defaultInterface); err != nil {
		log.Fatalf("Failed to claim interface %d: %v", defaultInterface, err) //nolint:gocritic // exitAfterDefer: log.Fatalf's os.Exit skips the deferred unix.Close(fd), but the kernel closes every fd on process exit regardless — the defer buys nothing that exiting doesn't already do
	}
	defer func() {
		if err := releaseInterface(fd, defaultInterface); err != nil {
			log.Printf("warning: release interface: %v", err)
		}
	}()

	// Step A: read the ASIC ID handshake.
	asicID := make([]byte, 4)
	n, err := bulkTransfer(fd, epInAddr, asicID, 5000)
	if err != nil {
		log.Fatalf("Failed to read ASIC ID: %v", err)
	}
	log.Printf("Received ASIC ID handshake (%d bytes): %x", n, asicID[:n])

	// Step B: load the payload.
	payloadData, err := os.ReadFile(payloadPath) //nolint:gosec // G703: payloadPath is os.Args[1] on this BMC-resident binary; no caller in this repo or nanokvm-build was found passing it a network- or request-derived value, and reading whatever file the local invoker names is the tool's entire purpose — not a boundary crossing for whoever already has a shell on the device
	if err != nil {
		log.Fatalf("Failed to read payload file: %v", err)
	}

	// Step C: push to RAM.
	if err := pushPayload(fd, payloadData); err != nil {
		log.Fatalf("Failed to push payload: %v", err)
	}

	log.Println("Payload pushed. Waiting for device to execute and re-enumerate...")
	time.Sleep(2 * time.Second)
}

// pushPayload sends the 4-byte little-endian length header followed by
// the raw payload bytes on the bulk OUT endpoint.
func pushPayload(fd int, data []byte) error {
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(len(data))) //nolint:gosec // G115: data is the boot payload (U-Boot image) read from the local file named on this BMC-resident binary's own argv; it is nowhere near the 4GiB uint32 range in practice, and the wire header is a hard 4-byte field this tool must fill regardless

	if _, err := bulkTransfer(fd, epOutAddr, header, 5000); err != nil {
		return fmt.Errorf("send size header: %w", err)
	}
	log.Printf("Sent payload size header: %d bytes", len(data)) //nolint:gosec // G706: len(data) is a byte count formatted with %d, not text, and it is printed to this process's own stdout on the BMC — there is no injectable content and no remote reader

	written, err := bulkTransfer(fd, epOutAddr, data, 30000)
	if err != nil {
		return fmt.Errorf("send payload data: %w", err)
	}
	if written != len(data) {
		return fmt.Errorf("incomplete write: sent %d of %d bytes", written, len(data))
	}
	log.Println("Payload transfer complete.")
	return nil
}

// findUSBDevice scans /sys/bus/usb/devices for a device matching the
// given VID/PID and returns the matching /dev/bus/usb/BBB/DDD path.
func findUSBDevice(vid, pid uint16) (string, error) {
	entries, err := os.ReadDir("/sys/bus/usb/devices")
	if err != nil {
		return "", fmt.Errorf("list usb devices: %w", err)
	}
	for _, e := range entries {
		base := filepath.Join("/sys/bus/usb/devices", e.Name())
		gotVID, err := readHexFile(filepath.Join(base, "idVendor"))
		if err != nil {
			continue // not a device node (e.g. a "usbN" bus root or interface)
		}
		gotPID, err := readHexFile(filepath.Join(base, "idProduct"))
		if err != nil {
			continue
		}
		if gotVID != uint64(vid) || gotPID != uint64(pid) {
			continue
		}
		busnum, err := readIntFile(filepath.Join(base, "busnum"))
		if err != nil {
			return "", fmt.Errorf("read busnum: %w", err)
		}
		devnum, err := readIntFile(filepath.Join(base, "devnum"))
		if err != nil {
			return "", fmt.Errorf("read devnum: %w", err)
		}
		return fmt.Sprintf("/dev/bus/usb/%03d/%03d", busnum, devnum), nil
	}
	return "", fmt.Errorf("no USB device with VID:PID %04x:%04x (is the BOOT button held?)", vid, pid)
}

func readHexFile(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 16, 32)
}

func readIntFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func claimInterface(fd, intf int) error {
	return unix.IoctlSetPointerInt(fd, usbdevfsClaimInterface, intf)
}

func releaseInterface(fd, intf int) error {
	return unix.IoctlSetPointerInt(fd, usbdevfsReleaseInterface, intf)
}

// bulkTransfer issues a USBDEVFS_BULK ioctl. Direction is encoded in
// the high bit of ep (0x80 = IN). Returns the number of bytes actually
// transferred.
func bulkTransfer(fd int, ep uint8, buf []byte, timeoutMs uint32) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	t := usbdevfsBulkTransfer{
		Endpoint: uint32(ep),
		Length:   uint32(len(buf)), //nolint:gosec // G115: buf is at most the payload size read from the local file named on argv (see pushPayload); the usbdevfs_bulktransfer.len field this feeds is a uint32 on the wire, so the conversion is required, not incidental
		Timeout:  timeoutMs,
		Data:     uintptr(unsafe.Pointer(&buf[0])),
	}
	err := unix.IoctlSetInt(fd, usbdevfsBulk, int(uintptr(unsafe.Pointer(&t))))
	r := t.Length
	if err != nil {
		return 0, fmt.Errorf("USBDEVFS_BULK ep=0x%02x: %w", ep, err)
	}
	// Keep buf alive across the syscall.
	runtime.KeepAlive(buf)
	return int(r), nil
}

// _IOC encoding (see linux/ioctl.h).
const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits

	iocRead  = 2
	iocWrite = 1
)

func ioc(dir, typ, nr, size uintptr) uint {
	return uint((dir << iocDirShift) | (typ << iocTypeShift) | (nr << iocNRShift) | (size << iocSizeShift))
}

func ior(typ, nr, size uintptr) uint  { return ioc(iocRead, typ, nr, size) }
func iowr(typ, nr, size uintptr) uint { return ioc(iocRead|iocWrite, typ, nr, size) }
