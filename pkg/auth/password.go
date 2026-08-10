package auth

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	log "github.com/sirupsen/logrus"
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

	log.Debugf("change password success, username: %s", username)
	return nil
}

// IsPasswordUpdated reports whether the default admin password has been
// changed: false while no account file exists or while the stored hash
// still matches "admin".
func IsPasswordUpdated() (bool, error) {
	if _, err := os.Stat(accountFile); err != nil {
		return false, nil
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
		log.Errorf("failed to change root password: %s", err)
		return err
	}

	log.Debugf("change root password successful.")
	return nil
}

func passwd(password string) error {
	cmd := exec.Command("passwd", "root")

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

	if err = cmd.Wait(); err != nil {
		return err
	}

	return nil
}
