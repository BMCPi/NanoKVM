package redfish

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"
)

func (s *Service) GetServiceRoot(c *gin.Context) {
	c.JSON(http.StatusOK, ServiceRoot{
		Resource: Resource{
			ODataType:    "#ServiceRoot.v1_9_0.ServiceRoot",
			ODataID:      ServiceRootPath,
			ODataContext: context("ServiceRoot.ServiceRoot"),
			ID:           "ServiceRoot",
			Name:         "NanoKVM BMC",
		},
		RedfishVersion: "1.0.0",
		Systems:        Link(systemsPath),
		Managers:       Link(managersPath),
		Chassis:        Link(chassisPath),
		SessionService: Link(sessionServicePath),
		UpdateService:  Link(updateServicePath),
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
