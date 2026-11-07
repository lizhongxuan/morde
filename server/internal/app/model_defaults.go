package app

import (
	"context"
	"math"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type AdminModelDefaultRecord struct {
	ModelID   string
	Version   int64
	UpdatedAt time.Time
}

type AdminModelDefaultView struct {
	Configured bool
	ModelID    string
	Version    int64
}

type AdminModelDefaultAvailability struct {
	Configured bool
	ModelID    string
}

type AdminModelDefaultStore interface {
	GetAdminModelDefault(context.Context) (AdminModelDefaultRecord, error)
	SetAdminModelDefault(context.Context, identity.Principal, int64, string, string) (AdminModelDefaultRecord, error)
	GetModelConnection(context.Context, string) (ModelConnectionRecord, error)
}

type AdminModelDefaultService struct {
	store AdminModelDefaultStore
}

func NewAdminModelDefaultService(store AdminModelDefaultStore) (*AdminModelDefaultService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("default_model", "default model store is required")
	}
	return &AdminModelDefaultService{store: store}, nil
}

func (s *AdminModelDefaultService) Get(ctx context.Context, principal identity.Principal) (AdminModelDefaultView, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminModelDefaultView{}, err
	}
	record, err := s.store.GetAdminModelDefault(ctx)
	if err != nil {
		return AdminModelDefaultView{}, err
	}
	configured, err := adminDefaultModelAvailable(ctx, s.store, record.ModelID)
	if err != nil {
		return AdminModelDefaultView{}, err
	}
	if !validAdminModelDefaultRecord(record) {
		return AdminModelDefaultView{}, domain.NewError(domain.CodeInternal, "default model setting is invalid")
	}
	return AdminModelDefaultView{Configured: configured, ModelID: record.ModelID, Version: record.Version}, nil
}

func (s *AdminModelDefaultService) Update(ctx context.Context, principal identity.Principal, expectedVersion int64, modelID, ip string) (AdminModelDefaultView, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminModelDefaultView{}, err
	}
	if expectedVersion < 0 || expectedVersion == math.MaxInt64 || len(ip) > 64 ||
		(modelID != "" && !domain.ValidModelConnectionID(modelID)) {
		return AdminModelDefaultView{}, domain.InvalidArgument("default_model", "invalid default model setting")
	}
	if modelID != "" {
		candidate, err := s.store.GetModelConnection(ctx, modelID)
		if domain.CodeOf(err) == domain.CodeNotFound || (err == nil && candidate.OwnerID != "") {
			return AdminModelDefaultView{}, domain.NewError(domain.CodeNotFound, "model connection not found")
		}
		if err != nil {
			return AdminModelDefaultView{}, err
		}
		if candidate.CheckedAt == nil || !candidate.Capabilities.Text {
			return AdminModelDefaultView{}, domain.NewError(domain.CodeUnsupported, "default model must be a checked shared text model")
		}
	}
	record, err := s.store.SetAdminModelDefault(ctx, principal, expectedVersion, modelID, ip)
	if err != nil {
		return AdminModelDefaultView{}, err
	}
	if !validAdminModelDefaultRecord(record) || record.Version != expectedVersion+1 || record.ModelID != modelID {
		return AdminModelDefaultView{}, domain.NewError(domain.CodeInternal, "updated default model setting is invalid")
	}
	configured, err := adminDefaultModelAvailable(ctx, s.store, record.ModelID)
	if err != nil {
		return AdminModelDefaultView{}, err
	}
	return AdminModelDefaultView{Configured: configured, ModelID: record.ModelID, Version: record.Version}, nil
}

func (s *AdminModelDefaultService) Available(ctx context.Context) (AdminModelDefaultAvailability, error) {
	record, err := s.store.GetAdminModelDefault(ctx)
	if err != nil {
		return AdminModelDefaultAvailability{}, err
	}
	if !validAdminModelDefaultRecord(record) {
		return AdminModelDefaultAvailability{}, domain.NewError(domain.CodeInternal, "default model setting is invalid")
	}
	configured, err := adminDefaultModelAvailable(ctx, s.store, record.ModelID)
	if err != nil {
		return AdminModelDefaultAvailability{}, err
	}
	modelID := ""
	if configured {
		modelID = record.ModelID
	}
	return AdminModelDefaultAvailability{Configured: configured, ModelID: modelID}, nil
}

func validAdminModelDefaultRecord(record AdminModelDefaultRecord) bool {
	return record.Version >= 0 && (record.ModelID == "" || domain.ValidModelConnectionID(record.ModelID))
}

func adminDefaultModelAvailable(ctx context.Context, store interface {
	GetModelConnection(context.Context, string) (ModelConnectionRecord, error)
}, modelID string) (bool, error) {
	if modelID == "" || !domain.ValidModelConnectionID(modelID) {
		return false, nil
	}
	record, err := store.GetModelConnection(ctx, modelID)
	if domain.CodeOf(err) == domain.CodeNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return record.OwnerID == "" && record.CheckedAt != nil && record.Capabilities.Text, nil
}
