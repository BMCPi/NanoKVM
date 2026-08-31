package application

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/pi-bmc/nanokvm-app/pkg/platform/streamio"
)

var (
	mutex      sync.Mutex
	isUpdating bool
)

func acquireUpdateLock() bool {
	mutex.Lock()
	defer mutex.Unlock()

	if isUpdating {
		return false
	}
	isUpdating = true
	return true
}

func releaseUpdateLock() {
	mutex.Lock()
	defer mutex.Unlock()
	isUpdating = false
}

func installPackage(log *slog.Logger, source string) error {
	// Extract into a dedicated subdir of CacheDir so the downloaded tarball
	// (which lives directly under CacheDir) isn't swept into AppDir below.
	extractDir := filepath.Join(CacheDir, "extracted")
	_ = os.RemoveAll(extractDir)

	dir, err := streamio.UnTarGz(source, extractDir)
	if err != nil {
		return fmt.Errorf("failed to decompress app: %w", err)
	}

	if err := backupCurrentApp(); err != nil {
		return err
	}

	if err := applyUpdate(log, dir); err != nil {
		return err
	}

	if err := ChmodRecursively(AppDir, 0o755); err != nil {
		return fmt.Errorf("failed to chmod: %w", err)
	}

	return nil
}

func backupCurrentApp() error {
	if err := os.RemoveAll(BackupDir); err != nil {
		return fmt.Errorf("failed to remove backup: %w", err)
	}

	if err := MoveFilesRecursively(AppDir, BackupDir); err != nil {
		return fmt.Errorf("failed to backup app: %w", err)
	}

	return nil
}

func applyUpdate(log *slog.Logger, sourceDir string) error {
	if err := MoveFilesRecursively(sourceDir, AppDir); err != nil {
		// Try to restore backup on failure
		if restoreErr := MoveFilesRecursively(BackupDir, AppDir); restoreErr != nil {
			log.Error("failed to restore backup after update failure", slog.Any("err", restoreErr))
		}
		return fmt.Errorf("failed to move update in place: %w", err)
	}
	return nil
}
