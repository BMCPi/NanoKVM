package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/unrolled/secure"
)

// TLS returns a handler that redirects plain-HTTP requests to HTTPS and
// aborts the chain on any redirect the secure middleware emits.
func TLS() gin.HandlerFunc {
	secureMiddleware := secure.New(secure.Options{
		SSLRedirect: true,
	})

	secureFunc := func(c *gin.Context) {
		err := secureMiddleware.Process(c.Writer, c.Request)
		if err != nil {
			c.Abort()
			return
		}

		if status := c.Writer.Status(); status > 300 && status < 399 {
			c.Abort()
		}
	}

	return secureFunc
}
