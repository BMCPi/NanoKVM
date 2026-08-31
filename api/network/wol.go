package network

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/proto"
)

const (
	WolMacFile = "/etc/kvm/cache/wol"
)

func (h *handlers) WakeOnLAN(c *gin.Context) {
	var req proto.WakeOnLANReq
	var rsp proto.Response

	if err := proto.ParseFormRequest(c, &req); err != nil {
		rsp.ErrRsp(c, -1, "invalid arguments")
		return
	}

	mac, err := parseMAC(req.Mac)
	if err != nil {
		rsp.ErrRsp(c, -2, "invalid MAC address")
		return
	}

	command := fmt.Sprintf("ether-wake -b %s", mac)
	// context.Background(), not c.Request.Context(): the magic packet send
	// must complete even if the client disconnects right after issuing the
	// request, so the command's lifetime is intentionally not tied to it.
	// This repo's idiom for a detached side effect is deps.ActionContext
	// (see api/vm/service.go's Deps field doc and api/vm/gpio.go's power
	// handlers), deliberately not adopted here: this package's Service
	// carries no *deps.Deps today, and wiring one in just to get a
	// shutdown-cancellable context would be a behaviour change out of scope
	// for a lint-only pass.
	cmd := exec.CommandContext(context.Background(), "sh", "-c", command)

	output, err := cmd.CombinedOutput()
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "failed to wake on lan", slog.Any("err", err))
		rsp.ErrRsp(c, -3, string(output))
		return
	}

	go h.saveMac(mac)

	rsp.OkRsp(c)
	h.log.DebugContext(c.Request.Context(), "wake on lan", slog.String("mac", mac))
}

func (h *handlers) GetMac(c *gin.Context) {
	var rsp proto.Response

	content, err := os.ReadFile(WolMacFile)
	if err != nil {
		rsp.ErrRsp(c, -2, "open file error")
		return
	}

	data := &proto.GetMacRsp{
		Macs: strings.Split(string(content), "\n"),
	}

	rsp.OkRspWithData(c, data)
}

func (h *handlers) SetMacName(c *gin.Context) {
	var req proto.SetMacNameReq // Mac:string Name:string
	var rsp proto.Response

	if err := proto.ParseFormRequest(c, &req); err != nil {
		rsp.ErrRsp(c, -1, "invalid arguments")
		return
	}

	content, err := os.ReadFile(WolMacFile)
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "failed to open file", slog.String("path", WolMacFile), slog.Any("err", err))
		rsp.ErrRsp(c, -2, "read failed")
		return
	}

	macs := strings.Split(string(content), "\n")
	var newLines []string
	macFound := false

	for _, line := range macs {
		parts := strings.Split(line, " ")
		if req.Mac != parts[0] {
			newLines = append(newLines, line)
			continue
		}
		newLines = append(newLines, parts[0]+" "+req.Name)
		macFound = true
	}

	if !macFound {
		h.log.ErrorContext(c.Request.Context(), "failed to found mac", slog.String("mac", req.Mac), slog.Any("err", err))
		rsp.ErrRsp(c, -3, "write failed")
		return
	}

	data := strings.Join(newLines, "\n")
	err = os.WriteFile(WolMacFile, []byte(data), 0o600) //nolint:gosec // G703: destination is the hardcoded WolMacFile constant, never attacker-influenced; only the persisted MAC/name content is request-supplied, which is the intended feature
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "failed to write file", slog.String("path", WolMacFile), slog.Any("err", err))
		rsp.ErrRsp(c, -3, "write failed")
		return
	}

	rsp.OkRsp(c)
	h.log.DebugContext(c.Request.Context(), "set wol mac name", slog.String("mac", req.Mac), slog.String("name", req.Name))
}

func (h *handlers) DeleteMac(c *gin.Context) {
	var req proto.DeleteMacReq
	var rsp proto.Response

	if err := proto.ParseFormRequest(c, &req); err != nil {
		rsp.ErrRsp(c, -1, "invalid arguments")
		return
	}

	content, err := os.ReadFile(WolMacFile)
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "failed to open file", slog.String("path", WolMacFile), slog.Any("err", err))
		rsp.ErrRsp(c, -2, "read failed")
		return
	}

	macs := strings.Split(string(content), "\n")
	var newMacs []string

	for _, mac := range macs {
		parts := strings.Split(mac, " ")
		if req.Mac != parts[0] {
			newMacs = append(newMacs, mac)
		}
	}

	data := strings.Join(newMacs, "\n")
	err = os.WriteFile(WolMacFile, []byte(data), 0o600) //nolint:gosec // G703: destination is the hardcoded WolMacFile constant, never attacker-influenced; only the persisted MAC/name content is request-supplied, which is the intended feature
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "failed to write file", slog.String("path", WolMacFile), slog.Any("err", err))
		rsp.ErrRsp(c, -3, "write failed")
		return
	}

	rsp.OkRsp(c)
	h.log.DebugContext(c.Request.Context(), "delete wol mac", slog.String("mac", req.Mac))
}

func parseMAC(mac string) (string, error) {
	mac = strings.ToUpper(strings.TrimSpace(mac))

	mac = strings.ReplaceAll(mac, "-", "")
	mac = strings.ReplaceAll(mac, ":", "")
	mac = strings.ReplaceAll(mac, ".", "")

	matched, err := regexp.MatchString("^[0-9A-F]{12}$", mac)
	if err != nil {
		return "", err
	}
	if !matched {
		return "", fmt.Errorf("invalid MAC address: %s", mac)
	}

	var result strings.Builder
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			result.WriteString(":")
		}
		result.WriteString(mac[i : i+2])
	}

	return result.String(), nil
}

func (h *handlers) saveMac(mac string) {
	if isMacExist(mac) {
		return
	}

	// 0o700/0o600: this is the one path that actually creates WolMacFile.
	// os.WriteFile's mode argument elsewhere in this file only applies at
	// creation time, and every other write path here reads the file first
	// and bails on error, so this OpenFile is the sole creator -- narrowing
	// only the WriteFile calls without narrowing this would leave the file
	// permanently at 0o644 regardless of what those calls ask for.
	err := os.MkdirAll(filepath.Dir(WolMacFile), 0o700)
	if err != nil {
		h.log.Error("failed to create dir", slog.Any("err", err))
		return
	}

	file, err := os.OpenFile(WolMacFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		h.log.Error("failed to open file", slog.String("path", WolMacFile), slog.Any("err", err))
		return
	}
	defer func() {
		_ = file.Close()
	}()

	content := fmt.Sprintf("%s\n", mac)
	_, err = file.WriteString(content)
	if err != nil {
		h.log.Error("failed to write file", slog.String("path", WolMacFile), slog.Any("err", err))
		return
	}
}

func isMacExist(mac string) bool {
	content, err := os.ReadFile(WolMacFile)
	if err != nil {
		return false
	}

	macs := strings.Split(string(content), "\n")
	for _, item := range macs {
		parts := strings.Split(item, " ")
		if mac == parts[0] {
			return true
		}
	}

	return false
}
