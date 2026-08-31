package ssh

import (
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/pi-bmc/nanokvm-app/pkg/shell"
)

// serveSFTP runs the in-process SFTP server on a session channel. The image
// ships no sftp-server binary, so the subsystem is served from this process —
// which is also what makes `scp` work: OpenSSH 9.0 and later speak SFTP for
// scp by default rather than the legacy RCP protocol.
//
// Access is the whole filesystem as root, matching what a shell on this
// server already grants; the gate is authentication, not the subsystem.
func serveSFTP(ch ssh.Channel) {
	// Relative paths (`scp file bmc:`, `sftp bmc` then `put`) must land where
	// a shell session starts rather than in the server process's cwd, which
	// is wherever init happened to launch it.
	server, err := sftp.NewServer(ch,
		sftp.WithServerWorkingDirectory(shell.Dir()),
		// The library writes exactly one kind of message here: the handles a
		// client left open when it vanished mid-transfer. Serve closes them
		// itself, but on a BMC that runs for months the report is worth
		// having rather than discarding (the library's default).
		sftp.WithDebug(debugWriter{}),
	)
	if err != nil {
		slog.Error("ssh: sftp server failed", slog.Any("err", err))
		exitSession(ch, 1)
		return
	}

	slog.Debug("ssh: sftp subsystem started", slog.String("dir", shell.Dir()))

	// Serve runs until the client closes the channel. A clean disconnect
	// surfaces as EOF and is the normal end of a transfer, not a failure.
	if err := server.Serve(); err != nil && !errors.Is(err, io.EOF) {
		slog.Warn("ssh: sftp session ended", slog.Any("err", err))
		exitSession(ch, 1)
		return
	}

	exitSession(ch, 0)
}

// debugWriter adapts the SFTP library's io.Writer diagnostics onto the app's
// logger.
type debugWriter struct{}

func (debugWriter) Write(p []byte) (int, error) {
	slog.Warn("ssh: sftp", slog.String("msg", strings.TrimRight(string(p), "\n")))
	return len(p), nil
}
