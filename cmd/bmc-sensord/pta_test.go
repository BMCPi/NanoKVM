package main

import (
	"testing"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/optee"
)

// The pTA UUID is written here as a string but reaches the TEE as 16 octets.
// The C original spells it as a TEEC_UUID literal:
//
//	{ 0x575d6607, 0x5a2b, 0x4384,
//	  { 0x82, 0x7e, 0xc1, 0x6a, 0x25, 0xac, 0x4f, 0xa5 } }
//
// which libteec serialises big-endian field by field. Getting the byte order
// wrong does not fail loudly — it opens a session to nothing and returns
// ITEM_NOT_FOUND — so the expected octets are pinned against that literal.
func TestUUIDOctetsMatchTheCLiteral(t *testing.T) {
	want := [16]byte{
		0x57, 0x5d, 0x66, 0x07, // timeLow, big-endian
		0x5a, 0x2b, // timeMid
		0x43, 0x84, // timeHiAndVersion
		0x82, 0x7e, 0xc1, 0x6a, 0x25, 0xac, 0x4f, 0xa5, // clockSeqAndNode
	}
	got := optee.MustParseUUID(UUIDString)
	if [16]byte(got) != want {
		t.Errorf("UUID octets = % x, want % x", got[:], want[:])
	}
	if s := got.String(); s != UUIDString {
		t.Errorf("round trip = %q, want %q", s, UUIDString)
	}
}

// GET and WAIT do not put the sample in the same slots. Mixing them up
// produces a sample that looks plausible and is wrong in every field.
func TestSampleSlotsDifferBetweenGetAndWait(t *testing.T) {
	// Distinct values per slot so a mis-read shows up as the wrong number
	// rather than a coincidence.
	p := optee.Params{
		{A: 11, B: 12},
		{A: 21, B: 22},
		{A: 31, B: 32},
		{},
	}

	get := sampleFromGet(p)
	if want := (Sample{TempMilliC: 11, Seq: 12, Status: 21, I2CErrors: 22}); get != want {
		t.Errorf("sampleFromGet = %+v, want %+v", get, want)
	}

	wait := sampleFromWait(p)
	if want := (Sample{TempMilliC: 21, Seq: 22, Status: 31, I2CErrors: 32}); wait != want {
		t.Errorf("sampleFromWait = %+v, want %+v", wait, want)
	}
}

func TestInitParamsCarryTheWireContract(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Period = 250 * time.Millisecond
	p := initParams(cfg)

	// The 64-bit BAR is split low half, high half.
	if p[0].A != 0x00000000 || p[0].B != 0x0000001f {
		t.Errorf("BAR split = a:0x%08x b:0x%08x, want a:0x00000000 b:0x0000001f", p[0].A, p[0].B)
	}
	if p[1].A != RP1I2C1Offset || p[1].B != EEPROMSlave {
		t.Errorf("i2c = a:0x%x b:0x%x, want a:0x%x b:0x%x",
			p[1].A, p[1].B, RP1I2C1Offset, EEPROMSlave)
	}
	if p[2].A != SensorEEPROMOffset || p[2].B != 250 {
		t.Errorf("eeprom/period = a:0x%x b:%d, want a:0x%x b:250", p[2].A, p[2].B, SensorEEPROMOffset)
	}
	for i, want := range []optee.ParamType{
		optee.ParamValueInput, optee.ParamValueInput, optee.ParamValueInput, optee.ParamNone,
	} {
		if p[i].Type != want {
			t.Errorf("param %d type = %d, want %d", i, p[i].Type, want)
		}
	}
}

// A BAR above 4 GiB has to survive the split; the default one does not
// exercise the high half meaningfully on its own.
func TestInitParamsSplitsAHighBAR(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BAR = 0x1234_5678_9abc_def0
	p := initParams(cfg)
	if p[0].A != 0x9abcdef0 || p[0].B != 0x12345678 {
		t.Errorf("BAR split = a:0x%08x b:0x%08x, want a:0x9abcdef0 b:0x12345678", p[0].A, p[0].B)
	}
}

func TestWaitParamsCarrySeqAndTimeout(t *testing.T) {
	p := waitParams(42, 1500*time.Millisecond)
	if p[0].Type != optee.ParamValueInput || p[0].A != 42 || p[0].B != 1500 {
		t.Errorf("wait request = %+v, want input seq 42 timeout 1500", p[0])
	}
	// Zero means wait forever, which is what the streaming mode sends.
	if p := waitParams(0, 0); p[0].B != 0 {
		t.Errorf("indefinite wait timeout = %d, want 0", p[0].B)
	}
}

// The temperature format is the one externally visible thing here, and the
// negative cases are where the C original is wrong: it takes abs() of the
// remainder after a truncating divide, so a reading between -1 and 0 °C loses
// its sign entirely.
func TestSampleTemperature(t *testing.T) {
	for _, tc := range []struct {
		milli int32
		want  string
	}{
		{0, "0.000"},
		{45678, "45.678"},
		{45000, "45.000"},
		{1, "0.001"},
		{-12345, "-12.345"},
		{-500, "-0.500"}, // the C original prints "0.500" here
		{-1, "-0.001"},   // and "0.001" here
		{-45000, "-45.000"},
	} {
		got := Sample{TempMilliC: tc.milli}.Temperature()
		if got != tc.want {
			t.Errorf("Temperature(%d) = %q, want %q", tc.milli, got, tc.want)
		}
	}
}

// Negating the most-negative int32 overflows if the sign split is done in
// 32-bit arithmetic.
func TestSampleTemperatureHandlesMinInt32(t *testing.T) {
	got := Sample{TempMilliC: -2147483648}.Temperature()
	if want := "-2147483.648"; got != want {
		t.Errorf("Temperature(MinInt32) = %q, want %q", got, want)
	}
}

// The printed line is the C daemon's, so anything scraping stdout keeps
// working across the port.
func TestSampleStringMatchesTheCFormat(t *testing.T) {
	s := Sample{TempMilliC: 45678, Seq: 7, Status: 0x3, I2CErrors: 2}
	if want := "seq=7 soc=45.678 C status=0x3 i2c_errs=2"; s.String() != want {
		t.Errorf("String() = %q, want %q", s.String(), want)
	}
}

func TestMillisClamps(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want uint32
	}{
		{0, 0},
		{-time.Second, 0},
		{500 * time.Microsecond, 0}, // sub-millisecond truncates to "none"
		{time.Millisecond, 1},
		{time.Second, 1000},
		{time.Duration(1<<62 - 1), ^uint32(0)}, // saturates rather than wrapping
	} {
		if got := millis(tc.in); got != tc.want {
			t.Errorf("millis(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
