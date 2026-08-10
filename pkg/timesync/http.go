package timesync

import (
	"context"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"

	log "github.com/sirupsen/logrus"
)

// fallbackHTTPURLs are captive-portal probe endpoints: tiny responses, very
// high availability, and an accurate Date header. Used as the last resort when
// UDP/123 is blocked. Second-granularity is good enough to bootstrap TLS.
var fallbackHTTPURLs = []string{
	"http://www.gstatic.com/generate_204",
	"http://cp.cloudflare.com/",
	"http://edge-http.microsoft.com/captiveportal/generate_204",
}

// queryHTTP races the URLs in chunks of queryParallel and returns the first
// parsable Date header.
func queryHTTP(urls []string) (time.Time, bool) {
	if len(urls) == 0 {
		return time.Time{}, false
	}
	urls = slices.Clone(urls)
	rand.Shuffle(len(urls), func(i, j int) { urls[i], urls[j] = urls[j], urls[i] })

	for chunk := range slices.Chunk(urls, queryParallel) {
		if t, ok := queryHTTPChunk(chunk); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func queryHTTPChunk(urls []string) (time.Time, bool) {
	// Buffered so stragglers can finish and exit after we return.
	results := make(chan *time.Time, len(urls))

	for _, url := range urls {
		go func() {
			t, err := queryHTTPTime(url)
			if err != nil {
				log.Debugf("timesync: http %s: %v", url, err)
				results <- nil
				return
			}
			results <- t
		}()
	}

	for range urls {
		if t := <-results; t != nil {
			return *t, true
		}
	}
	return time.Time{}, false
}

func queryHTTPTime(url string) (*time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
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
