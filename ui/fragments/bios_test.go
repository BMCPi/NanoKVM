package fragments

// fragments_bios_test.go covers the layer this file owns: turning an
// api/redfish BiosAttribute ("what this attribute is") into a
// components.BiosAttr ("which control to draw"). The rendering contract is
// covered in ui/components/bios_test.go and the staging semantics in
// api/redfish/bios_view_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/api/redfish"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

func TestBiosControlForMapsEveryType(t *testing.T) {
	opts := []redfish.BiosOption{{Value: "A", Label: "A"}}

	for _, tc := range []struct {
		name string
		attr redfish.BiosAttribute
		want components.BiosControl
	}{
		{"boolean", redfish.BiosAttribute{Type: redfish.BiosTypeBoolean}, components.BiosControlSwitch},
		{"integer", redfish.BiosAttribute{Type: redfish.BiosTypeInteger}, components.BiosControlNumber},
		{"password", redfish.BiosAttribute{Type: redfish.BiosTypePassword}, components.BiosControlPassword},
		{"string", redfish.BiosAttribute{Type: redfish.BiosTypeString}, components.BiosControlText},
		{"enumeration with values", redfish.BiosAttribute{Type: redfish.BiosTypeEnumeration, Options: opts}, components.BiosControlSelect},
		// An Enumeration with no declared values cannot be a select: the
		// dropdown would be empty and could only ever clear the attribute.
		{"enumeration with no values", redfish.BiosAttribute{Type: redfish.BiosTypeEnumeration}, components.BiosControlText},
		// Read-only wins over the declared type, whatever it is.
		{"read-only integer", redfish.BiosAttribute{Type: redfish.BiosTypeInteger, ReadOnly: true}, components.BiosControlReadOnly},
		{"read-only enumeration", redfish.BiosAttribute{Type: redfish.BiosTypeEnumeration, Options: opts, ReadOnly: true}, components.BiosControlReadOnly},
		// An unrecognised type still gets an editor rather than vanishing.
		{"unknown type", redfish.BiosAttribute{Type: "SomethingNew"}, components.BiosControlText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := biosControlFor(tc.attr); got != tc.want {
				t.Errorf("biosControlFor(%s) = %q, want %q", tc.attr.Type, got, tc.want)
			}
		})
	}
}

func TestBiosAttrCarriesBoundsAndOptions(t *testing.T) {
	lo, hi := int64(0), int64(16)
	minLen, maxLen := int64(2), int64(8)

	got := biosAttr(redfish.BiosAttribute{
		Name: "ProcCoreCount", DisplayName: "Active Cores", HelpText: "How many.",
		WarningText: "Reboot required.", Type: redfish.BiosTypeInteger,
		Current: float64(16), Pending: int64(8), HasPending: true,
		LowerBound: &lo, UpperBound: &hi, MinLength: &minLen, MaxLength: &maxLen,
		Registered: true,
	}, "")

	if got.Label != "Active Cores" {
		t.Errorf("Label = %q", got.Label)
	}
	if got.Help != "How many." || got.Warning != "Reboot required." {
		t.Errorf("help/warning not carried: %+v", got)
	}
	// The editor is seeded with the staged value, and the live value stays
	// available for the "was X" hint.
	if got.Value != "8" {
		t.Errorf("Value = %q, want the staged 8", got.Value)
	}
	if got.Current != "16" {
		t.Errorf("Current = %q, want the live 16", got.Current)
	}
	if !got.Staged {
		t.Error("Staged = false, want true")
	}
	if got.Min != "0" || got.Max != "16" {
		t.Errorf("bounds = %q..%q, want 0..16", got.Min, got.Max)
	}
	if got.MinLength != "2" || got.MaxLength != "8" {
		t.Errorf("lengths = %q..%q, want 2..8", got.MinLength, got.MaxLength)
	}
	if got.Unregistered {
		t.Error("Unregistered = true for a registered attribute")
	}
}

// A bound of zero is a real constraint. Rendering it as "" would drop the
// input's min attribute and let negatives through in the browser.
func TestBiosAttrKeepsZeroBounds(t *testing.T) {
	zero := int64(0)
	got := biosAttr(redfish.BiosAttribute{Type: redfish.BiosTypeInteger, LowerBound: &zero}, "")
	if got.Min != "0" {
		t.Errorf("Min = %q, want %q", got.Min, "0")
	}
	if got.Max != "" {
		t.Errorf("Max = %q, want empty for an undeclared bound", got.Max)
	}
}

// A select whose live value is not among the registry's allowed values must
// still render that value — otherwise the control silently displays something
// the host is not actually running.
func TestBiosAttrSurfacesOutOfRegistryCurrentValue(t *testing.T) {
	got := biosAttr(redfish.BiosAttribute{
		Name: "Mode", Type: redfish.BiosTypeEnumeration, Registered: true,
		Current: "LegacyOnly",
		Options: []redfish.BiosOption{{Value: "UEFI", Label: "UEFI"}, {Value: "Both", Label: "Both"}},
	}, "")

	if got.Control != components.BiosControlSelect {
		t.Fatalf("Control = %q, want select", got.Control)
	}
	if len(got.Options) != 3 {
		t.Fatalf("options = %d, want the two declared plus the current one", len(got.Options))
	}
	last := got.Options[len(got.Options)-1]
	if last.Value != "LegacyOnly" {
		t.Errorf("current out-of-registry value not offered: %+v", got.Options)
	}
	if !strings.Contains(last.Label, "not in registry") {
		t.Errorf("the injected option should say what it is, got %q", last.Label)
	}
}

