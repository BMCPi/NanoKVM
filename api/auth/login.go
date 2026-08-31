package auth

import (
	"log/slog"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/middleware"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"

	"github.com/gin-gonic/gin"
)

func (h *handlers) Login(c *gin.Context) {
	var req proto.LoginReq
	var rsp proto.Response

	// authentication disabled
	conf := config.GetInstance()
	if conf.Authentication == "disable" {
		rsp.OkRspWithData(c, &proto.LoginRsp{
			Token: "disabled",
		})
		return
	}

	clientIP := requestIP(c)
	if locked, code, msg := h.d.Auth.CheckLoginAttempt(clientIP); locked {
		time.Sleep(3 * time.Second)
		rsp.ErrRsp(c, code, msg)
		return
	}

	if err := proto.ParseFormRequest(c, &req); err != nil {
		time.Sleep(3 * time.Second)
		rsp.ErrRsp(c, -1, "invalid parameters")
		return
	}

	if ok := h.d.Auth.CompareAccount(req.Username, req.Password); !ok {
		time.Sleep(2 * time.Second)

		if locked, code, msg := h.d.Auth.RecordLoginFailure(clientIP); locked {
			rsp.ErrRsp(c, code, msg)
			return
		}

		rsp.ErrRsp(c, -2, "invalid username or password")
		return
	}

	h.d.Auth.ClearLoginAttempt(clientIP)

	token, err := middleware.GenerateJWT(req.Username)
	if err != nil {
		time.Sleep(1 * time.Second)
		rsp.ErrRsp(c, -3, "generate token failed")
		return
	}

	rsp.OkRspWithData(c, &proto.LoginRsp{
		Token: token,
	})

	h.log.DebugContext(c.Request.Context(), "login success", slog.String("username", req.Username))
}

func (h *handlers) Logout(c *gin.Context) {
	conf := config.GetInstance()

	if conf.JWT.RevokeTokensOnLogout {
		config.RegenerateSecretKey()
	}

	var rsp proto.Response
	rsp.OkRsp(c)
}

func (h *handlers) GetAccount(c *gin.Context) {
	var rsp proto.Response

	account, err := h.d.Auth.GetAccount()
	if err != nil {
		rsp.ErrRsp(c, -1, "get account failed")
		return
	}

	rsp.OkRspWithData(c, &proto.GetAccountRsp{
		Username: account.Username,
	})
	h.log.DebugContext(c.Request.Context(), "get account successful")
}

// requestIP gets a reliable real IP for brute-force accounting.
func requestIP(c *gin.Context) string {
	ip := c.RemoteIP()
	if ip == "" {
		ip = c.ClientIP()
	}
	return ip
}
