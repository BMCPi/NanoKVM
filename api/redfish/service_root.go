package redfish

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"
)

func (s *Service) GetServiceRoot(c *gin.Context) {
	c.JSON(http.StatusOK, ServiceRoot{
		Resource: Resource{
			ODataType:    odataTypeServiceRoot,
			ODataID:      ServiceRootPath,
			ODataContext: odataContext("ServiceRoot.ServiceRoot"),
			ID:           schemaNameServiceRoot,
			Name:         "NanoKVM BMC",
		},
		RedfishVersion: redfishProtocolVersion,
		// The same stable identity the Manager reports. Discovery tools read
		// it straight off the service root, before authenticating deeper.
		UUID:           managerUUID(),
		Systems:        Link(systemsPath),
		Managers:       Link(managersPath),
		Chassis:        Link(chassisPath),
		SessionService: Link(sessionServicePath),
		UpdateService:  Link(updateServicePath),
		Registries:     Link(registriesPath),
		// Links.Sessions is what gofish and other DMTF-conformant clients
		// POST to during Login() — without it they fail with "unable to
		// execute request, no target provided".
		Links: ServiceRootLinks{
			Sessions: Link(sessionsPath),
		},
	})
}

// GetRedfishBase handles GET /redfish and returns the Redfish version object.
func (s *Service) GetRedfishBase(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"v1": schemas.DefaultServiceRoot,
	})
}
