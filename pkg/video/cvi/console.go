package cvi

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Keeping a driver's error path from taking the board down.
//
// The media drivers report back-pressure one line at a time, per frame. When
// the encoder falls behind, vb_qbuf calls CVI_TRACE_BASE(CVI_BASE_DBG_ERR),
// which is pr_err -- KERN_ERR, level 3 -- once for every frame it drops. At
// 60fps that is 60 KERN_ERR a second, and on a board whose console is a
// 115200 serial line each one costs milliseconds of synchronous, in-kernel
// write on the only core there is.
//
// The result is not a noisy log. It is a dead board: ping still answers,
// because that is handled in softirq, but userspace stops being scheduled
// long enough to matter. SSH and HTTP never complete a handshake, the serial
// shell stops echoing, SysRq gets no response, and the only way back is a
// power cycle. That has happened three times on this hardware, and it is a
// particularly bad failure for a BMC, where physical access is the thing the
// device exists to avoid needing.
//
// Nothing is lost by silencing it. The messages still go to the kernel ring
// buffer, so dmesg -- which is how anyone actually reads these -- is
// unaffected. Only the synchronous write to the serial console goes away, and
// that is precisely the expensive part.
//
// This is a blunt instrument aimed at one specific self-inflicted wound, and
// it is global state owned by no single subsystem, so it is worth being able
// to turn off: NANOKVM_KEEP_CONSOLE_LOGLEVEL=1 leaves it alone for anyone
// debugging over the serial console who would rather have the messages.
//
// The proper home for this is the kernel command line (loglevel=), which
// applies from boot rather than from whenever the video pipeline first comes
// up. Until that is in the image, this covers the window that matters.
const (
	printkPath = "/proc/sys/kernel/printk"

	// Below KERN_ERR, so the per-frame drop messages stop reaching the
	// console. KERN_CRIT and worse still get through, which is the level at
	// which someone reading a serial console genuinely needs to see it.
	quietConsoleLevel = 3
)

// consoleLogLevel reads the current console loglevel.
//
// The file is four numbers -- console, default, minimum, boot-default -- and
// only the first is the one in question.
func consoleLogLevel() (int, error) {
	raw, err := os.ReadFile(printkPath)
	if err != nil {
		return 0, fmt.Errorf("cvi: read %s: %w", printkPath, err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, fmt.Errorf("cvi: %s is empty", printkPath)
	}
	lvl, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("cvi: parse %s: %w", printkPath, err)
	}
	return lvl, nil
}

// setConsoleLogLevel writes the console loglevel. Writing a single value to
// this file sets that field alone; the other three are left as they are.
func setConsoleLogLevel(lvl int) error {
	if err := os.WriteFile(printkPath, []byte(strconv.Itoa(lvl)), 0); err != nil {
		return fmt.Errorf("cvi: write %s: %w", printkPath, err)
	}
	return nil
}

// quietKernelConsole lowers the console loglevel past the drivers' per-frame
// error reporting, and returns a function that puts it back.
//
// A failure here is reported and otherwise ignored: not being able to write a
// sysctl is a reason to carry on without the protection, not a reason to
// refuse to capture video.
func quietKernelConsole() (restore func()) {
	if _, keep := envUint("NANOKVM_KEEP_CONSOLE_LOGLEVEL"); keep {
		return func() {}
	}

	was, err := consoleLogLevel()
	if err != nil || was <= quietConsoleLevel {
		return func() {}
	}
	if err := setConsoleLogLevel(quietConsoleLevel); err != nil {
		return func() {}
	}
	return func() { _ = setConsoleLogLevel(was) }
}
