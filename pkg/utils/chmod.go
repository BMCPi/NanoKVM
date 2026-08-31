package utils

import (
	"os"
	"path/filepath"
)

func ChmodRecursively(path string, mode uint32) error {
	return filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			// G122 wants the walk re-scoped through os.Root so the window
			// between Walk's lstat and this chmod cannot be won by a symlink
			// swap. There is no second party to win it here: the sole caller
			// is the application installer (pkg/application/install.go), which
			// walks /var/lib/nanokvm/app immediately after unpacking an update
			// into it. That tree is written by this process alone, on a data
			// partition no other component touches, and the update it came
			// from is either a release asset from this project's own GitHub or
			// an upload from a session already authenticated to replace the
			// root-owned binary this process is. Nothing less privileged than
			// the process doing the chmod is in the race. The walk can
			// legitimately contain symlinks the archive itself authored (see
			// untar.go, which recreates tar.TypeSymlink entries verbatim,
			// absolute targets included), and os.Chmod follows them rather
			// than changing the link itself; that is accepted, not hardened,
			// because the archive supplier is already trusted to replace this
			// root binary.
			//nolint:gosec // G122: walked tree, symlinks included, is written by this process alone from an already-trusted archive; no lower-privileged racer exists
			err = os.Chmod(path, os.FileMode(mode))
			if err != nil {
				return err
			}
		}

		return nil
	})
}
