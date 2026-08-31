package middleware

import (
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
)

const cookieName = "nano-kvm-token"

// pkgLogHolder backs ParseJWT's debug logging. ParseJWT is reached from many
// places — CheckToken and ResolveAuth below, but also CheckPageAuth and
// api/redfish's sessionUsername, which have no logger of their own to hand
// it. Rather than let whichever caller happens to construct CheckToken or
// ResolveAuth first (or last) stamp this shared debug line with its own
// component forever — main.go, ui.go, api/auth and api/redfish's Register
// all build one, in an order this package does not control — the line gets
// one fixed identity, set exactly once by main via SetLogger. CheckToken and
// ResolveAuth's own log parameters are NOT written here; they stay scoped to
// their own closures (see their doc comments). See logger.Holder's doc
// comment for why a sync.Once-guarded var would still get this wrong for a
// caller that reaches ParseJWT before SetLogger has run.
var pkgLogHolder logger.Holder

// SetLogger seeds the component identity for pkg/middleware's shared,
// caller-independent log sites — currently just ParseJWT's debug line, which
// CheckToken and ResolveAuth both reach indirectly. Call it once, at startup;
// per the design's component taxonomy this rides the same "http" component
// as RequestLogger/Recovery, so main seeds it right alongside them. Safe to
// call more than once (e.g. in tests), but doing so from more than one
// component defeats the point — this is not a per-caller logger.
func SetLogger(log *slog.Logger) {
	pkgLogHolder.Set(log)
}

// pkgLog returns the package's fixed-identity logger, defaulting to the
// process logger before SetLogger has run.
func pkgLog() *slog.Logger {
	return pkgLogHolder.Get()
}

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

// CheckToken gates API routes on a valid token, aborting with 401 otherwise.
// Rejections are logged through the caller's own logger (its component, not
// the shared identity ParseJWT's debug line uses — see pkgLogHolder's doc
// comment) since "who rejected this request" belongs to the caller's chain.
func CheckToken(log *slog.Logger) gin.HandlerFunc {
	log = logger.Or(log)
	return func(c *gin.Context) {
		if allowByToken(c) {
			c.Next()
			return
		}

		log.DebugContext(c.Request.Context(), "check token: rejected",
			slog.String("path", c.Request.URL.Path))
		abortUnauthorized(c)
	}
}

// ResolveAuth inspects the JWT cookie (or auth-disabled config) and sets
// the authedKey context flag accordingly. It NEVER redirects or aborts;
// downstream handlers may render different content for authed vs guest.
// If the cookie is present but invalid/expired it is cleared.
//
// Like CheckToken, it logs through the caller's own logger — the cookie is
// scoped to the caller's chain (ui, api/auth, ...), not the shared identity
// ParseJWT's debug line carries.
func ResolveAuth(log *slog.Logger) gin.HandlerFunc {
	log = logger.Or(log)
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
			log.DebugContext(c.Request.Context(), "resolve auth: clearing stale cookie")
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

	// refreshTokenDuration is an operator-supplied count of seconds. Clamp it to
	// the largest span time.Duration can hold so an absurd value in server.yaml
	// yields a far-future expiry rather than wrapping negative — which would
	// mint tokens that are already expired.
	const maxExpireSeconds = uint64(math.MaxInt64) / uint64(time.Second)

	seconds := conf.JWT.RefreshTokenDuration
	if seconds > maxExpireSeconds {
		seconds = maxExpireSeconds
	}

	expireDuration := time.Duration(seconds) * time.Second

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

	t, err := jwt.ParseWithClaims(jwtToken, &Token{}, func(_ *jwt.Token) (any, error) {
		return []byte(conf.JWT.SecretKey), nil
	})
	if err != nil {
		pkgLog().Debug("parse jwt error", slog.Any("err", err))
		return nil, err
	}

	if claims, ok := t.Claims.(*Token); ok && t.Valid {
		return claims, nil
	}

	// err is the (nil) parse error; preserved verbatim from the previous
	// else-branch so this path keeps returning exactly what it always has.
	return nil, err
}
