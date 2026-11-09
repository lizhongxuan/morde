package api

import (
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type personalOfficePreviewResponse struct {
	Text string `json:"text"`
}

// RegisterPersonalOfficePreviewRoute adds a user-triggered, in-memory Office
// text preview endpoint under /api/personal-data.
func RegisterPersonalOfficePreviewRoute(group *gin.RouterGroup) {
	group.POST("/office-preview", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !optionalMessageActor(c, principal) {
			return
		}

		mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || len(params) != 0 || !isPersonalOfficePreviewMediaType(mediaType) {
			writeError(c, domain.InvalidArgument("content_type", "Office preview content type is not supported"))
			return
		}
		if c.Request.ContentLength > int64(app.PersonalOfficePreviewMaxBytes) {
			writeError(c, domain.NewError(domain.CodeRequestTooLarge, "Office document exceeds the preview limit"))
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, int64(app.PersonalOfficePreviewMaxBytes))
		content, err := io.ReadAll(c.Request.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(c, domain.NewError(domain.CodeRequestTooLarge, "Office document exceeds the preview limit"))
				return
			}
			writeError(c, domain.InvalidArgument("document", "Office document could not be read"))
			return
		}
		text, err := app.PreviewPersonalOfficeText(mediaType, content)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("Cache-Control", "private, no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.JSON(http.StatusOK, personalOfficePreviewResponse{Text: text})
	})
}

func isPersonalOfficePreviewMediaType(mediaType string) bool {
	switch mediaType {
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return true
	default:
		return false
	}
}
