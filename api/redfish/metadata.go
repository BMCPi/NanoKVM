package redfish

// metadata.go serves /redfish/v1/$metadata, the OData CSDL service document.
//
// Every resource we emit carries an @odata.context of the form
// "/redfish/v1/$metadata#Thing.Thing" (see odataContext), so the document has
// been advertised on every response since the service was written while the
// URI itself 404'd. DSP0266 §7.1 requires it, and conformance checkers and
// stricter OData clients resolve the context before trusting a payload.
//
// The body is a static EDMX document referencing the DMTF-published schema
// for each type we actually serve, plus the entity container that names the
// top-level resources. It is static because the resource set is static: a new
// resource type means a new Reference line here, the same way it means a new
// route.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// metadataSchemas are the schema names referenced by the CSDL document, in
// the order they are emitted. Each becomes a Reference to the DMTF-hosted
// definition plus an Include of the namespace.
var metadataSchemas = []string{
	"ServiceRoot",
	"ComputerSystemCollection",
	"ComputerSystem",
	"ChassisCollection",
	"Chassis",
	"Thermal",
	"SensorCollection",
	"Sensor",
	"ManagerCollection",
	"Manager",
	"EthernetInterfaceCollection",
	"EthernetInterface",
	"NetworkInterfaceCollection",
	"SerialInterfaceCollection",
	"SerialInterface",
	"VirtualMediaCollection",
	"VirtualMedia",
	"StorageCollection",
	"Storage",
	"Drive",
	"MemoryCollection",
	"Memory",
	"ProcessorCollection",
	"Processor",
	"BootOptionCollection",
	"BootOption",
	"Bios",
	"SecureBoot",
	"SessionCollection",
	"Session",
	"SessionService",
	"UpdateService",
	"SoftwareInventoryCollection",
	"SoftwareInventory",
	"MessageRegistryFileCollection",
	"MessageRegistryFile",
	"AttributeRegistry",
}

// GetMetadata serves the CSDL service document.
func (s *Service) GetMetadata(c *gin.Context) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<edmx:Edmx xmlns:edmx="http://docs.oasis-open.org/odata/ns/edmx" Version="4.0">` + "\n")

	// The OData core vocabulary every Redfish CSDL document references first.
	b.WriteString(`  <edmx:Reference Uri="http://docs.oasis-open.org/odata/odata/v4.0/errata03/csd01/complete/vocabularies/Org.OData.Core.V1.xml">` + "\n")
	b.WriteString(`    <edmx:Include Namespace="Org.OData.Core.V1" Alias="OData"/>` + "\n")
	b.WriteString(`  </edmx:Reference>` + "\n")

	for _, name := range metadataSchemas {
		b.WriteString(`  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/` + name + `_v1.xml">` + "\n")
		b.WriteString(`    <edmx:Include Namespace="` + name + `"/>` + "\n")
		b.WriteString(`  </edmx:Reference>` + "\n")
	}

	b.WriteString(`  <edmx:DataServices>` + "\n")
	b.WriteString(`    <Schema xmlns="http://docs.oasis-open.org/odata/ns/edm" Namespace="Service">` + "\n")
	b.WriteString(`      <EntityContainer Name="Service" Extends="ServiceRoot.v1_0_0.ServiceContainer"/>` + "\n")
	b.WriteString(`    </Schema>` + "\n")
	b.WriteString(`  </edmx:DataServices>` + "\n")
	b.WriteString(`</edmx:Edmx>` + "\n")

	c.Data(http.StatusOK, "application/xml; charset=utf-8", []byte(b.String()))
}
