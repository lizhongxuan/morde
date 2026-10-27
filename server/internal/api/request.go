package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func decodeJSON(c *gin.Context, target any) bool {
	return decodeJSONWithOptions(c, target, false)
}

func decodeStrictJSON(c *gin.Context, target any) bool {
	return decodeJSONWithOptions(c, target, true)
}

func decodeJSONWithOptions(c *gin.Context, target any, strict bool) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(c, domain.InvalidArgument("body", "content type must be application/json"))
		return false
	}
	decoder := json.NewDecoder(c.Request.Body)
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		writeDecodeError(c, err)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		writeDecodeError(c, err)
		return false
	}
	return true
}

func writeDecodeError(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(c, domain.NewError(domain.CodeRequestTooLarge, "request body too large"))
		return
	}
	writeError(c, domain.InvalidArgument("body", "invalid json request"))
}

func requestPage(c *gin.Context) (app.PageRequest, bool) {
	limitValues, exists := c.Request.URL.Query()["limit"]
	limit := int64(50)
	if exists {
		if len(limitValues) != 1 {
			writeError(c, domain.InvalidArgument("limit", "page limit is invalid"))
			return app.PageRequest{}, false
		}
		parsed, err := strconv.ParseInt(limitValues[0], 10, 32)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(c, domain.InvalidArgument("limit", "page limit is invalid"))
			return app.PageRequest{}, false
		}
		limit = parsed
	}
	cursor, ok := singleQuery(c, "cursor")
	if !ok {
		return app.PageRequest{}, false
	}
	if len(cursor) > 1024 {
		writeError(c, domain.InvalidArgument("cursor", "page cursor is invalid"))
		return app.PageRequest{}, false
	}
	return app.PageRequest{Limit: int32(limit), Cursor: cursor}, true
}

func singleQuery(c *gin.Context, key string) (string, bool) {
	values, exists := c.Request.URL.Query()[key]
	if !exists {
		return "", true
	}
	if len(values) != 1 {
		writeError(c, domain.InvalidArgument(key, "query parameter must appear once"))
		return "", false
	}
	return values[0], true
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func cursorPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
