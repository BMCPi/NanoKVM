package redfish

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
)

func Register(r *gin.Engine, d *deps.Deps) {
	service := NewService(d)

	// Public endpoints
	r.GET("/redfish", service.GetRedfishBase)

	// The service root is served at both spellings. The canonical form is
	// schemas.DefaultServiceRoot ("/redfish/v1/", trailing slash) — that is
	// what gofish requests on Login and what we now emit as @odata.id. The
	// bare "/redfish/v1" stays registered so existing callers keep working
	// rather than relying on gin's 301 redirect.
	r.GET(ServiceRootPath, service.GetServiceRoot)
	r.GET(strings.TrimSuffix(ServiceRootPath, "/"), service.GetServiceRoot)

	r.GET("/redfish/v1/SessionService", service.GetSessionService)
	r.POST("/redfish/v1/SessionService/Sessions", service.CreateSession)

	// OpenAPI documentation — public so clients (bmclib, gofish, etc.)
	// can introspect the surface before authenticating. The rendered
	// human-readable docs page lives at /docs (behind auth, sharing the
	// dashboard chrome); see the ui package.
	r.GET("/redfish/v1/openapi.yaml", service.GetOpenAPIYAML)
	r.GET("/redfish/v1/openapi.json", service.GetOpenAPIJSON)

	// Protected endpoints
	api := r.Group("/redfish/v1").Use(CheckAuth())
	{
		// Systems
		api.GET("/Systems", service.GetSystemCollection)
		api.GET("/Systems/1", service.GetSystem)
		api.POST("/Systems/1/Actions/ComputerSystem.Reset", service.ResetSystem)
		api.PATCH("/Systems/1", service.PatchSystem)

		// Bios — RPi 5 bootloader EEPROM as Redfish Bios.Attributes.
		// Live values come from the bootconf.txt embedded in pieeprom.bin
		// (U-Boot writes a fresh EEPROM dump each boot);
		// PATCH /Bios/Settings stages a pieeprom.upd for the host's
		// rpi-eeprom-update to flash on next boot.
		api.GET("/Systems/1/Bios", service.GetBios)
		api.GET("/Systems/1/Bios/Settings", service.GetBiosSettings)
		api.PATCH("/Systems/1/Bios/Settings", service.PatchBiosSettings)
		api.GET("/Systems/1/Bios/AttributeRegistry", service.GetBiosAttributeRegistry)

		// Per-device inventory collections — the standard homes for what
		// used to sit under the ComputerSystem's Oem.NanoKVM block.
		api.GET("/Systems/1/Memory", service.GetMemoryCollection)
		api.GET("/Systems/1/Memory/:module", service.GetMemoryModule)
		api.GET("/Systems/1/Processors", service.GetProcessorCollection)
		api.GET("/Systems/1/Processors/:processor", service.GetProcessor)
		api.GET("/Systems/1/EthernetInterfaces", service.GetEthernetInterfaceCollection)
		api.GET("/Systems/1/EthernetInterfaces/:nic", service.GetEthernetInterface)

		// Storage — the USB gadget's LUNs as a Storage subsystem with one
		// Drive per backed LUN (boot image + inserted virtual media).
		api.GET("/Systems/1/Storage", service.GetStorageCollection)
		api.GET("/Systems/1/Storage/:storage", service.GetStorage)
		api.GET("/Systems/1/Storage/:storage/Drives/:drive", service.GetDrive)

		// Chassis — the host baseboard (SMBIOS type 2); the service root
		// has always advertised this collection.
		api.GET("/Chassis", service.GetChassisCollection)
		api.GET("/Chassis/1", service.GetChassis)

		// TrustedComponents — the rpi-eeprom bootloader as the platform root
		// of trust, with its firmware version/flash-time as a nested
		// SoftwareInventory.
		api.GET("/Systems/1/TrustedComponents", service.GetTrustedComponentCollection)
		api.GET("/Systems/1/TrustedComponents/Bootloader", service.GetTrustedComponentBootloader)
		api.GET("/Systems/1/TrustedComponents/Bootloader/SoftwareImages/Active", service.GetBootloaderSoftwareInventory)

		// Managers
		api.GET("/Managers", service.GetManagerCollection)
		api.GET("/Managers/1", service.GetManager)
		api.GET("/Managers/1/Oem/Dell/DellAttributes/iDRAC.Embedded.1", service.GetDellIDRACAttributes)

		// Serial Interfaces
		api.GET("/Managers/1/SerialInterfaces", service.GetSerialInterfaceCollection)
		api.GET("/Managers/1/SerialInterfaces/1", service.GetSerialInterface)
		api.PATCH("/Managers/1/SerialInterfaces/1", service.PatchSerialInterface)

		// Virtual Media
		api.GET("/Managers/1/VirtualMedia", service.GetVirtualMediaCollection)
		api.GET("/Managers/1/VirtualMedia/CD", service.GetVirtualMedia)
		api.POST("/Managers/1/VirtualMedia/CD/Actions/VirtualMedia.InsertMedia", service.InsertMedia)
		api.POST("/Managers/1/VirtualMedia/CD/Actions/VirtualMedia.EjectMedia", service.EjectMedia)

		// Sessions
		api.GET("/SessionService/Sessions", service.GetSessionCollection)
		api.DELETE("/SessionService/Sessions/:id", service.DeleteSession)

		// UpdateService (firmware updates)
		api.GET("/UpdateService", service.GetUpdateService)
		api.GET("/UpdateService/FirmwareInventory", service.GetFirmwareInventoryCollection)
		api.GET("/UpdateService/FirmwareInventory/BIOS", service.GetFirmwareInventoryUBoot)
		api.POST("/UpdateService/Actions/UpdateService.SimpleUpdate", service.SimpleUpdate)
		api.POST("/UpdateService/Actions/UpdateService.StartUpdate", service.StartUpdate)
	}
}
