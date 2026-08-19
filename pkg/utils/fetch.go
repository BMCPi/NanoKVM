package utils

// fetch.go centralises BMC-initiated downloads: the operator hands the BMC a
// URL (a virtual media ISO, a firmware capsule) and the BMC pulls the bytes
// itself.
//
// These downloads share the constraint that makes uploads dangerous on this
// device — see the header of multipart_stream.go. The body must be streamed
// to persistent storage rather than accumulated anywhere RAM-backed, and it
// must be bounded, because the remote decides how many bytes to send.
//
// FetchURL applies three bounds no caller should have to remember:
//
//   - a declared-size check, so a remote that honestly announces an oversized
//     Content-Length is refused before a single byte is written;
//   - a hard cap enforced while reading, which is what actually protects the
//     BMC when the remote omits or lies about Content-Length;
//   - connection and response-header timeouts, so a remote that accepts a
//     connection and then goes silent cannot pin a download goroutine (and
//     the "a fetch is in progress" latch guarding it) until the next reboot.
//
// The total transfer is deliberately NOT on a stopwatch: a multi-gigabyte ISO
// over a slow management link is a legitimate download. A peer that stalls
// mid-body after sending headers is therefore still only bounded by the cap
// and by the caller's context — cancelling that aborts the transfer, including
// a read already blocked on the body, which is how a download in flight is
// abandoned at shutdown instead of pinning the process.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ErrRemoteTooLarge is returned when a download exceeds its cap, either from
// the declared Content-Length or from the bytes actually received.
var ErrRemoteTooLarge = errors.New("remote file exceeds maximum allowed size")

// fetchClient bounds connection setup and the wait for response headers but
// not the transfer itself. http.DefaultClient has no timeouts at all, which on
// a BMC means one unreachable host can hold a fetch slot indefinitely.
var fetchClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
	},
}

// RemoteFile is an in-progress download. It reads as the remote body, capped
// at the requested limit, and must be closed by the caller.
type RemoteFile struct {
	// ContentLength is what the remote declared, or -1 when it declared
	// nothing. Useful for progress reporting; never trusted as a bound.
	ContentLength int64

	body io.ReadCloser
	r    io.Reader
}

func (f *RemoteFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *RemoteFile) Close() error               { return f.body.Close() }

// FetchURL issues a GET for rawURL and returns the response body bounded to
// maxBytes (pass 0 for no cap). Only http and https are accepted — the scheme
// check lives here so no caller can forget it, and so a redirect chain cannot
// land somewhere unexpected. A non-2xx response is an error and the body is
// closed before returning.
//
// Cancelling ctx aborts the transfer, so the returned RemoteFile's Read fails
// rather than blocking. The caller must still Close it.
func FetchURL(ctx context.Context, rawURL string, maxBytes int64) (*RemoteFile, error) {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("url must be an http or https URL")
	}

	// #nosec G107 — scheme validated immediately above.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("remote returned %d", resp.StatusCode)
	}

	// Refuse an honestly-declared oversize before writing anything at all.
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: remote declares %d bytes, limit is %d",
			ErrRemoteTooLarge, resp.ContentLength, maxBytes)
	}

	f := &RemoteFile{ContentLength: resp.ContentLength, body: resp.Body, r: resp.Body}
	if maxBytes > 0 {
		// The cap that actually matters: it holds when the remote sent no
		// Content-Length, or understated it.
		f.r = &cappedReader{r: resp.Body, remaining: maxBytes, err: ErrRemoteTooLarge}
	}
	return f, nil
}
