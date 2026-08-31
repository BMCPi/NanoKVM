package fragments

// overview_test.go covers the Resources card's model.
//
// resourceDetail is the figure that gives a percentage meaning, and it has one
// rule worth pinning: both halves must be in the same unit. "900 MB / 6.8 GB"
// is arithmetic the card exists to save the reader from, and the unit is
// chosen from the total precisely so the pair can never be mixed.

import (
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/sysinfo"
)

func TestResourceDetailKeepsBothHalvesInOneUnit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		used, total uint64
		want        string
	}{
		{"this device's memory", 161, 246, "161 / 246 MB"},
		{"just under the GB switch", 900, 1023, "900 / 1023 MB"},
		// The moment the total reaches a GB, so does the used half — the bug
		// this guards is "900 MB / 1.0 GB".
		{"at the GB switch", 900, 1024, "0.9 / 1.0 GB"},
		{"a data volume", 1229, 6963, "1.2 / 6.8 GB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resourceDetail(tc.used, tc.total); got != tc.want {
				t.Errorf("resourceDetail(%d, %d) = %q, want %q", tc.used, tc.total, got, tc.want)
			}
		})
	}
}

// A subsystem with no total has no absolute figure to show, and "0 / 0 MB"
// beside a percentage is worse than nothing.
func TestResourceDetailIsEmptyWithoutATotal(t *testing.T) {
	if got := resourceDetail(0, 0); got != "" {
		t.Errorf("resourceDetail(0, 0) = %q, want empty", got)
	}
}

// With no history the card must report that it is still collecting rather than
// rendering three empty plots, which read as measured zeros.
func TestResourcesModelIsNotSamplingBeforeAnyHistory(t *testing.T) {
	if len(sysinfo.ResourceHistory()) > 0 {
		t.Skip("the sampler is running in this process; the empty case cannot be observed")
	}
	if m := overviewResourcesModel(); m.Sampling {
		t.Error("the model claims to be sampling with no history behind it")
	}
}
