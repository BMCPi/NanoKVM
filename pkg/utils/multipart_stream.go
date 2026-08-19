package utils

// multipart_stream.go exists because this BMC cannot afford the standard
// library's default answer to "give me the uploaded file".
//
// http.Request.FormFile / ParseMultipartForm call mime/multipart's ReadForm,
// which buffers maxMemory bytes in RAM and spools EVERY remaining byte to a
// temp file in os.TempDir() before the handler is handed a reader. On this
// device the root filesystem is a squashfs with a tmpfs overlay upper capped
// at 25% of RAM (see the initramfs init in nanokvm-build), so os.TempDir()
// is RAM. Uploading an ISO that way needs the whole image resident before a
// single byte reaches the media directory on the data partition: the tmpfs
// hits ENOSPC or the OOM killer takes the server, the client's upload dies
// mid-stream as a bare connection error, and the BMC is gone.
//
// StreamMultipartFile walks the request body part-by-part instead and hands
// the caller the file part while it is still on the wire, so an upload of any
// size costs one copy buffer of memory and lands directly on persistent
// storage.

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// maxMultipartValueBytes caps a single non-file form field read into memory.
// The metadata parts that legitimately accompany an image upload (a JSON
// request body, a target filename) are tiny; anything larger is a client bug
// or an attempt to make the BMC buffer a payload it deliberately refuses to
// buffer.
const maxMultipartValueBytes = 1 << 20 // 1 MiB

var (
	// ErrNoFilePart is returned when the request carried no file part (or
	// none matching the requested field names).
	ErrNoFilePart = errors.New("no file part found in multipart request")

	// ErrUploadTooLarge is returned from Read once the file part exceeds the
	// limit passed to StreamMultipartFile.
	ErrUploadTooLarge = errors.New("upload exceeds maximum allowed size")
)

// StreamedUpload is a multipart file part still being received. It reads as
// the file's bytes; the request body is consumed as the caller reads, so the
// caller must copy it to its destination before responding.
type StreamedUpload struct {
	// FormName and Filename come from the part's Content-Disposition.
	FormName string
	Filename string

	// Values holds the non-file fields. On return from StreamMultipartFile
	// it contains only the fields that appeared BEFORE the file part —
	// fields after it are still unread on the wire. Call Rest to add them.
	Values map[string]string

	part   *multipart.Part
	reader *multipart.Reader
	body   io.Reader // part, wrapped in the size cap when one was requested
}

func (u *StreamedUpload) Read(p []byte) (int, error) { return u.body.Read(p) }

// Close releases the file part. It does not drain the rest of the request.
func (u *StreamedUpload) Close() error { return u.part.Close() }

// Rest consumes the parts that follow the file part, adding any non-file
// fields to Values, and returns Values. Call it after the file body has been
// read when a trailing metadata part matters (some Redfish clients put the
// InsertMediaRequestBody after the image). Malformed trailing parts are
// ignored: the file is already safely on disk by then, and failing the whole
// upload over unreadable trailing metadata would be the worse outcome.
func (u *StreamedUpload) Rest() map[string]string {
	for {
		part, err := u.reader.NextRawPart()
		if err != nil {
			return u.Values
		}
		if part.FileName() == "" {
			if v, err := readPartValue(part); err == nil {
				u.Values[part.FormName()] = v
			}
		}
		_ = part.Close()
	}
}

// StreamMultipartFile returns the first file part of r's multipart body whose
// form name matches one of names, or the first file part of any name when
// names is empty. Non-file fields encountered on the way are collected into
// the result's Values.
//
// maxBytes caps the file part; pass 0 for no cap. The cap is enforced while
// reading, so an oversized upload fails at the limit rather than after the
// BMC has written it.
//
// The returned upload holds the live request body: read it to completion (or
// Close it) before writing a response.
func StreamMultipartFile(r *http.Request, maxBytes int64, names ...string) (*StreamedUpload, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("invalid multipart data: %w", err)
	}

	values := make(map[string]string)
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart: %w", err)
		}

		if part.FileName() == "" {
			// A small metadata field: safe to hold in memory.
			if v, err := readPartValue(part); err == nil {
				values[part.FormName()] = v
			}
			_ = part.Close()
			continue
		}

		if !matchesFormName(part.FormName(), names) {
			_ = part.Close()
			continue
		}

		u := &StreamedUpload{
			FormName: part.FormName(),
			Filename: part.FileName(),
			Values:   values,
			part:     part,
			reader:   reader,
		}
		u.body = part
		if maxBytes > 0 {
			u.body = &cappedReader{r: part, remaining: maxBytes, err: ErrUploadTooLarge}
		}
		return u, nil
	}

	if len(names) > 0 {
		return nil, fmt.Errorf("%w; expected one of: %s", ErrNoFilePart, strings.Join(names, ", "))
	}
	return nil, ErrNoFilePart
}

func matchesFormName(got string, names []string) bool {
	if len(names) == 0 {
		return true
	}
	for _, n := range names {
		if got == n {
			return true
		}
	}
	return false
}

// readPartValue reads a non-file part into a string, refusing to grow past
// maxMultipartValueBytes.
func readPartValue(part *multipart.Part) (string, error) {
	b, err := io.ReadAll(io.LimitReader(part, maxMultipartValueBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > maxMultipartValueBytes {
		return "", fmt.Errorf("form field %q exceeds %d bytes", part.FormName(), maxMultipartValueBytes)
	}
	return string(b), nil
}

// cappedReader is io.LimitReader that reports an error instead of a silent
// EOF when the limit is hit — a truncated ISO written as if it succeeded is
// worse than a failed upload.
//
// It reads one byte past the budget to tell "exactly at the limit" from "over
// it": a file of exactly maxBytes drives remaining to 0 and must still be
// allowed to reach EOF, so the overflow is only declared once a byte actually
// arrives beyond it.
type cappedReader struct {
	r         io.Reader
	remaining int64
	err       error // reported once the budget is blown
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.remaining < 0 {
		return 0, c.err
	}
	if int64(len(p)) > c.remaining+1 {
		p = p[:c.remaining+1]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	if c.remaining < 0 {
		return 0, c.err
	}
	return n, err
}
