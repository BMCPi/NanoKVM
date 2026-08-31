package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ChangePassword updates both credentials the BMC authenticates against:
// the web/SSH account file and the root shell password, which must not
// diverge. On a root-password failure the account write is rolled back so
// the two stay consistent.
func ChangePassword(username, plainPassword string) error {
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	if err := SetAccount(username, string(hashedPassword)); err != nil {
		return err
	}

	if err := changeRootPassword(plainPassword); err != nil {
		_ = DelAccount()
		return err
	}

	slog.Debug("change password success", slog.String("username", username))
	return nil
}

// IsPasswordUpdated reports whether the default admin password has been
// changed: false while no account file exists or while the stored hash
// still matches "admin".
func IsPasswordUpdated() (bool, error) {
	if _, err := os.Stat(accountFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No account file at all: the device is still on the shipped
			// default credential. This is the expected pre-setup state, not
			// a failure, so it is reported as "not updated" with no error.
			return false, nil
		}
		// Any other stat failure (permissions, I/O) says nothing about the
		// password. Surface it rather than let an unreadable account file be
		// indistinguishable from a device that was never configured — the
		// same split GetAccount already makes.
		return false, err
	}

	account, err := GetAccount()
	if err != nil || account == nil {
		return false, err
	}

	err = bcrypt.CompareHashAndPassword([]byte(account.Password), []byte("admin"))

	// If the hash is not valid, still assume it's not updated. The error
	// we want to see is password and hash not matching.
	return errors.Is(err, bcrypt.ErrMismatchedHashAndPassword), nil
}

func changeRootPassword(password string) error {
	err := passwd(password)
	if err != nil {
		slog.Error("failed to change root password", slog.Any("err", err))
		return err
	}

	slog.Debug("change root password successful")
	return nil
}

func passwd(password string) error {
	// context.Background(): nothing above this call carries a context today
	// (ChangePassword is exported without one), and passwd must not be
	// cancelled halfway through rewriting /etc/shadow, so the conversion to
	// CommandContext is mechanical and leaves the lifetime unchanged.
	cmd := exec.CommandContext(context.Background(), "passwd", "root")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer func() {
		_ = stdin.Close()
	}()

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err = cmd.Start(); err != nil {
		return err
	}

	if _, err = io.WriteString(stdin, password+"\n"); err != nil {
		return err
	}

	time.Sleep(100 * time.Millisecond)

	if _, err = io.WriteString(stdin, password+"\n"); err != nil {
		return err
	}

	return cmd.Wait()
}
