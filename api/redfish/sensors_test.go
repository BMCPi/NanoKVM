package redfish

import (
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/bmcsensor"
)

// writeEEPROM lays a record down at its real offset inside a file the size of
// the emulated part, then points the package's reader at it. Restoring the
// reader afterwards matters: it is a process-wide singleton because staleness
// is measured across reads.
func writeEEPROM(t *testing.T, seq uint32, tempMilliC int32, status uint32) {
	t.Helper()
	rec := make([]byte, bmcsensor.RecordSize)
	binary.LittleEndian.PutUint32(rec[0:4], bmcsensor.RecordMagic)
	binary.LittleEndian.PutUint16(rec[4:6], bmcsensor.RecordVersion)
	binary.LittleEndian.PutUint16(rec[6:8], bmcsensor.RecordSize)
	binary.LittleEndian.PutUint32(rec[8:12], seq)
	binary.LittleEndian.PutUint32(rec[12:16], uint32(tempMilliC))
	binary.LittleEndian.PutUint32(rec[16:20], 1234)
	binary.LittleEndian.PutUint32(rec[20:24], status)
	binary.LittleEndian.PutUint32(rec[28:32], crc32.ChecksumIEEE(rec[:28]))

	buf := make([]byte, 64*1024)
	copy(buf[bmcsensor.RecordOffset:], rec)
	path := filepath.Join(t.TempDir(), "slave-eeprom")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := socReader
	socReader = bmcsensor.NewReaderAt(path, bmcsensor.DefaultStaleAfter)
	t.Cleanup(func() { socReader = prev })
	testEEPROMPath = path
}

// testEEPROMPath is where writeEEPROM last laid a fixture down, so a test
// that wants a differently-configured reader over the same bytes can build
// one without repeating the fixture.
var testEEPROMPath string

func socReaderPathForTest(t *testing.T) string {
	t.Helper()
	if testEEPROMPath == "" {
		t.Fatal("writeEEPROM has not run")
	}
	return testEEPROMPath
}

// noEEPROM points the reader at nothing, which is a BMC whose kernel has no
// slave EEPROM configured.
func noEEPROM(t *testing.T) {
	t.Helper()
	prev := socReader
	socReader = bmcsensor.NewReaderAt(filepath.Join(t.TempDir(), "absent"), time.Minute)
	t.Cleanup(func() { socReader = prev })
}

func sensorRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	svc := NewService(testDeps())
	r := gin.New()
	r.GET(chassisItemPath, svc.GetChassis)
	r.GET(sensorsPath, svc.GetSensorCollection)
	r.GET(sensorsPath+"/:sensor", svc.GetSensor)
	return r
}

func getJSON(t *testing.T, r *gin.Engine, path string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: decode: %v (%s)", path, err, w.Body.String())
		}
	}
	return w.Code, body
}

func TestSensorReportsTheEEPROMReading(t *testing.T) {
	writeEEPROM(t, 7, 47250, bmcsensor.StatusTempValid|bmcsensor.StatusI2CReady|bmcsensor.StatusLastPushOK)
	code, body := getJSON(t, sensorRouter(), sensorsPath+"/"+socSensorID)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := body["Reading"]; got != 47.25 {
		t.Errorf("Reading = %v, want 47.25", got)
	}
	if got := body["ReadingUnits"]; got != "Cel" {
		t.Errorf("ReadingUnits = %v, want Cel", got)
	}
	status, _ := body["Status"].(map[string]any)
	if status["State"] != "Enabled" || status["Health"] != "OK" {
		t.Errorf("Status = %v, want Enabled/OK", status)
	}
	oem, _ := body["Oem"].(map[string]any)
	nano, _ := oem["NanoKVM"].(map[string]any)
	if nano["Sequence"] != float64(7) || nano["Stale"] != false || nano["TemperatureValid"] != true {
		t.Errorf("Oem.NanoKVM = %v", nano)
	}
}

