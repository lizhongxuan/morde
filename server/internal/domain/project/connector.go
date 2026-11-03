package project

import (
	"errors"
	"time"

	"morde/server/internal/domain"
)

// MCPConnectorGrant records a project's explicit authorization to use a
// connector that remains owned by an individual account. It never contains
// credential material.
type MCPConnectorGrant struct {
	ProjectID        string
	ConnectorOwnerID string
	ConnectorID      string
	Version          int64
	AuthorizedBy     string
	Active           bool
	RevokedAt        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (value MCPConnectorGrant) Validate() error {
	if !ValidID(value.ProjectID) {
		return errors.New("project connector grant project id is invalid")
	}
	if !domain.ValidUserID(value.ConnectorOwnerID) || !domain.ValidMCPConnectorID(value.ConnectorID) {
		return errors.New("project connector reference is invalid")
	}
	if value.Version < 1 || value.AuthorizedBy != value.ConnectorOwnerID {
		return errors.New("project connector authorization is invalid")
	}
	if value.Active == (value.RevokedAt != nil) {
		return errors.New("project connector grant state is invalid")
	}
	if value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return errors.New("project connector grant timestamps are invalid")
	}
	return nil
}
