package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

type hostMemberResponse struct {
	UserID      string            `json:"user_id"`
	Permissions []host.Permission `json:"permissions"`
	CreatedAt   time.Time         `json:"created_at"`
}

type sharedHostResponse struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	State       host.State        `json:"state"`
	URL         string            `json:"url"`
	Permissions []host.Permission `json:"permissions"`
}

type inviteHostMemberRequest struct {
	UserID      string            `json:"user_id"`
	Permissions []host.Permission `json:"permissions"`
}

func (h *api) inviteHostMember(c *gin.Context) {
	if h.deps.HostSharing == nil {
		writeError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	hostID, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	var request inviteHostMemberRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeDecodeError(c, err)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(c, domain.InvalidArgument("body", "request body must contain exactly one value"))
		return
	}
	member, err := h.deps.HostSharing.Invite(c.Request.Context(), principal, hostID, request.UserID, request.Permissions)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toHostMemberResponse(member))
}

func (h *api) listHostMembers(c *gin.Context) {
	if h.deps.HostSharing == nil {
		writeError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	hostID, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	items, err := h.deps.HostSharing.ListMembers(c.Request.Context(), principal, hostID)
	if err != nil {
		writeError(c, err)
		return
	}
	response := struct {
		Items []hostMemberResponse `json:"items"`
	}{Items: make([]hostMemberResponse, 0, len(items))}
	for _, item := range items {
		response.Items = append(response.Items, toHostMemberResponse(item))
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) revokeHostMember(c *gin.Context) {
	if h.deps.HostSharing == nil {
		writeError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	hostID, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	userID, ok := safeIDParam(c, "user_id", "user_id", domain.ValidUserID)
	if !ok {
		return
	}
	if err := h.deps.HostSharing.Revoke(c.Request.Context(), principal, hostID, userID); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *api) listSharedHosts(c *gin.Context) {
	if h.deps.HostSharing == nil {
		writeError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	items, err := h.deps.HostSharing.ListSharedHosts(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := struct {
		Items []sharedHostResponse `json:"items"`
	}{Items: make([]sharedHostResponse, 0, len(items))}
	for _, item := range items {
		if !domain.ValidHostID(item.HostID) || item.Name == "" || !validHostState(item.State) || len(item.Permissions) == 0 {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		response.Items = append(response.Items, sharedHostResponse{
			ID: item.HostID, Name: item.Name, State: item.State, URL: h.hostURL(item.HostID),
			Permissions: append([]host.Permission{}, item.Permissions...),
		})
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) issueObservationTicket(c *gin.Context) {
	if h.deps.HostSharing == nil {
		writeError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	hostID, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || len(query) > 1 || len(query["target"]) > 1 || len(query) == 1 && query["target"] == nil {
		writeError(c, domain.InvalidArgument("target", "ticket target is invalid"))
		return
	}
	target := "desktop"
	if values, exists := query["target"]; exists {
		target = values[0]
	}
	var info app.EnterInfo
	switch target {
	case "desktop":
		info, err = h.deps.HostSharing.IssueObservationTicket(c.Request.Context(), principal, hostID)
	case "terminal":
		info, err = h.deps.HostSharing.IssueTerminalTicket(c.Request.Context(), principal, hostID)
	default:
		writeError(c, domain.InvalidArgument("target", "ticket target is invalid"))
		return
	}
	if err != nil {
		writeError(c, err)
		return
	}
	if info.URL == "" || !info.ExpiresAt.After(time.Now()) {
		writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}{URL: info.URL, ExpiresAt: info.ExpiresAt})
}

func toHostMemberResponse(value host.Member) hostMemberResponse {
	return hostMemberResponse{UserID: value.UserID, Permissions: append([]host.Permission{}, value.Permissions...), CreatedAt: value.CreatedAt}
}
