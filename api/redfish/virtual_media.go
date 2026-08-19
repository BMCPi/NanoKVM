package redfish

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

// maxMediaUploadBytes caps a multipart image push. The image is streamed
// straight to the media directory on the data partition (constant memory), so
// this bounds what a client can force onto that partition, not RAM.
const maxMediaUploadBytes = 8 << 30 // 8 GiB

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

	// Download the ISO into the media staging directory. utils.FetchURL caps
	// the transfer and bounds the connection: the remote, not the operator,
	// decides how many bytes this BMC is asked to store.
	remote, err := utils.FetchURL(req.Image, maxMediaUploadBytes)
	if err != nil {
		redfishErrorResponse(c, http.StatusBadGateway, "fetch failed: "+err.Error())
		return
	}
	defer remote.Close()

	if err := stageAndInsert(s.Firmware, name, remote); err != nil {
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
//
// The body is walked part-by-part and the image is streamed straight to the
// media directory. ParseMultipartForm/FormFile would spool the whole image
// into os.TempDir() first, which on this device is the RAM-backed overlay —
// a large ISO took the server down mid-upload. See
// pkg/utils/multipart_stream.go.
func (s *Service) insertMediaUpload(c *gin.Context) {
	// Accept the file under any of the conventional Redfish part names:
	// Image is the spec'd name, redfishtool uses "file", and some tools use
	// the resource name VirtualMediaImage.
	upload, err := utils.StreamMultipartFile(c.Request, maxMediaUploadBytes,
		"Image", "file", "VirtualMediaImage")
	if err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, err.Error())
		return
	}
	defer upload.Close()

	// InsertMediaRequestBody may precede or follow the image part. Only the
	// leading one can be honoured before the file lands, so parse what we
	// have now and reconcile a trailing copy once the image is staged.
	meta, rerr := parseInsertMediaMeta(upload.Values)
	if rerr != nil {
		redfishErrorResponse(c, rerr.status, rerr.msg)
		return
	}

	name := mediaFilename(meta.Image, upload.Filename)

	n, err := s.Firmware.SaveMediaFile(name, upload)
	if err != nil {
		redfishErrorResponse(c, http.StatusInternalServerError, "save media failed: "+err.Error())
		return
	}

	// Now that the wire is drained, a trailing InsertMediaRequestBody can
	// still rename what we staged — but never re-validate TransferMethod
	// into a failure here: the bytes are already on disk.
	if trailing, err := parseInsertMediaMeta(upload.Rest()); err == nil && trailing.Image != "" {
		if want := mediaFilename(trailing.Image, upload.Filename); want != name {
			if renamed, err := renameStagedMedia(s.Firmware, name, want); err != nil {
				log.Warnf("redfish: keeping staged name %q: %v", name, err)
			} else {
				name = renamed
			}
		}
	}

	if err := s.Firmware.InsertVirtualMedia(name); err != nil {
		redfishErrorResponse(c, http.StatusConflict, "insert media failed: "+err.Error())
		return
	}

	recordTransfer("Upload", "", name)
	log.Infof("redfish: virtual media inserted (upload): %s (%d bytes)", name, n)
	c.JSON(http.StatusOK, buildVirtualMediaResource(s.Firmware))
}

// parseInsertMediaMeta decodes the optional InsertMediaRequestBody part and
// rejects a TransferMethod that contradicts the multipart push.
func parseInsertMediaMeta(values map[string]string) (insertMediaRequest, *InsertError) {
	var meta insertMediaRequest
	v := values["InsertMediaRequestBody"]
	if v == "" {
		return meta, nil
	}
	if err := json.Unmarshal([]byte(v), &meta); err != nil {
		return meta, &InsertError{http.StatusBadRequest, "InsertMediaRequestBody: " + err.Error()}
	}
	if meta.TransferMethod != "" && !strings.EqualFold(meta.TransferMethod, "Upload") {
		msg := "TransferMethod=" + meta.TransferMethod + " not valid for multipart upload"
		return meta, &InsertError{http.StatusBadRequest, msg}
	}
	return meta, nil
}

// mediaFilename picks the staging name: the client's explicit Image wins over
// the part's filename, and a name that is only path separators falls back to
// vm.iso rather than escaping the media directory.
func mediaFilename(preferred, fallback string) string {
	name := preferred
	if name == "" {
		name = fallback
	}
	name = filepath.Base(name)
	if name == "" || name == "." || name == "/" {
		return "vm.iso"
	}
	return name
}

// renameStagedMedia moves an already-staged file to the name a trailing
// InsertMediaRequestBody asked for, returning the name actually in effect.
func renameStagedMedia(fwCtrl *firmware.Controller, from, to string) (string, error) {
	dir := fwCtrl.GetMediaDir()
	if dir == "" {
		return from, fmt.Errorf("mediaDir not configured")
	}
	if err := os.Rename(filepath.Join(dir, from), filepath.Join(dir, to)); err != nil {
		return from, err
	}
	return to, nil
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
