package cvi

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestNormaliseModule(t *testing.T) {
	cases := map[string]string{
		"cv181x_sys":     "cv181x_sys",
		"cv181x_sys.ko":  "cv181x_sys",
		"cvi-mipi-rx":    "cvi_mipi_rx",
		"cvi-mipi-rx.ko": "cvi_mipi_rx",
	}
	for in, want := range cases {
		if got := normaliseModule(in); got != want {
			t.Errorf("normaliseModule(%q) = %q, want %q", in, got, want)
		}
	}
}

// The load order is a dependency order: everything links against symbols from
// sys and base, so those have to go in first and come out last.
func TestMediaModuleOrder(t *testing.T) {
	sys := slices.Index(mediaModules, "cv181x_sys")
	base := slices.Index(mediaModules, "cv181x_base")
	if sys < 0 || base < 0 {
		t.Fatal("cv181x_sys and cv181x_base must both be in the load list")
	}
	if sys > base {
		t.Errorf("cv181x_sys must load before cv181x_base, got %d and %d", sys, base)
	}
	for _, name := range []string{"cv181x_vi", "cv181x_vpss", "cvi_vc_driver"} {
		if i := slices.Index(mediaModules, name); i < base {
			t.Errorf("%s at %d loads before cv181x_base at %d", name, i, base)
		}
	}
}

func TestFindModuleSearchesLayouts(t *testing.T) {
	root := t.TempDir()
	const release = "6.18.38"

	// Only the "extra" layout exists here; findModule has to try more than
	// one subdirectory to find it.
	dir := filepath.Join(root, release, "extra")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "cv181x_sys.ko")
	if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := moduleDirs
	moduleDirs = []string{root}
	t.Cleanup(func() { moduleDirs = old })

	got, err := findModule(release, "cv181x_sys")
	if err != nil {
		t.Fatalf("findModule: %v", err)
	}
	if got != want {
		t.Errorf("findModule = %q, want %q", got, want)
	}

	if _, err := findModule(release, "not_a_module"); err == nil {
		t.Error("findModule should fail for a module that is not present")
	}
}
