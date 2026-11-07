package api

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

const projectConversationAttachmentBase = "/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id"
const projectConversationAttachmentReadBase = "/projects/:id/conversations/:conversation_id/messages/:message_id/attachments-by-id/:attachment_id"

// Keep slow downloads from exhausting the pool or monopolizing it by one actor.
type projectAttachmentReadLimiter struct {
	mu      sync.Mutex
	active  int
	byActor map[string]int
}

func (l *projectAttachmentReadLimiter) acquire(actor string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= 32 || l.byActor[actor] >= 4 {
		return nil, false
	}
	l.active++
	l.byActor[actor]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.active--
			l.byActor[actor]--
			if l.byActor[actor] == 0 {
				delete(l.byActor, actor)
			}
		})
	}, true
}

type projectConversationAttachmentService interface {
	Initialize(context.Context, identity.Principal, app.ProjectConversationAttachmentScope, app.ProjectConversationAttachmentMetadata) (app.ProjectConversationAttachmentStatus, error)
	Get(context.Context, identity.Principal, app.ProjectConversationAttachmentScope) (app.ProjectConversationAttachmentStatus, error)
	PutChunk(context.Context, identity.Principal, app.ProjectConversationAttachmentScope, int32, string, io.Reader, int64) (bool, error)
	Commit(context.Context, identity.Principal, app.ProjectConversationAttachmentScope) (app.ProjectConversationAttachmentStatus, error)
	Open(context.Context, identity.Principal, app.ProjectConversationAttachmentScope) (io.ReadCloser, app.ProjectConversationAttachmentMetadata, error)
	GetByID(context.Context, identity.Principal, app.ProjectConversationAttachmentReference) (app.ProjectConversationAttachmentMetadata, error)
	OpenByID(context.Context, identity.Principal, app.ProjectConversationAttachmentReference) (io.ReadCloser, app.ProjectConversationAttachmentMetadata, error)
}

type projectConversationDerivativeService interface {
	PutDerivatives(context.Context, identity.Principal, app.ProjectConversationAttachmentScope, app.ProjectConversationDerivatives) (app.ProjectConversationAttachmentStatus, error)
}

func projectAttachmentRequestScope(c *gin.Context) (identity.Principal, app.ProjectConversationAttachmentScope, bool) {
	principal, projectID, conversationID, ok := projectConversationPrincipalAndConversationID(c)
	if !ok || !requiredMessageActor(c, principal) {
		return identity.Principal{}, app.ProjectConversationAttachmentScope{}, false
	}
	instanceID, ok := requiredMessageServiceInstance(c)
	if !ok {
		return identity.Principal{}, app.ProjectConversationAttachmentScope{}, false
	}
	messageID, ok := safeIDParam(c, "message_id", "message_id", domain.ValidMessageID)
	if !ok {
		return identity.Principal{}, app.ProjectConversationAttachmentScope{}, false
	}
	return principal, app.ProjectConversationAttachmentScope{ProjectID: projectID, ConversationID: conversationID,
		MessageID: messageID, ClientAttachmentID: c.Param("client_attachment_id"), ServiceInstanceID: instanceID}, true
}

func projectAttachmentRequestReference(c *gin.Context) (identity.Principal, app.ProjectConversationAttachmentReference, bool) {
	principal, projectID, conversationID, ok := projectConversationPrincipalAndConversationID(c)
	if !ok || !requiredMessageActor(c, principal) {
		return identity.Principal{}, app.ProjectConversationAttachmentReference{}, false
	}
	instanceID, ok := requiredMessageServiceInstance(c)
	if !ok {
		return identity.Principal{}, app.ProjectConversationAttachmentReference{}, false
	}
	messageID, ok := safeIDParam(c, "message_id", "message_id", domain.ValidMessageID)
	if !ok {
		return identity.Principal{}, app.ProjectConversationAttachmentReference{}, false
	}
	attachmentID, ok := safeIDParam(c, "attachment_id", "attachment_id", app.ValidProjectConversationAttachmentID)
	if !ok {
		return identity.Principal{}, app.ProjectConversationAttachmentReference{}, false
	}
	return principal, app.ProjectConversationAttachmentReference{ProjectID: projectID, ConversationID: conversationID,
		MessageID: messageID, AttachmentID: attachmentID, ServiceInstanceID: instanceID}, true
}

