package vm

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
	sshsvc "github.com/pi-bmc/nanokvm-app/pkg/ssh"
)

// SSH state is config, not a flag file: the BMC ships no sshd, so there is no
// init script to poke. The listener lives in-process (pkg/ssh) and
// the toggle starts or stops it, persisting the choice to /etc/kvm/server.yaml
// (a bind mount onto the data partition) so it survives a reboot.

func (h *handlers) GetSSHState(c *gin.Context) {
	var rsp proto.Response

	rsp.OkRspWithData(c, &proto.GetSSHStateRsp{
		Enabled: config.GetInstance().SSH.Enabled,
	})
}

func (h *handlers) EnableSSH(c *gin.Context) {
	h.setSSHEnabled(c, true)
}

func (h *handlers) DisableSSH(c *gin.Context) {
	h.setSSHEnabled(c, false)
}

func (h *handlers) setSSHEnabled(c *gin.Context, enabled bool) {
	var rsp proto.Response

	conf := config.GetInstance()
	previous := conf.SSH.Enabled
	conf.SSH.Enabled = enabled

	if err := sshsvc.Restart(); err != nil {
		// Roll back so the reported state matches reality — a port already in
		// use must not leave the UI showing SSH as enabled.
		conf.SSH.Enabled = previous
		h.log.ErrorContext(c.Request.Context(), "failed to apply SSH state", slog.Any("err", err))
		rsp.ErrRsp(c, -1, "operation failed")
		return
	}

	if previous != enabled {
		config.Save()
	}

	rsp.OkRsp(c)
	if enabled {
		h.log.InfoContext(c.Request.Context(), "SSH enabled")
	} else {
		h.log.InfoContext(c.Request.Context(), "SSH disabled")
	}
}

// GetSSHKeys returns the configured authorized_keys content.
func (h *handlers) GetSSHKeys(c *gin.Context) {
	var rsp proto.Response

	keys, err := sshsvc.ReadAuthorizedKeys()
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "failed to read authorized keys", slog.Any("err", err))
		rsp.ErrRsp(c, -1, "operation failed")
		return
	}

	rsp.OkRspWithData(c, &proto.GetSSHKeysRsp{SSHKey: keys})
}

// SetSSHKeys replaces authorized_keys. An empty value clears the file, leaving
// password auth (when enabled) as the only way in. Content is validated before
// anything is written, so a malformed paste cannot silently break key auth.
func (h *handlers) SetSSHKeys(c *gin.Context) {
	var (
		req proto.SetSSHKeysReq
		rsp proto.Response
	)

	if err := proto.ParseFormRequest(c, &req); err != nil {
		rsp.ErrRsp(c, -1, "invalid parameters")
		return
	}

	if err := sshsvc.WriteAuthorizedKeys(req.SSHKey); err != nil {
		h.log.ErrorContext(c.Request.Context(), "failed to write authorized keys", slog.Any("err", err))
		rsp.ErrRsp(c, -1, err.Error())
		return
	}

	rsp.OkRsp(c)
}
