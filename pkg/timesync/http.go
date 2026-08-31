package timesync

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"time"
)

// fallbackHTTPURLs are captive-portal probe endpoints: tiny responses, very
// high availability, and an accurate Date header. Used as the last resort when
// UDP/123 is blocked. Second-granularity is good enough to bootstrap TLS.
var fallbackHTTPURLs = []string{
	"http://www.gstatic.com/generate_204",
	"http://cp.cloudflare.com/",
	"http://edge-http.microsoft.com/captiveportal/generate_204",
}

// queryClient bounds connection setup and the wait for response headers for
// the HTTP time-fallback probes -- http.DefaultClient has no timeouts at all,
// which would let one unreachable captive-portal endpoint pin a query
// goroutine indefinitely. Same shape as pkg/utils/fetch.go's fetchClient,
// scaled to queryTimeout since these are tiny, latency-sensitive probes
// rather than downloads: the overall Timeout is a backstop behind the
// per-request ctx deadline queryHTTPTime already applies.
var queryClient = &http.Client{
	Timeout: queryTimeout + time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   queryTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   queryTimeout,
		ResponseHeaderTimeout: queryTimeout,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
	},
}

// queryHTTP races the URLs in chunks of queryParallel and returns the first
// parsable Date header. Abandons the remaining chunks as soon as ctx is
// cancelled.
func queryHTTP(ctx context.Context, urls []string) (time.Time, bool) {
	if len(urls) == 0 {
		return time.Time{}, false
	}
	urls = slices.Clone(urls)
	rand.Shuffle(len(urls), func(i, j int) { urls[i], urls[j] = urls[j], urls[i] })

	for chunk := range slices.Chunk(urls, queryParallel) {
		if ctx.Err() != nil {
			return time.Time{}, false
		}
		if t, ok := queryHTTPChunk(ctx, chunk); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// queryHTTPChunk races one chunk and collects results with drain-or-abandon
// semantics: it waits for either a parsed Date header or ctx to be
// cancelled, whichever comes first, rather than blocking on every straggler.
// results is buffered to len(urls) so an abandoned goroutine's send never
// blocks.
func queryHTTPChunk(ctx context.Context, urls []string) (time.Time, bool) {
	results := make(chan *time.Time, len(urls))

	for _, url := range urls {
		go func() {
			t, err := queryHTTPTime(ctx, url)
			if err != nil {
				pkgLog().Debug("timesync: http query failed", slog.String("url", url), slog.Any("err", err))
				results <- nil
				return
			}
			results <- t
		}()
	}

	for range urls {
		select {
		case t := <-results:
			if t != nil {
				return *t, true
			}
		case <-ctx.Done():
			return time.Time{}, false
		}
	}
	return time.Time{}, false
}

func queryHTTPTime(ctx context.Context, url string) (*time.Time, error) {
	reqCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := queryClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	t, err := time.Parse(time.RFC1123, resp.Header.Get("Date"))
	if err != nil {
		return nil, err
	}
	return &t, nil
}
