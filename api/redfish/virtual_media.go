package redfish

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// insertMediaRequest is the JSON body for VirtualMedia.InsertMedia.
// Accepted both as the application/json body for TransferMethod=Stream
// (the default) and as the "InsertMediaRequestBody" multipart part when
// the client uses TransferMethod=Upload.
// lastTransfer records the parameters of the most recent successful
// InsertMedia call so subsequent GETs can echo them back. The Dell
// terraform provider compares these against config on refresh and will
// raise "inconsistent result after apply" if they're missing.
var lastTransfer struct {
	sync.Mutex
	Method       string // "Stream" or "Upload"
	ProtocolType string // "HTTPS", "HTTP", "NFS", ...
	Image        string // last URL or filename
}

func recordTransfer(method, protocolType, image string) {
	lastTransfer.Lock()
	defer lastTransfer.Unlock()
	lastTransfer.Method = method
	lastTransfer.ProtocolType = protocolType
	lastTransfer.Image = image
}

type insertMediaRequest struct {
	Image          string `json:"Image"`
	TransferMethod string `json:"TransferMethod"` // "Stream" (default) or "Upload"
	Inserted       *bool  `json:"Inserted"`
	WriteProtected *bool  `json:"WriteProtected"`
	UserName       string `json:"UserName"` // accepted but ignored
	Password       string `json:"Password"` // accepted but ignored
}

// GetVirtualMediaCollection returns the VirtualMedia collection for Manager/1.
func (s *Service) GetVirtualMediaCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"VirtualMediaCollection", "Virtual Media Collection", virtualMediaPath,
		Link(virtualMediaCDPath),
	))
}

// GetVirtualMedia returns the single VirtualMedia resource (slot 1).
func (s *Service) GetVirtualMedia(c *gin.Context) {
	c.JSON(http.StatusOK, buildVirtualMediaResource(s.Firmware))
}

// InsertMedia handles POST …/VirtualMedia/1/Actions/VirtualMedia.InsertMedia.
//
// Two transfer methods are supported (per Redfish VirtualMedia v1_3_0):
//
//   - Stream (default) — JSON body with { "Image": "<http(s) URL>" }.
//     The BMC pulls the image from the URL and stages it.
//   - Upload — multipart/form-data push from the client. The request
//     carries the binary image as a file part plus an optional
//     "InsertMediaRequestBody" JSON part naming the file. This is how
//     redfishtool/gofish/python-redfish-utility ship local ISOs that
//     aren't reachable from the BMC's network.
func (s *Service) InsertMedia(c *gin.Context) {
	ctype, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if ctype == "multipart/form-data" {
		s.insertMediaUpload(c)
		return
	}
	s.insertMediaStream(c)
}

// insertMediaStream handles TransferMethod=Stream: BMC fetches the image
// from an HTTP(S) URL named in the JSON body.
func (s *Service) insertMediaStream(c *gin.Context) {
	var req insertMediaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TransferMethod != "" && !strings.EqualFold(req.TransferMethod, "Stream") {
		redfishErrorResponse(c, http.StatusBadRequest,
			"TransferMethod="+req.TransferMethod+" requires multipart/form-data; resend as multipart upload")
		return
	}
	if req.Image == "" {
		redfishErrorResponse(c, http.StatusBadRequest, "Image is required")
		return
	}

	// Validate URL — only http/https.
	parsed, err := url.ParseRequestURI(req.Image)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		redfishErrorResponse(c, http.StatusBadRequest, "Image must be an http or https URL")
		return
	}

	name := filepath.Base(parsed.Path)
	if name == "" || name == "." {
		name = "vm.iso"
	}

	// Download the ISO into the media staging directory.
	// #nosec G107 — scheme already validated above.
	resp, err := http.Get(req.Image) //nolint:noctx
	if err != nil {
		redfishErrorResponse(c, http.StatusBadGateway, "fetch failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		redfishErrorResponse(c, http.StatusBadGateway, fmt.Sprintf("remote returned %d", resp.StatusCode))
		return
	}

	if err := stageAndInsert(s.Firmware, name, resp.Body); err != nil {
		redfishErrorResponse(c, err.status, err.msg)
		return
	}

	protocol := strings.ToUpper(parsed.Scheme)
	recordTransfer("Stream", protocol, req.Image)
	log.Infof("redfish: virtual media inserted (stream): %s", name)
	c.JSON(http.StatusOK, buildVirtualMediaResource(s.Firmware))
}

