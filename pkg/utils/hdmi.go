package utils

import (
	"log/slog"
	"os"
)

const (
	HDMIDisableFile = "/etc/kvm/hdmi_disable"
)

func PersistHDMIDisabled() {
	f, err := os.OpenFile(HDMIDisableFile, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		slog.Error("failed to create hdmi disable file", slog.Any("err", err))
		return
	}
	f.Close()
}

func PersistHDMIEnabled() {
	if err := os.Remove(HDMIDisableFile); err != nil {
		slog.Error("failed to remove hdmi disable file", slog.Any("err", err))
		return
	}
}

func IsHdmiDisabled() bool {
	if _, err := os.Stat(HDMIDisableFile); err != nil {
		if os.IsNotExist(err) {
			return false // HDMI is enabled
		}
		slog.Error("failed to check hdmi disable file", slog.Any("err", err))
		return false // Assume HDMI is enabled on error
	}
	return true // HDMI is disabled
}
