package utils

import (
	"log/slog"
	"os"
)

const (
	HDMIDisableFile = "/etc/kvm/hdmi_disable"
)

func PersistHDMIDisabled(log *slog.Logger) {
	f, err := os.OpenFile(HDMIDisableFile, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		log.Error("failed to create hdmi disable file", slog.Any("err", err))
		return
	}
	f.Close()
}

func PersistHDMIEnabled(log *slog.Logger) {
	if err := os.Remove(HDMIDisableFile); err != nil {
		log.Error("failed to remove hdmi disable file", slog.Any("err", err))
		return
	}
}

func IsHdmiDisabled(log *slog.Logger) bool {
	if _, err := os.Stat(HDMIDisableFile); err != nil {
		if os.IsNotExist(err) {
			return false // HDMI is enabled
		}
		log.Error("failed to check hdmi disable file", slog.Any("err", err))
		return false // Assume HDMI is enabled on error
	}
	return true // HDMI is disabled
}
