package proto

import (
	"log/slog"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

var env = os.Getenv(gin.EnvGinMode)

// ValidateRequest Validates request parameters.
func ValidateRequest(log *slog.Logger, req any) error {
	validate := validator.New()

	if err := validate.Struct(req); err != nil {
		log.Error("validate request failed", slog.Any("err", err))
		return err
	}

	if env == "" || env == "debug" {
		log.Debug("request", slog.Any("req", req))
	}

	return nil
}

// ParseQueryRequest Validates GET requests.
func ParseQueryRequest(c *gin.Context, log *slog.Logger, req any) error {
	var err error
	if err = c.ShouldBindQuery(req); err != nil {
		log.ErrorContext(c.Request.Context(), "parse request failed", slog.Any("err", err))
		return err
	}

	return ValidateRequest(log, req)
}

// ParseFormRequest Validates POST Requests.
func ParseFormRequest(c *gin.Context, log *slog.Logger, req any) error {
	var err error
	if err = c.ShouldBind(req); err != nil {
		log.ErrorContext(c.Request.Context(), "parse request failed", slog.Any("err", err))
		return err
	}

	return ValidateRequest(log, req)
}
