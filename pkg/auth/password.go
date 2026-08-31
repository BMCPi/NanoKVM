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

	if err := s.changeRootPassword(s.rootCtx, plainPassword); err != nil {
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

func (s *Service) changeRootPassword(ctx context.Context, password string) error {
	err := passwd(ctx, password)
	if err != nil {
		s.log.Error("failed to change root password", slog.Any("err", err))
		return err
	}

	s.log.Debug("change root password successful")
	return nil
}

func passwd(ctx context.Context, password string) error {
	// Bounded well past what a healthy `passwd` takes, but bounded
	// nonetheless: rewriting /etc/shadow mustn't be interrupted casually, so
	// this is a generous timeout rather than the request's own (often much
	// shorter) deadline -- ctx here is the Service's root ctx, not a caller's
	// request context, precisely so a client disconnecting mid-request can't
	// cut this off halfway through.
	execCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
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
