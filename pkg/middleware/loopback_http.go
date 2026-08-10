package middleware

import (
	"net/http"
	"strings"
)

// ListenAndServeLoopbackHTTPRedirect starts an HTTP server that redirects all
// requests to HTTPS.
func ListenAndServeLoopbackHTTPRedirect(
	httpPort string,
	httpsPort string,
	handler http.Handler,
) error {
	return http.ListenAndServe(httpPort, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host := req.Host
		if strings.Contains(host, httpPort) {
			host = strings.Split(host, httpPort)[0]
		}

		targetURL := "https://" + host + req.URL.String()
		if httpsPort != ":443" {
			targetURL = "https://" + host + httpsPort + req.URL.String()
		}

		http.Redirect(w, req, targetURL, http.StatusTemporaryRedirect)
	}))
}
