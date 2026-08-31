package utils

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

func Download(req *http.Request, target string) error {
	log.Debugf("downloading %s to %s", req.URL.String(), target)
	err := os.MkdirAll(filepath.Dir(target), 0o755)
	if err != nil {
		log.Errorf("create dir %s err: %s", filepath.Dir(target), err)
		return err
	}
	out, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		log.Errorf("cannot create file '%s', error: %s", target, err)
		return err
	}
	defer func() {
		_ = out.Close()
	}()

	// G704 reads the request's URL as tainted because it is a parameter rather
	// than a literal. Choosing the URL is this helper's contract: Download
	// takes an already-built request precisely so the caller decides what to
	// fetch. The only caller is pkg/application's updater, which builds the
	// request from the asset URL returned by this project's own GitHub release
	// API (a hardcoded api.github.com endpoint) — no request-supplied or
	// operator-supplied value reaches it, so there is nothing here for an
	// allowlist to filter that the caller has not already fixed.
	//nolint:gosec // G704: URL is the caller's by contract; the sole caller uses GitHub release metadata
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		log.Errorf("request error: %s", err)
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		log.Errorf("request failed, status code: %d", resp.StatusCode)
		return errors.New("update website is inaccessible right now")
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/octet-stream" && contentType != "application/zip" && contentType != "application/gzip" {
		log.Debugf("unexpected content-type, it should be either octet-stream or (g)zip, but got: %s", contentType)
		return errors.New("unsupported content type")
	}

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		log.Errorf("download file to %s err: %s", target, err)
		return err
	}

	return nil
}
