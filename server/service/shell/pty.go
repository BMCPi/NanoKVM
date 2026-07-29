// Package shell runs local shell sessions on the BMC. It is the single
// implementation behind both front ends that need one: the web terminal
// drawer (server/service/vm.Shell, over a WebSocket) and the in-process SSH
// server (server/service/ssh).
package shell

import (
	"fmt"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// shellCandidates are tried in order when picking the login shell. The image
// ships bash; busybox's ash is the fallback.
var shellCandidates = []string{"/bin/bash", "/bin/sh"}

// LoginShell returns the shell to run sessions with.
func LoginShell() string {
	for _, sh := range shellCandidates {
		if fi, err := os.Stat(sh); err == nil && !fi.IsDir() {
			return sh
		}
	}
	return "/bin/sh"
}

// Dir is the session's working directory — /root when it exists (the BMC
// runs as root), otherwise the filesystem root.
func Dir() string {
	if fi, err := os.Stat("/root"); err == nil && fi.IsDir() {
		return "/root"
	}
	return "/"
}

// baseEnv is the environment every session starts from. Callers append the
// client's TERM and any per-session variables.
func baseEnv(shell string) []string {
	return []string{
		"HOME=" + Dir(),
		"SHELL=" + shell,
		"USER=root",
		"LOGNAME=root",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
}

// openPTY allocates a pseudo-terminal pair the way openpty(3) does, without
// pulling in cgo: open the multiplexer, unlock the slave, resolve its index.
func openPTY() (*os.File, *os.File, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	fd := int(ptmx.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		_ = ptmx.Close()
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}

	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		_ = ptmx.Close()
		return nil, nil, fmt.Errorf("ptsname: %w", err)
	}

	pts, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = ptmx.Close()
		return nil, nil, fmt.Errorf("open pts: %w", err)
	}

	return ptmx, pts, nil
}
