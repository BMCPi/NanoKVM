package redfish

import "github.com/gin-gonic/gin"

type redfishErrorInfo struct {
	MessageID string `json:"MessageId"`
	Message   string `json:"Message"`
}

type redfishErrorBody struct {
	ExtendedInfo []redfishErrorInfo `json:"@Message.ExtendedInfo"`
	Code         string             `json:"code"`
	Message      string             `json:"message"`
}

type redfishError struct {
	Error redfishErrorBody `json:"error"`
}

func redfishErrorResponse(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, redfishError{
		Error: redfishErrorBody{
			Code:    "Base.1.0.GeneralError",
			Message: message,
			ExtendedInfo: []redfishErrorInfo{
				{
					MessageID: "Base.1.0.GeneralError",
					Message:   message,
				},
			},
		},
	})
}