func TestBiosAttrMarksUnregisteredAndCarriesError(t *testing.T) {
	got := biosAttr(redfish.BiosAttribute{
		Name: "MysteryKnob", Type: redfish.BiosTypeString, Current: "x", Registered: false,
	}, "must be a whole number")

	if !got.Unregistered {
		t.Error("Unregistered = false for an attribute with no registry entry")
	}
	// With no DisplayName the raw key is the label.
	if got.Label != "MysteryKnob" {
		t.Errorf("Label = %q, want the raw attribute name", got.Label)
	}
	if !got.HasError() || got.Error != "must be a whole number" {
		t.Errorf("validation message not carried: %+v", got)
	}
}

func TestBiosCountLabelPluralises(t *testing.T) {
	if got := biosCountLabel(1, "change", "changes"); got != "1 change" {
		t.Errorf("got %q", got)
	}
	if got := biosCountLabel(3, "change", "changes"); got != "3 changes" {
		t.Errorf("got %q", got)
	}
}

// biosRouter mounts the BIOS fragment routes without the auth middleware.
func biosRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	d := &deps.Deps{}
	r := gin.New()
	r.Use(deps.Middleware(d))
	biosFragmentRoutes(r.Group("/ui"), d)
	return r
}

// Smoke test over the wire: the routes exist, answer 200, and every response
// carries the out-of-band regions the page's layering depends on. Host state
// is empty in a test binary, so this exercises the empty path — which is also
// the one a freshly-flashed BMC actually shows.
func TestBiosFragmentRoutesRespondWithOutOfBandRegions(t *testing.T) {
	r := biosRouter(t)

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"menu", http.MethodGet, "/ui/bios/menu?path=./Main"},
		{"search", http.MethodGet, "/ui/bios/search?q=core"},
		{"search cleared", http.MethodGet, "/ui/bios/search?q="},
		{"discard all", http.MethodPost, "/ui/bios/discard"},
		{"discard one", http.MethodPost, "/ui/bios/discard?attr=Nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			body := w.Body.String()
			for _, id := range []string{`id="bios-rail"`, `id="bios-staged"`} {
				if !strings.Contains(body, id) {
					t.Errorf("response missing out-of-band %s", id)
				}
			}
			if n := strings.Count(body, `hx-swap-oob="true"`); n != 2 {
				t.Errorf("out-of-band regions = %d, want 2", n)
			}
		})
	}
}

// A rejected value must come back as a 200 carrying the inline error, not an
// error status: htmx skips the swap on a 4xx/5xx, so the message would never
// reach the field that caused it.
func TestBiosStageAnswers200SoErrorsCanBeSwappedIn(t *testing.T) {
	r := biosRouter(t)

	form := strings.NewReader("__attr=NoSuchAttribute&NoSuchAttribute=x&menu=./Main")
	req := httptest.NewRequest(http.MethodPost, "/ui/bios/stage", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so htmx performs the swap", w.Code)
	}
	if trigger := w.Header().Get("HX-Trigger"); !strings.Contains(trigger, "ui-toast") {
		t.Errorf("want a toast reporting the rejection, got %q", trigger)
	}
}

// An attribute described from the BMC's compiled-in platform table is not
// "unregistered" — it has a type, a value list and a menu, just not from the
// host. Marking it both ways would put two contradictory badges on one row.
func TestBiosAttrDistinguishesCatalogedFromUnregistered(t *testing.T) {
	cataloged := biosAttr(redfish.BiosAttribute{
		Name: "EthIp4Mode", DisplayName: "IPv4 Policy",
		Type: redfish.BiosTypeEnumeration, MenuPath: "./IPv4 (BMC Managed)",
		Current: "Dhcp", Registered: false, Cataloged: true,
		Options: []redfish.BiosOption{{Value: "Dhcp", Label: "DHCP"}},
	}, "")

	if !cataloged.Cataloged {
		t.Error("Cataloged = false; the row cannot say where its description came from")
	}
	if cataloged.Unregistered {
		t.Error("Unregistered = true as well; the row would carry both badges")
	}
	if cataloged.Control != components.BiosControlSelect {
		t.Errorf("control = %q, want a select", cataloged.Control)
	}

	// A registry that named the attribute but omitted its values leaves both
	// true, and the operator is still being shown constraints the running
	// firmware never asserted.
	toppedUp := biosAttr(redfish.BiosAttribute{
		Name: "EthIp4Mode", Type: redfish.BiosTypeEnumeration,
		Current: "Dhcp", Registered: true, Cataloged: true,
		Options: []redfish.BiosOption{{Value: "Dhcp", Label: "DHCP"}},
	}, "")
	if !toppedUp.Cataloged || toppedUp.Unregistered {
		t.Errorf("registered+cataloged mapped wrong: cataloged=%v unregistered=%v",
			toppedUp.Cataloged, toppedUp.Unregistered)
	}
}
