package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

const hostCookieName = "ch_host"

func (g *gateway) signCookie(hostID, userID string, expires time.Time) string {
	unsigned := strings.Join([]string{hostID, userID, strconv.FormatInt(expires.Unix(), 10)}, ".")
	mac := hmac.New(sha256.New, g.cookieKey)
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + hex.EncodeToString(mac.Sum(nil))
}

func (g *gateway) signClientCookie(hostID, userID, installationID string, expires time.Time) string {
	unsigned := strings.Join([]string{"v2", hostID, userID, strconv.FormatInt(expires.Unix(), 10), installationID}, ".")
	mac := hmac.New(sha256.New, g.cookieKey)
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + hex.EncodeToString(mac.Sum(nil))
}

func (g *gateway) verifyCookie(raw, expectedHost string, now time.Time) (identity.Principal, time.Duration, bool) {
	parts := strings.Split(raw, ".")
	client := len(parts) == 6 && parts[0] == "v2"
	if !client && len(parts) != 4 {
		return identity.Principal{}, 0, false
	}
	fields := parts
	installationID := ""
	if client {
		fields = parts[1:]
		installationID = fields[3]
		if !domain.ValidClientInstallationID(installationID) {
			return identity.Principal{}, 0, false
		}
	}
	if fields[0] != expectedHost || !domain.ValidHostID(fields[0]) || !domain.ValidUserID(fields[1]) || len(fields[len(fields)-1]) != sha256.Size*2 {
		return identity.Principal{}, 0, false
	}
	expiresUnix, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return identity.Principal{}, 0, false
	}
	expires := time.Unix(expiresUnix, 0)
	if !expires.After(now) {
		return identity.Principal{}, 0, false
	}
	sig, err := hex.DecodeString(fields[len(fields)-1])
	if err != nil || len(sig) != sha256.Size {
		return identity.Principal{}, 0, false
	}
	mac := hmac.New(sha256.New, g.cookieKey)
	_, _ = mac.Write([]byte(strings.Join(parts[:len(parts)-1], ".")))
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return identity.Principal{}, 0, false
	}
	return identity.Principal{UserID: fields[1], Role: identity.RoleUser, Kind: identity.KindHostCookie, HostScope: fields[0], ClientInstallationID: installationID}, expires.Sub(now), true
}

func (g *gateway) setHostCookie(c *gin.Context, hostID, userID, installationID string) {
	expires := time.Now().Add(g.deps.Security.HostCookieTTL)
	value := g.signCookie(hostID, userID, expires)
	if installationID != "" {
		value = g.signClientCookie(hostID, userID, installationID, expires)
	}
	cookie := &http.Cookie{
		Name:     hostCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(g.deps.Security.HostCookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   g.deps.Domains.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	c.Writer.Header().Add("Set-Cookie", cookie.String())
}
func (g *gateway) clearHostCookie(c *gin.Context) {
	cookie := &http.Cookie{
		Name:     hostCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		Secure:   g.deps.Domains.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	c.Writer.Header().Add("Set-Cookie", cookie.String())
}
