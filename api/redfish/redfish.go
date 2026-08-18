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

	// Protected endpoints. CheckAuth passes host-interface requests through
	// unauthenticated (DSP0270); handlers guard host-owned writes with
	// hostWritable instead.
	api := r.Group("/redfish/v1").Use(CheckAuth())
	{
		// Systems. PATCH carries both write directions: operators stage the
		// boot override, the host firmware reports identity/boot progress.
		api.GET("/Systems", service.GetSystemCollection)
		api.GET("/Systems/1", service.GetSystem)
		api.POST("/Systems/1/Actions/ComputerSystem.Reset", service.ResetSystem)
		api.PATCH("/Systems/1", service.PatchSystem)

		// Bios — host-reported attributes; operators stage changes on the
		// SettingsObject and the host firmware applies them at boot. The
		// registry is a host-published document.
		api.GET("/Systems/1/Bios", service.GetBios)
		api.PATCH("/Systems/1/Bios", service.PatchBios)
		api.GET("/Systems/1/Bios/Settings", service.GetBiosSettings)
		api.PATCH("/Systems/1/Bios/Settings", service.PatchBiosSettings)
		// The attribute registry hangs off Bios rather than /Registries:
		// EDK2's client builds the URI as <parent of Bios>/<AttributeRegistry
		// property>, so the wildcard catches whatever name it derived
		// (BiosAttributeRegistry, a versioned variant, or the legacy
		// spelling). gin matches the static Settings siblings above first.
		api.GET("/Systems/1/Bios/:registry", service.GetBiosAttributeRegistry)
		api.PUT("/Systems/1/Bios/:registry", service.PutBiosAttributeRegistry)

		// Registry-file collection: BiosAttributeRegistryDxe starts here and
		// gives up if the branch is missing from the service root.
		api.GET("/Registries", service.GetRegistries)
		api.GET("/Registries/:id", service.GetRegistryFile)

		// BootOptions — one member per Boot#### variable, POSTed by the
		// host's boot manager whenever it re-enumerates.
		api.GET("/Systems/1/BootOptions", service.GetBootOptionCollection)
		api.POST("/Systems/1/BootOptions", service.PostBootOption)
		api.GET("/Systems/1/BootOptions/:option", service.GetBootOption)
		api.PATCH("/Systems/1/BootOptions/:option", service.PatchBootOption)
		api.DELETE("/Systems/1/BootOptions/:option", service.DeleteBootOption)

		// SecureBoot — host-owned state, reported over the host interface.
		api.GET("/Systems/1/SecureBoot", service.GetSecureBoot)
		api.PATCH("/Systems/1/SecureBoot", service.PatchSecureBoot)

		// Per-device inventory collections, all host-reported.
		api.GET("/Systems/1/Memory", service.GetMemoryCollection)
		api.POST("/Systems/1/Memory", service.PostMemoryModule)
		api.GET("/Systems/1/Memory/:module", service.GetMemoryModule)
		api.PATCH("/Systems/1/Memory/:module", service.PatchMemoryModule)
		api.GET("/Systems/1/Processors", service.GetProcessorCollection)
		api.POST("/Systems/1/Processors", service.PostProcessor)
		api.GET("/Systems/1/Processors/:processor", service.GetProcessor)
		api.PATCH("/Systems/1/Processors/:processor", service.PatchProcessor)
		api.DELETE("/Systems/1/Processors/:processor", service.DeleteProcessor)
		api.GET("/Systems/1/EthernetInterfaces", service.GetEthernetInterfaceCollection)
		api.GET("/Systems/1/EthernetInterfaces/:nic", service.GetEthernetInterface)

		// Storage — subsystem "1" is the host's own storage (drives the host
		// firmware reports); "BMC" is the USB gadget's LUNs.
		api.GET("/Systems/1/Storage", service.GetStorageCollection)
		api.GET("/Systems/1/Storage/:storage", service.GetStorage)
		api.GET("/Systems/1/Storage/:storage/Drives/:drive", service.GetDrive)
		api.POST("/Systems/1/Storage/:storage/Drives", service.PostHostDrive)
		api.PATCH("/Systems/1/Storage/:storage/Drives/:drive", service.PatchHostDrive)

		// Chassis — the host baseboard; the service root has always
		// advertised this collection.
		api.GET("/Chassis", service.GetChassisCollection)
		api.GET("/Chassis/1", service.GetChassis)
		// The host's thermal feature driver GETs then PATCHes this on every
		// boot; a 404 here ends its walk.
		api.GET("/Chassis/1/Thermal", service.GetChassisThermal)
		api.PATCH("/Chassis/1/Thermal", service.PatchChassisThermal)

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

		// UpdateService (host image updates)
		api.GET("/UpdateService", service.GetUpdateService)
		api.GET("/UpdateService/FirmwareInventory", service.GetFirmwareInventoryCollection)
		api.GET("/UpdateService/FirmwareInventory/BIOS", service.GetFirmwareInventoryBIOS)
		api.POST("/UpdateService/Actions/UpdateService.SimpleUpdate", service.SimpleUpdate)
	}
}
