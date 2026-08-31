package application

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/application"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
)

var validFilenameRegex = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// OfflineUpdate installs an uploaded package (multipart field "file") and
// restarts the service. application.RunOfflineUpdate owns the update lock
// and the cache lifecycle; this handler only stages the upload.
func (h *handlers) OfflineUpdate(c *gin.Context) {
	var rsp proto.Response

	err := application.RunOfflineUpdate(h.log, func(cacheDir string) (string, error) {
		reader, err := c.Request.MultipartReader()
		if err != nil {
			return "", fmt.Errorf("invalid multipart data: %w", err)
		}
		return h.processUpload(reader, cacheDir)
	})
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "offline update failed", slog.Any("err", err))
		rsp.ErrRsp(c, -1, fmt.Sprintf("update failed: %s", err))
		return
	}

	h.log.DebugContext(c.Request.Context(), "offline update application success")
	rsp.OkRsp(c)
	// The process context, not the request's: the restart must outlive this
	// request, and the wait it bounds should only skip at shutdown, not on a
	// browser navigating away.
	application.RestartAfter(h.d.Ctx, application.RestartDelay, h.log)
}

// processUpload reads the multipart stream and stages the "file" field
// into destDir, returning the staged path.
func (h *handlers) processUpload(reader *multipart.Reader, destDir string) (string, error) {
	var outPath string

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("failed to read multipart: %w", err)
		}

		if part.FormName() != "file" {
			continue
		}

		outPath, err = h.saveUploadedFile(part, destDir)
		if err != nil {
			return "", err
		}
	}

	if outPath == "" {
		return "", fmt.Errorf("no file uploaded")
	}

	return outPath, nil
}

func (h *handlers) saveUploadedFile(part *multipart.Part, destDir string) (string, error) {
	filename := part.FileName()
	if filename == "" {
		return "", fmt.Errorf("no filename provided")
	}

	if err := h.validateFilename(filename); err != nil {
		return "", err
	}

	outPath := filepath.Join(destDir, filename)
	out, err := os.Create(outPath)
	if err != nil {
		return "", fmt.Errorf("failed to create output file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, part); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return outPath, nil
}

func (h *handlers) validateFilename(filename string) error {
	baseName := filepath.Base(filename)

	// Check if the path contains directory components
	if baseName != filename {
		h.log.Warn("path detected in filename", slog.String("filename", filename))
		return fmt.Errorf("path detected in filename")
	}

	// Check for path traversal attempts
	if strings.Contains(filename, "..") {
		h.log.Warn("path traversal attempt", slog.String("filename", filename))
		return fmt.Errorf("invalid filename: path traversal detected")
	}

	// Validate filename characters
	if !validFilenameRegex.MatchString(filename) {
		h.log.Warn("invalid filename characters", slog.String("filename", filename))
		return fmt.Errorf("invalid filename: contains invalid characters")
	}

	return nil
}
