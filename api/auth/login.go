package auth

import (
	"log/slog"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/ctxutil"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/middleware"
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
		// Anti-brute-force delay, ctx-aware so a client disconnect or server
		// shutdown unblocks this goroutine instead of pinning it for the
		// full duration.
		if err := ctxutil.SleepCtx(c.Request.Context(), 3*time.Second); err != nil {
			return
		}
		rsp.ErrRsp(c, code, msg)
		return
	}

	if err := proto.ParseFormRequest(c, h.log, &req); err != nil {
		if err := ctxutil.SleepCtx(c.Request.Context(), 3*time.Second); err != nil {
			return
		}
		rsp.ErrRsp(c, -1, "invalid parameters")
		return
	}

	if ok := h.d.Auth.CompareAccount(req.Username, req.Password); !ok {
		// Record the failure before the cancellable delay below: if a client
		// disconnects during the wait, the ctx.Done branch returns early, and
		// a failure recorded after that point would never be counted —
		// defeating the lockout.
		locked, code, msg := h.d.Auth.RecordLoginFailure(clientIP)

		// CompareAccount (pkg/auth/account.go) returns false uniformly for
		// "no such user" and "wrong password" — this single branch already
		// handles both identically, so waiting on ctx-cancellation here the
		// same way for every caller does not reopen a username-enumeration
		// timing gap.
		if err := ctxutil.SleepCtx(c.Request.Context(), 2*time.Second); err != nil {
			return
		}

		if locked {
			rsp.ErrRsp(c, code, msg)
			return
		}

		rsp.ErrRsp(c, -2, "invalid username or password")
		return
	}

	h.d.Auth.ClearLoginAttempt(clientIP)

	token, err := middleware.GenerateJWT(req.Username)
	if err != nil {
		if err := ctxutil.SleepCtx(c.Request.Context(), 1*time.Second); err != nil {
			return
		}
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
