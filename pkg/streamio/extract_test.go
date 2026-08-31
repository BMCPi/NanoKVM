package streamio

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The extraction helpers unpack application update packages, so the property
// that matters most is that an ordinary package still comes out byte-for-byte
// and mode-for-mode. The size cap is exercised through the budget-parameter
// variants so the test does not have to produce 256 MiB of fixture.

// writeTarGz builds a .tar.gz containing one directory and the named regular
// files, and returns its path.
func writeTarGz(t *testing.T, files map[string][]byte) string {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	if err := tw.WriteHeader(&tar.Header{
		Name: "server/", Typeflag: tar.TypeDir, Mode: 0o755,
	}); err != nil {
		t.Fatalf("write dir header: %v", err)
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body)),
		}); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	path := filepath.Join(t.TempDir(), "package.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	return path
}

func TestUnTarGzExtractsPackage(t *testing.T) {
	body := bytes.Repeat([]byte("nanokvm"), 4096)
	src := writeTarGz(t, map[string][]byte{
		"server/NanoKVM-Server": body,
		"version":               []byte("2.3.15"),
	})

	dest := filepath.Join(t.TempDir(), "extracted")
	got, err := UnTarGz(src, dest)
	if err != nil {
		t.Fatalf("UnTarGz: %v", err)
	}
	if want, _ := filepath.Abs(dest); got != want {
		t.Errorf("dest = %q, want %q", got, want)
	}

	binary := filepath.Join(dest, "server", "NanoKVM-Server")
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("read extracted binary: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Errorf("extracted binary differs from archived content (%d vs %d bytes)", len(data), len(body))
	}
	// The mask applied to header.Mode must not have cost the executable bit.
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("stat extracted binary: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestUnTarGzRejectsOversizedArchive(t *testing.T) {
	src := writeTarGz(t, map[string][]byte{"big": bytes.Repeat([]byte{0}, 4096)})

	_, err := unTarGzCapped(src, filepath.Join(t.TempDir(), "extracted"), 1024)
	if !errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("err = %v, want ErrDecompressedTooLarge", err)
	}
}

// An archive that exactly fills the budget is legitimate and must extract.
func TestUnTarGzAcceptsArchiveAtExactlyTheLimit(t *testing.T) {
	src := writeTarGz(t, map[string][]byte{"exact": bytes.Repeat([]byte{0}, 1024)})

	if _, err := unTarGzCapped(src, filepath.Join(t.TempDir(), "extracted"), 1024); err != nil {
		t.Fatalf("unTarGz at exactly the limit: %v", err)
	}
}

// writeZip builds a .zip containing the named files and returns its path.
func writeZip(t *testing.T, files map[string][]byte) string {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	path := filepath.Join(t.TempDir(), "package.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	return path
}

func TestUnzipExtractsArchive(t *testing.T) {
	body := bytes.Repeat([]byte("nanokvm"), 4096)
	src := writeZip(t, map[string][]byte{"dir/payload.bin": body})

	dest := filepath.Join(t.TempDir(), "extracted")
	if err := Unzip(src, dest); err != nil {
		t.Fatalf("Unzip: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "dir", "payload.bin"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Errorf("extracted file differs from archived content (%d vs %d bytes)", len(data), len(body))
	}
}

func TestUnzipRejectsOversizedArchive(t *testing.T) {
	src := writeZip(t, map[string][]byte{"big": bytes.Repeat([]byte{0}, 4096)})

	err := unzipCapped(src, filepath.Join(t.TempDir(), "extracted"), 1024)
	if !errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("err = %v, want ErrDecompressedTooLarge", err)
	}
}

func TestUnzipAcceptsArchiveAtExactlyTheLimit(t *testing.T) {
	src := writeZip(t, map[string][]byte{"exact": bytes.Repeat([]byte{0}, 1024)})

	if err := unzipCapped(src, filepath.Join(t.TempDir(), "extracted"), 1024); err != nil {
		t.Fatalf("unzip at exactly the limit: %v", err)
	}
}
