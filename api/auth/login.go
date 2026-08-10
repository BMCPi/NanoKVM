package auth

import (
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/auth"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/middleware"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func (s *Service) Login(c *gin.Context) {
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
	if locked, code, msg := auth.CheckLoginAttempt(clientIP); locked {
		time.Sleep(3 * time.Second)
		rsp.ErrRsp(c, code, msg)
		return
	}

	if err := proto.ParseFormRequest(c, &req); err != nil {
		time.Sleep(3 * time.Second)
		rsp.ErrRsp(c, -1, "invalid parameters")
		return
	}

	if ok := auth.CompareAccount(req.Username, req.Password); !ok {
		time.Sleep(2 * time.Second)

		if locked, code, msg := auth.RecordLoginFailure(clientIP); locked {
			rsp.ErrRsp(c, code, msg)
			return
		}

		rsp.ErrRsp(c, -2, "invalid username or password")
		return
	}

	auth.ClearLoginAttempt(clientIP)

	token, err := middleware.GenerateJWT(req.Username)
	if err != nil {
		time.Sleep(1 * time.Second)
		rsp.ErrRsp(c, -3, "generate token failed")
		return
	}

	rsp.OkRspWithData(c, &proto.LoginRsp{
		Token: token,
	})

	log.Debugf("login success, username: %s", req.Username)
}

func (s *Service) Logout(c *gin.Context) {
	conf := config.GetInstance()

	if conf.JWT.RevokeTokensOnLogout {
		config.RegenerateSecretKey()
	}

	var rsp proto.Response
	rsp.OkRsp(c)
}

func (s *Service) GetAccount(c *gin.Context) {
	var rsp proto.Response

	account, err := auth.GetAccount()
	if err != nil {
		rsp.ErrRsp(c, -1, "get account failed")
		return
	}

	rsp.OkRspWithData(c, &proto.GetAccountRsp{
		Username: account.Username,
	})
	log.Debugf("get account successful")
}

// requestIP gets a reliable real IP for brute-force accounting.
func requestIP(c *gin.Context) string {
	ip := c.RemoteIP()
	if ip == "" {
		ip = c.ClientIP()
	}
	return ip
}