// insertMediaUpload handles TransferMethod=Upload: the client pushes the
// image body as a multipart file part. An optional "InsertMediaRequestBody"
// JSON part may override the filename used when staging.
func (s *Service) insertMediaUpload(c *gin.Context) {
	// 8 GiB max upload — large enough for any installer ISO, small enough
	// that a runaway client can't exhaust the BMC's tmpfs.
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}

	var meta insertMediaRequest
	if v := c.Request.FormValue("InsertMediaRequestBody"); v != "" {
		if err := json.Unmarshal([]byte(v), &meta); err != nil {
			redfishErrorResponse(c, http.StatusBadRequest, "InsertMediaRequestBody: "+err.Error())
			return
		}
		if meta.TransferMethod != "" && !strings.EqualFold(meta.TransferMethod, "Upload") {
			redfishErrorResponse(c, http.StatusBadRequest,
				"TransferMethod="+meta.TransferMethod+" not valid for multipart upload")
			return
		}
	}

	// Accept the file under any of the conventional Redfish part names.
	file, header, err := firstFormFile(c, "Image", "file", "VirtualMediaImage")
	if err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, err.Error())
		return
	}
	defer file.Close()

	name := meta.Image
	if name == "" {
		name = header.Filename
	}
	name = filepath.Base(name)
	if name == "" || name == "." || name == "/" {
		name = "vm.iso"
	}

	if err := stageAndInsert(s.Firmware, name, file); err != nil {
		redfishErrorResponse(c, err.status, err.msg)
		return
	}

	recordTransfer("Upload", "", name)
	log.Infof("redfish: virtual media inserted (upload): %s (%d bytes)", name, header.Size)
	c.JSON(http.StatusOK, buildVirtualMediaResource(s.Firmware))
}

type InsertError struct {
	status int
	msg    string
}

func (e *InsertError) Error() string { return e.msg }

// stageAndInsert saves r to mediaDir/<name> then inserts it. Returns a
// typed error so callers can map to the appropriate HTTP status.
func stageAndInsert(fwCtrl *firmware.Controller, name string, r io.Reader) *InsertError {
	if _, err := fwCtrl.SaveMediaFile(name, r); err != nil {
		return &InsertError{http.StatusInternalServerError, "save media failed: " + err.Error()}
	}
	if err := fwCtrl.InsertVirtualMedia(name); err != nil {
		return &InsertError{http.StatusConflict, "insert media failed: " + err.Error()}
	}
	return nil
}

// firstFormFile returns the first multipart file part that matches any of
// the supplied field names. Redfish clients vary on which name they use
// (Image is the spec'd name; redfishtool uses "file"; some tools use the
// resource name VirtualMediaImage).
func firstFormFile(c *gin.Context, names ...string) (io.ReadCloser, *multipartHeader, error) {
	for _, n := range names {
		f, h, err := c.Request.FormFile(n)
		if err == nil {
			return f, &multipartHeader{Filename: h.Filename, Size: h.Size}, nil
		}
	}
	return nil, nil, fmt.Errorf("no file part found; expected one of: %s", strings.Join(names, ", "))
}

type multipartHeader struct {
	Filename string
	Size     int64
}

// EjectMedia handles POST …/VirtualMedia/1/Actions/VirtualMedia.EjectMedia.
func (s *Service) EjectMedia(c *gin.Context) {
	fwCtrl := s.Firmware
	if err := fwCtrl.EjectVirtualMedia(); err != nil {
		redfishErrorResponse(c, http.StatusInternalServerError, "eject media failed: "+err.Error())
		return
	}

	recordTransfer("", "", "")
	log.Info("redfish: virtual media ejected")
	c.Status(http.StatusNoContent)
}

func buildVirtualMediaResource(fwCtrl *firmware.Controller) VirtualMedia {
	vm := fwCtrl.GetVirtualMediaState()

	// ConnectedVia is a single Redfish enum string (NotConnected, URI,
	// Applet, Oem). Not an array — gofish unmarshal will reject [].
	connectedVia := schemas.NotConnectedConnectedVia
	var insertedMedia *InsertedMedia
	if vm.Inserted {
		connectedVia = schemas.URIConnectedVia
		insertedMedia = &InsertedMedia{
			ImageName:     vm.ImageName,
			CapacityBytes: vm.ImageSize,
		}
	}

	lastTransfer.Lock()
	method := lastTransfer.Method
	protocol := lastTransfer.ProtocolType
	image := lastTransfer.Image
	lastTransfer.Unlock()

	return VirtualMedia{
		Resource: Resource{
			ODataType:    "#VirtualMedia.v1_3_0.VirtualMedia",
			ODataID:      virtualMediaCDPath,
			ODataContext: context("VirtualMedia.VirtualMedia"),
			ID:           "CD",
			Name:         "Virtual Removable Media",
		},
		MediaTypes:           []schemas.VirtualMediaType{schemas.CDVirtualMediaType},
		MediaType:            schemas.CDVirtualMediaType,
		ConnectedVia:         connectedVia,
		Inserted:             vm.Inserted,
		WriteProtected:       true,
		InsertedMedia:        insertedMedia,
		Image:                image,
		TransferMethod:       schemas.TransferMethod(method),
		TransferProtocolType: schemas.VirtualMediaTransferProtocolType(protocol),
		Links: VirtualMediaLinks{
			Systems: Links{Link(systemPath)},
		},
		Actions: VirtualMediaActions{
			InsertMedia: ActionTarget{Target: virtualMediaCDPath + "/Actions/VirtualMedia.InsertMedia"},
			EjectMedia:  ActionTarget{Target: virtualMediaCDPath + "/Actions/VirtualMedia.EjectMedia"},
		},
	}
}
