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
	sessionURI := sessionsPath + "/" + sessionID

	log.Debugf("redfish session created for user: %s", req.UserName)

	c.Header("X-Auth-Token", token)
	// Location is where the client learns its own session URI. gofish reads
	// it from this header and nowhere else, and bmclib's SessionActive()
	// (which gates every power/boot/system call it makes, and its
	// Compatible() probe) is just gofish's GetSession() — that returns
	// "client not authenticated" whenever the URI is empty. Omitting
	// Location therefore makes Rufio treat a BMC that accepted the
	// credentials as unauthenticated. DSP0266 requires the header on a 201
	// from Sessions for the same reason.
	c.Header("Location", sessionURI)
	c.JSON(http.StatusCreated, sessionResource(sessionID, req.UserName))
}

// GetSession returns the session identified in the URI.
//
// Sessions are stateless JWTs: the bearer already proved possession of a
// valid token to reach CheckAuth, so there is no server-side record to look
// up and the id is echoed back as given.
func (s *Service) GetSession(c *gin.Context) {
	c.JSON(http.StatusOK, sessionResource(c.Param("id"), sessionUsername(c)))
}

// sessionResource builds the Session body shared by create and get.
func sessionResource(id, username string) Session {
	return Session{
		Resource: Resource{
			ODataType:    "#Session.v1_3_0.Session",
			ODataID:      sessionsPath + "/" + id,
			ODataContext: odataContext("Session.Session"),
			ID:           id,
			Name:         "User Session",
		},
		UserName: username,
	}
}

// sessionUsername recovers the account name behind the current request from
// whichever credential CheckAuth accepted — the X-Auth-Token a Redfish
// client was issued, or the Basic header it sent instead. It returns ""
// (the property is omitempty) when neither is present: a host-interface
// request, an auth-disabled service, or a browser presenting only the web
// UI's cookie. None of those name a Redfish account.
func sessionUsername(c *gin.Context) string {
	if token := c.GetHeader("X-Auth-Token"); token != "" {
		if claims, err := middleware.ParseJWT(token); err == nil {
			return claims.Username
		}
	}
	if user, _, ok := c.Request.BasicAuth(); ok {
		return user
	}
	return ""
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
