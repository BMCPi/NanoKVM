package redfish

import (
	"fmt"
	"net/http"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/auth"
	"github.com/pi-bmc/nanokvm-app/pkg/middleware"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func (s *Service) GetSessionService(c *gin.Context) {
	c.JSON(http.StatusOK, SessionService{
		Resource: Resource{
			ODataType:    "#SessionService.v1_1_8.SessionService",
			ODataID:      sessionServicePath,
			ODataContext: odataContext("SessionService.SessionService"),
			ID:           "SessionService",
			Name:         "Session Service",
		},
		ServiceEnabled: true,
		Sessions:       Link(sessionsPath),
	})
}

func (s *Service) CreateSession(c *gin.Context) {
	var req struct {
		UserName string `json:"UserName"`
		Password string `json:"Password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "invalid request body")
		return
	}

	if ok := auth.ComparePlainAccount(req.UserName, req.Password); !ok {
		time.Sleep(2 * time.Second)
		redfishErrorResponse(c, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, err := middleware.GenerateJWT(req.UserName)
	if err != nil {
		redfishErrorResponse(c, http.StatusInternalServerError, "failed to generate token")
		return
	}

	sessionID := fmt.Sprintf("%d", time.Now().UnixNano())

	log.Debugf("redfish session created for user: %s", req.UserName)

	c.Header("X-Auth-Token", token)
	c.JSON(http.StatusCreated, Session{
		Resource: Resource{
			ODataType:    "#Session.v1_3_0.Session",
			ODataID:      sessionsPath + "/" + sessionID,
			ODataContext: odataContext("Session.Session"),
			ID:           sessionID,
			Name:         "User Session",
		},
		UserName: req.UserName,
	})
}

func (s *Service) GetSessionCollection(c *gin.Context) {
	// Sessions are stateless JWTs, so the collection is always empty.
	c.JSON(http.StatusOK, newCollection(
		"SessionCollection", "Session Collection", sessionsPath,
	))
}

func (s *Service) DeleteSession(c *gin.Context) {
	// Stub — session management is not persisted
	c.Status(http.StatusNoContent)
}
