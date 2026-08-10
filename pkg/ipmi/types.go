package ipmi

// RMCP header constants
const (
	rmcpVersion  byte = 0x06
	rmcpReserved byte = 0x00
	rmcpSeqNone  byte = 0xFF

	rmcpClassASF  byte = 0x06
	rmcpClassIPMI byte = 0x07
)

// ASF constants
const (
	asfIANA        uint32 = 0x000011BE
	asfMessagePing byte   = 0x80
	asfMessagePong byte   = 0x40
)

// IPMI authentication types
const (
	authTypeNone  byte = 0x00
	authTypeRMCPP byte = 0x06
)

// RMCP+ payload types
const (
	payloadTypeIPMI            byte = 0x00
	payloadTypeSOL             byte = 0x01
	payloadTypeOpenSessionReq  byte = 0x10
	payloadTypeOpenSessionResp byte = 0x11
	payloadTypeRAKPMsg1        byte = 0x12
	payloadTypeRAKPMsg2        byte = 0x13
	payloadTypeRAKPMsg3        byte = 0x14
	payloadTypeRAKPMsg4        byte = 0x15
)

// IPMI NetFn values
const (
	netFnChassisReq  byte = 0x00
	netFnChassisResp byte = 0x01
	netFnAppReq      byte = 0x06
	netFnAppResp     byte = 0x07
)

// Chassis commands
const (
	cmdGetChassisStatus     byte = 0x01
	cmdChassisControl       byte = 0x02
	cmdSetSystemBootOptions byte = 0x08
	cmdGetSystemBootOptions byte = 0x09
)

// App commands
const (
	cmdGetDeviceID       byte = 0x01
	cmdGetChannelAuthCap byte = 0x38
	cmdSetSessionPriv    byte = 0x3B
	cmdCloseSession      byte = 0x3C
	cmdActivatePayload   byte = 0x48
	cmdDeactivatePayload byte = 0x49
)

// RMCP+ authentication algorithms
const (
	authAlgoNone     byte = 0x00
	authAlgoHMACSHA1 byte = 0x01
)

// RMCP+ integrity algorithms
const (
	integAlgoNone       byte = 0x00
	integAlgoHMACSHA196 byte = 0x01
)

// RMCP+ confidentiality algorithms
const (
	confidAlgoNone byte = 0x00
)

// IPMI completion codes
const (
	ccOK               byte = 0x00
	ccInvalidCommand   byte = 0xC1
	ccInvalidParam     byte = 0xC9
	ccUnspecified      byte = 0xFF
	ccPayloadAlready   byte = 0x80
	ccPayloadNotActive byte = 0x80
)

// Chassis control actions
const (
	controlPowerDown    byte = 0x00
	controlPowerUp      byte = 0x01
	controlPowerCycle   byte = 0x02
	controlHardReset    byte = 0x03
	controlSoftShutdown byte = 0x05
)

// Boot device values (bits 5:2 of boot flags byte 2)
const (
	bootDeviceNone  byte = 0x00
	bootDevicePXE   byte = 0x04
	bootDeviceDisk  byte = 0x08
	bootDeviceCDROM byte = 0x14
	bootDeviceBIOS  byte = 0x18
)

// Boot option parameter selectors
const (
	bootParamSetInProgress byte = 0x00
	bootParamBootFlags     byte = 0x05
)

// IPMI addressing
const (
	bmcSlaveAddr byte = 0x20
	swID         byte = 0x81
)

// Default IPMI credentials (hardcoded for now)
const defaultPassword = "admin"

// bmcGUID is a fixed identifier for this BMC instance.
var bmcGUID = [16]byte{
	0x4e, 0x61, 0x6e, 0x6f, 0x4b, 0x56, 0x4d, 0x2d,
	0x42, 0x4d, 0x43, 0x2d, 0x47, 0x55, 0x49, 0x44,
}

// ipmiChecksum computes the IPMI checksum (two's complement of the sum mod 256).
func ipmiChecksum(data []byte) byte {
	var sum byte
	for _, b := range data {
		sum += b
	}
	return -sum
}