// A host that has gone quiet leaves its last record behind, and it keeps
// parsing. Publishing that as a live reading would show a die temperature for
// a machine that may be powered off, so the value has to be withheld.
//
// Staleness is a real elapsed-time property, so it is exercised with a
// one-millisecond window and a real pause rather than by reaching into the
// reader's clock. Twenty milliseconds against a one-millisecond window leaves
// no room for a loaded machine to make this flaky.
func TestSensorWithholdsAStaleReading(t *testing.T) {
	writeEEPROM(t, 7, 47250, bmcsensor.StatusTempValid)
	socReader = bmcsensor.NewReaderAt(socReaderPathForTest(t), time.Millisecond)
	if _, err := socReader.Read(); err != nil {
		t.Fatalf("priming read: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	_, body := getJSON(t, sensorRouter(), sensorsPath+"/"+socSensorID)
	if _, present := body["Reading"]; present {
		t.Errorf("a stale sample should not publish a Reading: %v", body["Reading"])
	}
	status, _ := body["Status"].(map[string]any)
	if status["State"] != "UnavailableOffline" {
		t.Errorf("Status.State = %v, want UnavailableOffline", status["State"])
	}
	oem, _ := body["Oem"].(map[string]any)
	nano, _ := oem["NanoKVM"].(map[string]any)
	if nano["Stale"] != true {
		t.Errorf("Oem should say the sample is stale: %v", nano)
	}
}

// The pTA clears TEMP_VALID when the AVS read failed and carries the previous
// value forward. That value is not a reading.
func TestSensorWithholdsAnInvalidTemperature(t *testing.T) {
	writeEEPROM(t, 7, 47250, bmcsensor.StatusI2CReady) // TEMP_VALID clear
	_, body := getJSON(t, sensorRouter(), sensorsPath+"/"+socSensorID)
	if _, present := body["Reading"]; present {
		t.Error("a sample without TEMP_VALID should not publish a Reading")
	}
}

// With no slave EEPROM the sensor still has to be a well-formed resource: a
// client walking Chassis should learn there is no reading, not hit a 404 that
// ends the walk.
func TestSensorWithoutAnEEPROM(t *testing.T) {
	noEEPROM(t)
	r := sensorRouter()

	code, body := getJSON(t, r, sensorsPath+"/"+socSensorID)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, present := body["Reading"]; present {
		t.Error("no EEPROM should mean no Reading")
	}

	code, coll := getJSON(t, r, sensorsPath)
	if code != http.StatusOK {
		t.Fatalf("collection status = %d, want 200", code)
	}
	if n := coll["Members@odata.count"]; n != float64(0) {
		t.Errorf("collection count = %v, want 0 when there is no EEPROM", n)
	}
}

func TestSensorCollectionListsTheSensor(t *testing.T) {
	writeEEPROM(t, 1, 40000, bmcsensor.StatusTempValid)
	code, coll := getJSON(t, sensorRouter(), sensorsPath)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if n := coll["Members@odata.count"]; n != float64(1) {
		t.Fatalf("count = %v, want 1", n)
	}
	members, _ := coll["Members"].([]any)
	first, _ := members[0].(map[string]any)
	if got := first["@odata.id"]; got != sensorsPath+"/"+socSensorID {
		t.Errorf("member = %v", got)
	}
}

func TestUnknownSensorIs404(t *testing.T) {
	writeEEPROM(t, 1, 40000, bmcsensor.StatusTempValid)
	code, _ := getJSON(t, sensorRouter(), sensorsPath+"/Nonexistent")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

// Chassis has to advertise the collection or nothing walking the tree will
// find it.
func TestChassisLinksSensors(t *testing.T) {
	writeEEPROM(t, 1, 40000, bmcsensor.StatusTempValid)
	_, body := getJSON(t, sensorRouter(), chassisItemPath)
	sensors, _ := body["Sensors"].(map[string]any)
	if got := sensors["@odata.id"]; got != sensorsPath {
		t.Errorf("Chassis.Sensors = %v, want %s", got, sensorsPath)
	}
	// Thermal stays where it was: the two are different owners.
	thermal, _ := body["Thermal"].(map[string]any)
	if got := thermal["@odata.id"]; got != chassisThermalPath {
		t.Errorf("Chassis.Thermal = %v", got)
	}
}
