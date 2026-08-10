package middleware

import (
	"net/http"
	"strings"
)

// NewLoopbackHTTPRedirect returns an unstarted HTTP server that redirects all
// requests to HTTPS. The caller owns its lifecycle (ListenAndServe/Shutdown),
// so it participates in the same graceful shutdown as the main server.
func NewLoopbackHTTPRedirect(httpPort string, httpsPort string) *http.Server {
	return &http.Server{
		Addr: httpPort,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			host := req.Host
			if strings.Contains(host, httpPort) {
				host = strings.Split(host, httpPort)[0]
			}

			targetURL := "https://" + host + req.URL.String()
			if httpsPort != ":443" {
				targetURL = "https://" + host + httpsPort + req.URL.String()
			}

			http.Redirect(w, req, targetURL, http.StatusTemporaryRedirect)
		}),
	}
}
