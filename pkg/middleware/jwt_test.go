package middleware

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// initTestConfig forces the config singleton to initialize and overrides
// fields needed for auth tests.
func initTestConfig() {
	conf := config.GetInstance()
	conf.Authentication = "enable"
	conf.JWT.SecretKey = "test-secret-key-for-unit-tests"
	conf.JWT.RefreshTokenDuration = 3600 // 1 hour
}

// validToken generates a non-expired JWT signed with the test secret.
func validToken(t *testing.T) string {
	t.Helper()
	tok, err := GenerateJWT("admin")
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}
	return tok
}

// expiredToken generates a JWT that expired 1 hour ago.
func expiredToken(t *testing.T) string {
	t.Helper()
	conf := config.GetInstance()
	claims := Token{
		Username: "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(conf.JWT.SecretKey))
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}
	return s
}

func TestCheckPageAuth_NoCookie_RedirectsToLogin(t *testing.T) {
	initTestConfig()

	w := httptest.NewRecorder()
	c, r := gin.CreateTestContext(w)
	r.GET("/dashboard", CheckPageAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	r.ServeHTTP(w, c.Request)

	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusFound)
	}
	loc := w.Header().Get("Location")
	if loc != "/auth/login" {
		t.Errorf("Location = %q, want /auth/login", loc)
	}
}

func TestCheckPageAuth_EmptyCookie_RedirectsToLogin(t *testing.T) {
	initTestConfig()

	w := httptest.NewRecorder()
	c, r := gin.CreateTestContext(w)
	r.GET("/dashboard", CheckPageAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	c.Request.AddCookie(&http.Cookie{Name: cookieName, Value: ""})
	r.ServeHTTP(w, c.Request)

	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusFound)
	}
}

func TestCheckPageAuth_ExpiredToken_RedirectsAndClearsCookie(t *testing.T) {
	initTestConfig()

	w := httptest.NewRecorder()
	c, r := gin.CreateTestContext(w)
	r.GET("/settings", CheckPageAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/settings", nil)
	c.Request.AddCookie(&http.Cookie{Name: cookieName, Value: expiredToken(t)})
	r.ServeHTTP(w, c.Request)

	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusFound)
	}

	// Verify the stale cookie is cleared in the response.
	found := false
	for _, ck := range w.Result().Cookies() {
		if ck.Name == cookieName {
			found = true
			if ck.MaxAge != -1 {
				t.Errorf("cookie MaxAge = %d, want -1", ck.MaxAge)
			}
		}
	}
	if !found {
		t.Error("expected Set-Cookie header to clear nano-kvm-token")
	}
}

func TestCheckPageAuth_InvalidToken_Redirects(t *testing.T) {
	initTestConfig()

	w := httptest.NewRecorder()
	c, r := gin.CreateTestContext(w)
	r.GET("/console", CheckPageAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/console", nil)
	c.Request.AddCookie(&http.Cookie{Name: cookieName, Value: "not-a-jwt"})
	r.ServeHTTP(w, c.Request)

	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusFound)
	}
}

func TestCheckPageAuth_WrongSecret_Redirects(t *testing.T) {
	initTestConfig()

	// Generate a token signed with a different secret.
	claims := Token{
		Username: "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte("wrong-secret"))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	w := httptest.NewRecorder()
	c, r := gin.CreateTestContext(w)
	r.GET("/dashboard", CheckPageAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	c.Request.AddCookie(&http.Cookie{Name: cookieName, Value: s})
	r.ServeHTTP(w, c.Request)

	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusFound)
	}
}

func TestCheckPageAuth_ValidToken_PassesThrough(t *testing.T) {
	initTestConfig()

	w := httptest.NewRecorder()
	c, r := gin.CreateTestContext(w)
	r.GET("/dashboard", CheckPageAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	c.Request.AddCookie(&http.Cookie{Name: cookieName, Value: validToken(t)})
	r.ServeHTTP(w, c.Request)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestCheckPageAuth_AuthDisabled_PassesWithoutCookie(t *testing.T) {
	initTestConfig()
	conf := config.GetInstance()
	conf.Authentication = "disable"
	defer func() { conf.Authentication = "enable" }()

	w := httptest.NewRecorder()
	c, r := gin.CreateTestContext(w)
	r.GET("/dashboard", CheckPageAuth(), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	r.ServeHTTP(w, c.Request)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestCheckToken_NoCookie_Returns401(t *testing.T) {
	initTestConfig()

	w := httptest.NewRecorder()
	c, r := gin.CreateTestContext(w)
	r.GET("/api/test", CheckToken(slog.New(slog.DiscardHandler)), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/api/test", nil)
	r.ServeHTTP(w, c.Request)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestCheckToken_ValidCookie_PassesThrough(t *testing.T) {
	initTestConfig()

	w := httptest.NewRecorder()
	c, r := gin.CreateTestContext(w)
	r.GET("/api/test", CheckToken(slog.New(slog.DiscardHandler)), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/api/test", nil)
	c.Request.AddCookie(&http.Cookie{Name: cookieName, Value: validToken(t)})
	r.ServeHTTP(w, c.Request)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestCheckToken_ExpiredCookie_Returns401(t *testing.T) {
	initTestConfig()

	w := httptest.NewRecorder()
	c, r := gin.CreateTestContext(w)
	r.GET("/api/test", CheckToken(slog.New(slog.DiscardHandler)), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/api/test", nil)
	c.Request.AddCookie(&http.Cookie{Name: cookieName, Value: expiredToken(t)})
	r.ServeHTTP(w, c.Request)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestGenerateJWT_RoundTrip(t *testing.T) {
	initTestConfig()

	tok, err := GenerateJWT("testuser")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	claims, err := ParseJWT(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.Username != "testuser" {
		t.Errorf("username = %q, want %q", claims.Username, "testuser")
	}
}

func TestParseJWT_DifferentSecret_Fails(t *testing.T) {
	initTestConfig()

	tok, err := GenerateJWT("admin")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Change the secret — simulates a server restart with new key
	conf := config.GetInstance()
	origKey := conf.JWT.SecretKey
	conf.JWT.SecretKey = "rotated-secret-key"
	defer func() { conf.JWT.SecretKey = origKey }()

	_, err = ParseJWT(tok)
	if err == nil {
		t.Error("expected parse to fail with rotated secret, but it succeeded")
	}
}