// RegisterProjectConversationAttachmentRoutes mounts independently scoped
// project files; it does not route them through the personal library API.
func RegisterProjectConversationAttachmentRoutes(group *gin.RouterGroup, service projectConversationAttachmentService) {
	readLimiter := &projectAttachmentReadLimiter{byActor: make(map[string]int)}
	derivativeLimiter := &projectAttachmentReadLimiter{byActor: make(map[string]int)}
	group = group.Group("")
	group.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "private, no-store")
		if service == nil {
			serviceUnavailable(c)
			c.Abort()
			return
		}
		c.Next()
	})
	group.PUT(projectConversationAttachmentBase, func(c *gin.Context) {
		principal, scope, ok := projectAttachmentRequestScope(c)
		if !ok {
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, personalAttachmentUploadMaxBodyBytes)
		var request struct {
			Name           string `json:"name"`
			MediaType      string `json:"media_type"`
			SizeBytes      *int64 `json:"size_bytes"`
			SHA256         string `json:"sha256"`
			DerivativeMode string `json:"derivative_mode"`
		}
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.SizeBytes == nil {
			writeError(c, domain.InvalidArgument("size_bytes", "project attachment size is required"))
			return
		}
		result, err := service.Initialize(c.Request.Context(), principal, scope, app.ProjectConversationAttachmentMetadata{
			Name: request.Name, MediaType: request.MediaType, SizeBytes: *request.SizeBytes, SHA256: request.SHA256,
			DerivativeMode: request.DerivativeMode})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	group.GET(projectConversationAttachmentBase, func(c *gin.Context) {
		principal, scope, ok := projectAttachmentRequestScope(c)
		if !ok {
			return
		}
		result, err := service.Get(c.Request.Context(), principal, scope)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	group.PUT(projectConversationAttachmentBase+"/chunks/:sequence", func(c *gin.Context) {
		principal, scope, ok := projectAttachmentRequestScope(c)
		if !ok {
			return
		}
		sequence, err := strconv.ParseInt(c.Param("sequence"), 10, 32)
		if err != nil || sequence < 0 || sequence >= 75 {
			writeError(c, domain.InvalidArgument("sequence", "project attachment chunk sequence is invalid"))
			return
		}
		if c.Request.ContentLength < 1 || c.Request.ContentLength > app.ProjectConversationAttachmentChunk {
			if c.Request.ContentLength > app.ProjectConversationAttachmentChunk {
				writeError(c, domain.NewError(domain.CodeRequestTooLarge, "project attachment chunk exceeds the size limit"))
			} else {
				writeError(c, domain.InvalidArgument("Content-Length", "project attachment chunk length is required"))
			}
			return
		}
		digests := c.Request.Header.Values(personalAttachmentChunkDigestHeader)
		if len(digests) != 1 || digests[0] == "" {
			writeError(c, domain.InvalidArgument(personalAttachmentChunkDigestHeader, "exactly one chunk digest is required"))
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, app.ProjectConversationAttachmentChunk)
		replay, err := service.PutChunk(c.Request.Context(), principal, scope, int32(sequence), digests[0], c.Request.Body, c.Request.ContentLength)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"sequence": sequence, "replay": replay})
	})
	group.POST(projectConversationAttachmentBase+"/commit", func(c *gin.Context) {
		principal, scope, ok := projectAttachmentRequestScope(c)
		if !ok || !personalAttachmentBodyIsEmpty(c) {
			return
		}
		result, err := service.Commit(c.Request.Context(), principal, scope)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	group.PUT(projectConversationAttachmentBase+"/derivatives", func(c *gin.Context) {
		principal, scope, ok := projectAttachmentRequestScope(c)
		if !ok {
			return
		}
		derivatives, ok := service.(projectConversationDerivativeService)
		if !ok {
			serviceUnavailable(c)
			return
		}
		release, ok := derivativeLimiter.acquire(principal.UserID)
		if !ok {
			writeError(c, domain.NewError(domain.CodeRateLimited, "too many project attachment derivative uploads"))
			return
		}
		defer release()
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, app.ProjectConversationDerivativeMaxJSONBytes)
		var input struct {
			Outcome string `json:"outcome"`
			Files   []struct {
				Kind      string `json:"kind"`
				Page      *int   `json:"page"`
				Name      string `json:"name"`
				MediaType string `json:"media_type"`
				SHA256    string `json:"sha256"`
				Content   []byte `json:"content_base64"`
			} `json:"files"`
		}
		if !decodeStrictJSON(c, &input) {
			return
		}
		request := app.ProjectConversationDerivatives{Outcome: input.Outcome}
		if input.Files != nil {
			request.Files = make([]app.ProjectConversationDerivativeFile, 0, len(input.Files))
			for _, file := range input.Files {
				if file.Page == nil {
					writeError(c, domain.InvalidArgument("page", "project attachment derivative page is required"))
					return
				}
				request.Files = append(request.Files, app.ProjectConversationDerivativeFile{Kind: file.Kind, Page: *file.Page,
					Name: file.Name, MediaType: file.MediaType, SHA256: file.SHA256, Content: file.Content})
			}
		}
		result, err := derivatives.PutDerivatives(c.Request.Context(), principal, scope, request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	group.GET(projectConversationAttachmentBase+"/content", func(c *gin.Context) {
		principal, scope, ok := projectAttachmentRequestScope(c)
		if !ok {
			return
		}
		release, ok := readLimiter.acquire(principal.UserID)
		if !ok {
			writeError(c, domain.NewError(domain.CodeRateLimited, "too many project attachment reads"))
			return
		}
		defer release()
		reader, metadata, err := service.Open(c.Request.Context(), principal, scope)
		if err != nil {
			writeError(c, err)
			return
		}
		writeProjectAttachmentContent(c, reader, metadata)
	})
	group.GET(projectConversationAttachmentReadBase, func(c *gin.Context) {
		principal, reference, ok := projectAttachmentRequestReference(c)
		if !ok {
			return
		}
		release, ok := readLimiter.acquire(principal.UserID)
		if !ok {
			writeError(c, domain.NewError(domain.CodeRateLimited, "too many project attachment reads"))
			return
		}
		defer release()
		metadata, err := service.GetByID(c.Request.Context(), principal, reference)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, metadata)
	})
	group.GET(projectConversationAttachmentReadBase+"/content", func(c *gin.Context) {
		principal, reference, ok := projectAttachmentRequestReference(c)
		if !ok {
			return
		}
		release, ok := readLimiter.acquire(principal.UserID)
		if !ok {
			writeError(c, domain.NewError(domain.CodeRateLimited, "too many project attachment reads"))
			return
		}
		defer release()
		reader, metadata, err := service.OpenByID(c.Request.Context(), principal, reference)
		if err != nil {
			writeError(c, err)
			return
		}
		writeProjectAttachmentContent(c, reader, metadata)
	})
}

func writeProjectAttachmentContent(c *gin.Context, reader io.ReadCloser, metadata app.ProjectConversationAttachmentMetadata) {
	stopClose := context.AfterFunc(c.Request.Context(), func() { _ = reader.Close() })
	defer stopClose()
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
}
