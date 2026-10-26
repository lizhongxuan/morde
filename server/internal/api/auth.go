package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type AuthService interface {
	Register(context.Context, string, string, string) (UserView, error)
	Login(context.Context, string, string, string) (LoginResult, error)
	Logout(context.Context, identity.Principal) error
	ChangePassword(context.Context, identity.Principal, string, string) error
	CurrentUser(context.Context, identity.Principal) (UserView, error)
}

type LoginResult struct {
	Credential string
	ExpiresAt  time.Time
	User       UserView
}

type UserView struct {
	ID                 string                 `json:"id"`
	Name               string                 `json:"name"`
	Role               identity.Role          `json:"role"`
	Status             identity.AccountStatus `json:"status"`
	MustChangePassword bool                   `json:"must_change_password"`
	Quota              TotalsView             `json:"quota"`
	Usage              TotalsView             `json:"usage"`
}

type TotalsView struct {
	Hosts    int `json:"hosts"`
	CPUMilli int `json:"cpu_milli"`
	MemMiB   int `json:"mem_mib"`
}

type loginRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

func (h *api) registerAuth(group *gin.RouterGroup) {
	group.POST("/auth/register", h.register)
	group.POST("/auth/login", h.login)
	group.POST("/auth/logout", h.logout)
	group.GET("/me", h.me)
	group.POST("/me/password", h.changePassword)
}

func (h *api) register(c *gin.Context) {
	var request loginRequest
	if !decodeJSON(c, &request) {
		return
	}
	user, err := h.deps.Auth.Register(c.Request.Context(), request.Name, request.Password, c.ClientIP())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"user": user})
}

func (h *api) login(c *gin.Context) {
	var request loginRequest
	if !decodeJSON(c, &request) {
		return
	}
	if strings.TrimSpace(request.Name) == "" || request.Password == "" {
		writeError(c, domain.InvalidArgument("name", "name and password are required"))
		return
	}
	result, err := h.deps.Auth.Login(c.Request.Context(), request.Name, request.Password, c.ClientIP())
	if err != nil {
		writeError(c, err)
		return
	}
	kind, _, _, err := identity.ParseCredential(result.Credential)
	now := time.Now()
	if err != nil || kind != identity.KindSession || !result.ExpiresAt.After(now) || result.ExpiresAt.Sub(now) > h.options.SessionTTL+time.Second {
		writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     portalCookieName,
		Value:    result.Credential,
		Path:     "/",
		Expires:  result.ExpiresAt,
		MaxAge:   int(result.ExpiresAt.Sub(now) / time.Second),
		HttpOnly: true,
		Secure:   h.options.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	c.JSON(http.StatusOK, gin.H{"user": result.User})
}

func (h *api) logout(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok || principal.Kind != identity.KindSession {
		unauthenticated(c)
		return
	}
	if err := h.deps.Auth.Logout(c.Request.Context(), principal); err != nil {
		writeError(c, err)
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     portalCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.options.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	c.Status(http.StatusNoContent)
}

func (h *api) me(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	user, err := h.deps.Auth.CurrentUser(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, user)
}

func (h *api) changePassword(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok || principal.Kind != identity.KindSession {
		unauthenticated(c)
		return
	}
	var request struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	if request.CurrentPassword == "" || request.NewPassword == "" {
		writeError(c, domain.InvalidArgument("password", "current and new passwords are required"))
		return
	}
	if err := h.deps.Auth.ChangePassword(c.Request.Context(), principal, request.CurrentPassword, request.NewPassword); err != nil {
		writeError(c, err)
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name: portalCookieName, Value: "", Path: "/", Expires: time.Unix(1, 0), MaxAge: -1,
		HttpOnly: true, Secure: h.options.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
	c.Status(http.StatusNoContent)
}
