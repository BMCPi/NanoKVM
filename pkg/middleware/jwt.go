package middleware

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

const cookieName = "nano-kvm-token"

// authedKey is the gin.Context key set to true when the request was
// authenticated (or when auth is globally disabled). Read it via IsAuthed.
const authedKey = "authed"

// IsAuthed reports whether the current request was authenticated. It is
// safe to call from any handler; returns false if no auth middleware ran.
func IsAuthed(c *gin.Context) bool {
	return c.GetBool(authedKey)
}

type Token struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

func CheckToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		if allowByToken(c) {
			c.Next()
			return
		}

		abortUnauthorized(c)
	}
}

// ResolveAuth inspects the JWT cookie (or auth-disabled config) and sets
// the authedKey context flag accordingly. It NEVER redirects or aborts;
// downstream handlers may render different content for authed vs guest.
// If the cookie is present but invalid/expired it is cleared.
func ResolveAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		conf := config.GetInstance()
		if conf.Authentication == "disable" {
			c.Set(authedKey, true)
			c.Next()
			return
		}

		cookie, err := c.Cookie(cookieName)
		if err != nil || cookie == "" {
			c.Next()
			return
		}

		if _, err := ParseJWT(cookie); err != nil {
			// Clear stale cookie so the browser stops sending it.
			ClearAuthCookie(c)
			c.Next()
			return
		}

		c.Set(authedKey, true)
		c.Next()
	}
}

// RequireAuth redirects unauthenticated requests to the login page.
// It must run AFTER ResolveAuth so the authedKey flag is populated.
func RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if IsAuthed(c) {
			c.Next()
			return
		}
		c.Redirect(http.StatusFound, "/auth/login")
		c.Abort()
	}
}

// CheckPageAuth protects server-rendered pages by validating the JWT
// cookie. On failure it clears the stale cookie and redirects to the
// login page. When authentication is disabled globally it passes through.
//
// Equivalent to chaining ResolveAuth + RequireAuth as separate middlewares
// on a route group; provided as a single handler for callers that register
// middleware individually (e.g. tests, ad-hoc routes).
func CheckPageAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		conf := config.GetInstance()
		if conf.Authentication == "disable" {
			c.Set(authedKey, true)
			c.Next()
			return
		}

		cookie, err := c.Cookie(cookieName)
		if err != nil || cookie == "" {
			c.Redirect(http.StatusFound, "/auth/login")
			c.Abort()
			return
		}

		if _, err := ParseJWT(cookie); err != nil {
			// Token invalid or expired — clear it so the browser stops
			// sending the stale value on every request.
			ClearAuthCookie(c)
			c.Redirect(http.StatusFound, "/auth/login")
			c.Abort()
			return
		}

		c.Set(authedKey, true)
		c.Next()
	}
}

func allowByToken(c *gin.Context) bool {
	conf := config.GetInstance()

	if conf.Authentication == "disable" {
		return true
	}

	// Web UI uses an HttpOnly cookie; Redfish clients (gofish/bmclib/Dell
	// Terraform provider) use the X-Auth-Token header set by the session
	// create response. Accept either.
	if token := c.GetHeader("X-Auth-Token"); token != "" {
		if _, err := ParseJWT(token); err == nil {
			return true
		}
	}

	cookie, err := c.Cookie(cookieName)
	if err != nil {
		return false
	}

	_, err = ParseJWT(cookie)
	return err == nil
}

func abortUnauthorized(c *gin.Context) {
	c.JSON(http.StatusUnauthorized, "unauthorized")
	c.Abort()
}

// ClearAuthCookie expires the nano-kvm-token cookie.
func ClearAuthCookie(c *gin.Context) {
	c.SetCookie(cookieName, "", -1, "/", "", false, false)
}

func GenerateJWT(username string) (string, error) {
	conf := config.GetInstance()

	expireDuration := time.Duration(conf.JWT.RefreshTokenDuration) * time.Second

	claims := Token{
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expireDuration)),
		},
	}

	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	return t.SignedString([]byte(conf.JWT.SecretKey))
}

func ParseJWT(jwtToken string) (*Token, error) {
	conf := config.GetInstance()

	t, err := jwt.ParseWithClaims(jwtToken, &Token{}, func(token *jwt.Token) (any, error) {
		return []byte(conf.JWT.SecretKey), nil
	})
	if err != nil {
		log.Debugf("parse jwt error: %s", err)
		return nil, err
	}

	if claims, ok := t.Claims.(*Token); ok && t.Valid {
		return claims, nil
	} else {
		return nil, err
	}
}
