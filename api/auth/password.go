package auth

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/auth"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

// ChangePassword decrypts the submitted password and delegates the
// credential change (account file + root shell password, with rollback)
// to the auth domain.
func (s *Service) ChangePassword(c *gin.Context) {
	var req proto.ChangePasswordReq
	var rsp proto.Response

	if err := proto.ParseFormRequest(c, &req); err != nil {
		rsp.ErrRsp(c, -1, "invalid parameters")
		return
	}

	password, err := utils.DecodeDecrypt(req.Password)
	if err != nil || password == "" {
		rsp.ErrRsp(c, -2, "invalid password")
		return
	}

	if err := auth.ChangePassword(req.Username, password); err != nil {
		rsp.ErrRsp(c, -5, "failed to change password")
		return
	}

	rsp.OkRsp(c)
	log.Debugf("change password success, username: %s", req.Username)
}

// IsPasswordUpdated reports whether the default admin password was changed.
func (s *Service) IsPasswordUpdated(c *gin.Context) {
	var rsp proto.Response

	updated, err := auth.IsPasswordUpdated()
	if err != nil {
		rsp.ErrRsp(c, -1, "failed to get password")
		return
	}

	rsp.OkRspWithData(c, &proto.IsPasswordUpdatedRsp{
		IsUpdated: updated,
	})
}
