package proto

import (
	"log/slog"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

var env = os.Getenv(gin.EnvGinMode)

// ValidateRequest Validates request parameters.
func ValidateRequest(req any) error {
	validate := validator.New()

	if err := validate.Struct(req); err != nil {
		slog.Error("validate request failed", slog.Any("err", err))
		return err
	}

	if env == "" || env == "debug" {
		slog.Debug("request", slog.Any("req", req))
	}

	return nil
}

// ParseQueryRequest Validates GET requests.
func ParseQueryRequest(c *gin.Context, req any) error {
	var err error
	if err = c.ShouldBindQuery(req); err != nil {
		slog.ErrorContext(c.Request.Context(), "parse request failed", slog.Any("err", err))
		return err
	}

	return ValidateRequest(req)
}

// ParseFormRequest Validates POST Requests.
func ParseFormRequest(c *gin.Context, req any) error {
	var err error
	if err = c.ShouldBind(req); err != nil {
		slog.ErrorContext(c.Request.Context(), "parse request failed", slog.Any("err", err))
		return err
	}

	return ValidateRequest(req)
}
