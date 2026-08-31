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
func (s *Service) ChangePassword(username, plainPassword string) error {
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	if err := s.SetAccount(username, string(hashedPassword)); err != nil {
		return err
	}

	if err := s.changeRootPassword(plainPassword); err != nil {
		_ = s.DelAccount()
		return err
	}

	s.log.Debug("change password success", slog.String("username", username))
	return nil
}

// IsPasswordUpdated reports whether the default admin password has been
// changed: false while no account file exists or while the stored hash
// still matches "admin".
func (s *Service) IsPasswordUpdated() (bool, error) {
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

	account, err := s.GetAccount()
	if err != nil || account == nil {
		return false, err
	}

	err = bcrypt.CompareHashAndPassword([]byte(account.Password), []byte("admin"))

	// If the hash is not valid, still assume it's not updated. The error
	// we want to see is password and hash not matching.
	return errors.Is(err, bcrypt.ErrMismatchedHashAndPassword), nil
}

func (s *Service) changeRootPassword(password string) error {
	err := passwd(password)
	if err != nil {
		s.log.Error("failed to change root password", slog.Any("err", err))
		return err
	}

	s.log.Debug("change root password successful")
	return nil
}

// passwdTimeout bounds the passwd exec. Generous relative to what a healthy
// `passwd` takes, but a wedged one must not be able to pin the caller's
// goroutine forever.
const passwdTimeout = 15 * time.Second

func passwd(password string) error {
	// Deliberately detached from the Service's rootCtx (and therefore from
	// SIGTERM), not derived from it: rootCtx is cancelled at shutdown, and if
	// this exec inherited that cancellation, a shutdown mid-rewrite of
	// /etc/shadow would kill `passwd` partway through, and ChangePassword's
	// caller would then roll back the account file via DelAccount() —
	// reverting auth to the default credentials. context.Background() plus
	// its own bound gives both properties this needs: bounded, so a wedged
	// passwd cannot pin the request goroutine, and shutdown-immune, because
	// interrupting mid-shadow-rewrite is worse than a delayed process exit.
	execCtx, cancel := context.WithTimeout(context.Background(), passwdTimeout)
	defer cancel()
	cmd := exec.CommandContext(execCtx, "passwd", "root")

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
