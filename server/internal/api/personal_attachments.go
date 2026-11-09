package api

import (
	"context"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

const (
	personalAttachmentUploadMaxBodyBytes int64 = 16 << 10
	personalAttachmentChunkDigestHeader        = "X-Chunk-SHA256"
)

type personalAttachmentService interface {
	CreatePersonalAttachmentUpload(context.Context, identity.Principal, app.PersonalAttachmentUploadCommand) (personal.AttachmentUploadReceipt, error)
	PutPersonalAttachmentChunk(context.Context, identity.Principal, app.PersonalAttachmentChunkCommand) (personal.AttachmentChunkReceipt, error)
	CommitPersonalAttachment(context.Context, identity.Principal, string, string) (personal.AttachmentCommitReceipt, error)
	CancelPersonalAttachmentUpload(context.Context, identity.Principal, string) error
	OpenPersonalAttachment(context.Context, identity.Principal, string) (io.ReadCloser, personal.AttachmentMetadata, error)
}

type personalAttachmentUploadRequest struct {
	ObjectID     string `json:"object_id"`
	BaseRevision *int64 `json:"base_revision"`
	SizeBytes    *int64 `json:"size_bytes"`
	SHA256       string `json:"sha256"`
	MediaType    string `json:"media_type"`
}

type personalAttachmentUploadResponse struct {
	UploadID          string  `json:"upload_id"`
	State             string  `json:"state"`
	ChunkCount        int32   `json:"chunk_count"`
	ReceivedSequences []int32 `json:"received_sequences"`
	Replay            bool    `json:"replay"`
}

type personalAttachmentChunkResponse struct {
	UploadID string `json:"upload_id"`
	Sequence int32  `json:"sequence"`
	Replay   bool   `json:"replay"`
}

type personalAttachmentCommitResponse struct {
	AttachmentID          string `json:"attachment_id"`
	ObjectType            string `json:"object_type"`
	ObjectID              string `json:"object_id"`
	ResultRevision        int64  `json:"result_revision"`
	AccountChangeSequence int64  `json:"account_change_sequence"`
	Replay                bool   `json:"replay"`
}

// RegisterPersonalAttachmentRoutes installs owner-scoped, chunk-bounded
// endpoints under /api/personal-data.
func RegisterPersonalAttachmentRoutes(group *gin.RouterGroup, service personalAttachmentService) {
	group.POST("/uploads", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !optionalMessageActor(c, principal) {
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, personalAttachmentUploadMaxBodyBytes)
		var request personalAttachmentUploadRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.BaseRevision == nil {
			writeError(c, domain.InvalidArgument("base_revision", "personal attachment upload request is invalid"))
			return
		}
		if request.SizeBytes == nil {
			writeError(c, domain.InvalidArgument("size_bytes", "personal attachment upload request is invalid"))
			return
		}
		idempotencyKey, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		receipt, err := service.CreatePersonalAttachmentUpload(c.Request.Context(), principal, app.PersonalAttachmentUploadCommand{
			IdempotencyKey: idempotencyKey, ObjectID: request.ObjectID, BaseRevision: *request.BaseRevision,
			SizeBytes: *request.SizeBytes, SHA256: request.SHA256, MediaType: request.MediaType,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		if receipt.ReceivedSequences == nil {
			receipt.ReceivedSequences = []int32{}
		}
		c.JSON(http.StatusOK, personalAttachmentUploadResponse{
			UploadID: receipt.UploadID, State: string(receipt.State), ChunkCount: receipt.ChunkCount,
			ReceivedSequences: receipt.ReceivedSequences, Replay: receipt.Replay,
		})
	})

	group.PUT("/uploads/:upload_id/chunks/:sequence", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !optionalMessageActor(c, principal) {
			return
		}
		sequenceValue, err := strconv.ParseInt(c.Param("sequence"), 10, 32)
		if err != nil || sequenceValue < 0 {
			writeError(c, domain.InvalidArgument("sequence", "personal attachment chunk sequence is invalid"))
			return
		}
		if c.Request.ContentLength < 1 || c.Request.ContentLength > personal.MaxAttachmentChunkBytes {
			if c.Request.ContentLength > personal.MaxAttachmentChunkBytes {
				writeError(c, domain.NewError(domain.CodeRequestTooLarge, "personal attachment chunk exceeds the size limit"))
			} else {
				writeError(c, domain.InvalidArgument("Content-Length", "personal attachment chunk length is required"))
			}
			return
		}
		digests := c.Request.Header.Values(personalAttachmentChunkDigestHeader)
		if len(digests) != 1 || digests[0] == "" {
			writeError(c, domain.InvalidArgument(personalAttachmentChunkDigestHeader, "exactly one chunk digest is required"))
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, personal.MaxAttachmentChunkBytes)
		receipt, err := service.PutPersonalAttachmentChunk(c.Request.Context(), principal, app.PersonalAttachmentChunkCommand{
			UploadID: c.Param("upload_id"), Sequence: int32(sequenceValue), SizeBytes: c.Request.ContentLength,
			SHA256: digests[0], Body: c.Request.Body,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, personalAttachmentChunkResponse{UploadID: receipt.UploadID, Sequence: receipt.Sequence, Replay: receipt.Replay})
	})

	group.POST("/uploads/:upload_id/commit", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !optionalMessageActor(c, principal) {
			return
		}
		if !personalAttachmentBodyIsEmpty(c) {
			return
		}
		idempotencyKey, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		receipt, err := service.CommitPersonalAttachment(c.Request.Context(), principal, c.Param("upload_id"), idempotencyKey)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, personalAttachmentCommitResponse{
			AttachmentID: receipt.AttachmentID, ObjectType: receipt.ObjectType, ObjectID: receipt.ObjectID,
			ResultRevision: receipt.ResultRevision, AccountChangeSequence: receipt.AccountChangeSequence, Replay: receipt.Replay,
		})
	})

	group.DELETE("/uploads/:upload_id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !optionalMessageActor(c, principal) {
			return
		}
		if !personalAttachmentBodyIsEmpty(c) {
			return
		}
		if err := service.CancelPersonalAttachmentUpload(c.Request.Context(), principal, c.Param("upload_id")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	group.GET("/attachments/:attachment_id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !personalAttachmentBodyIsEmpty(c) {
			return
		}
		reader, metadata, err := service.OpenPersonalAttachment(c.Request.Context(), principal, c.Param("attachment_id"))
		if err != nil {
			writeError(c, err)
			return
		}
		defer func() { _ = reader.Close() }()
		c.Header("Content-Type", metadata.MediaType)
		c.Header("Content-Length", strconv.FormatInt(metadata.SizeBytes, 10))
		c.Header("Cache-Control", "private, no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Disposition", "attachment")
		c.Header("ETag", `"`+metadata.SHA256+`"`)
		c.Status(http.StatusOK)
		if _, err := io.Copy(c.Writer, reader); err != nil {
			_ = c.Error(err)
			c.Abort()
		}
	})
}

func personalAttachmentBodyIsEmpty(c *gin.Context) bool {
	if c.Request.ContentLength > 0 {
		writeError(c, domain.InvalidArgument("body", "request body must be empty"))
		return false
	}
	if c.Request.ContentLength == 0 {
		return true
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1)
	var body [1]byte
	n, err := c.Request.Body.Read(body[:])
	if n > 0 {
		writeError(c, domain.InvalidArgument("body", "request body must be empty"))
		return false
	}
	if err != io.EOF {
		writeError(c, domain.InvalidArgument("body", "request body must be empty"))
		return false
	}
	return true
}
