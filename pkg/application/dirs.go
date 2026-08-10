package application

import (
	"os"
	"path/filepath"
)

const (
	// GitHub repository for release downloads.
	GitHubOwner = "BMCPi"
	GitHubRepo  = "NanoKVM"

	// BuiltinAppDir is the copy of the app baked into the read-only squashfs
	// image. Never written to; it is the last-resort fallback when no
	// self-update has been installed (or every installed one is broken).
	BuiltinAppDir = "/kvmapp"

	// AppDir is where self-updates install to — on the persistent data
	// partition, because the root overlay is volatile (a write anywhere else
	// vanishes on reboot) and the baked-in copy is read-only. The init script
	// launches the first runnable of AppDir, BackupDir, BuiltinAppDir, so a
	// half-written update degrades to the previous (or factory) version
	// instead of bricking the service.
	AppDir = "/var/lib/nanokvm/app"

	// BackupDir holds the previous install for rollback; see AppDir.
	BackupDir = "/var/lib/nanokvm/app.prev"

	// CacheDir stages update downloads/uploads. On the data partition, NOT
	// under /root or /tmp: both are RAM-backed now (volatile overlay/tmpfs),
	// and a ~53 MB tarball plus its extraction would eat half the board's RAM.
	CacheDir = "/var/lib/nanokvm/cache"
)

// serverBinary is the relative path of the daemon inside an install dir.
const serverBinary = "server/NanoKVM-Server"

// ActiveAppDir returns the install directory the launcher would start:
// the first of AppDir, BackupDir, BuiltinAppDir that contains an executable
// server binary. Must mirror the cascade in the build's launcher script
// (meta-nanokvm recipes-nanokvm/nanokvm/files/nanokvm-server-run), which
// busybox init runs under an inittab ::respawn entry.
func ActiveAppDir() string {
	for _, dir := range []string{AppDir, BackupDir, BuiltinAppDir} {
		info, err := os.Stat(filepath.Join(dir, serverBinary))
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return dir
		}
	}
	return BuiltinAppDir
}
