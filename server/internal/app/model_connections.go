package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type ModelConnectionRecord struct {
	ID           string
	OwnerID      string
	Name         string
	BaseURL      string
	ModelName    string
	APIKeyCipher []byte
	Capabilities ModelCapabilities
	CheckedAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type ModelConnectionStore interface {
	CreateModelConnection(context.Context, ModelConnectionRecord) (ModelConnectionRecord, error)
	ListModelConnections(context.Context, string) ([]ModelConnectionRecord, error)
	GetModelConnection(context.Context, string) (ModelConnectionRecord, error)
	UpdateModelConnection(context.Context, ModelConnectionRecord) (ModelConnectionRecord, error)
	DeleteModelConnection(context.Context, string) error
}

type ModelConnectionView struct {
	ID           string
	Name         string
	BaseURL      string
	ModelName    string
	Shared       bool
	HasAPIKey    bool
	Capabilities ModelCapabilities
	CheckedAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type ModelConnectionCommand struct {
	Name      string
	BaseURL   string
	ModelName string
	APIKey    string
	Shared    bool
}

type ModelConnectionService struct {
	store       ModelConnectionStore
	sealer      *Sealer
	client      ModelClient
	allowedHost map[string]struct{}
}

func NewModelConnectionService(store ModelConnectionStore, sealer *Sealer, client ModelClient, allowedHosts []string) (*ModelConnectionService, error) {
	if store == nil || sealer == nil || client == nil || len(allowedHosts) == 0 {
		return nil, fmt.Errorf("model connection dependencies and allowed hosts are required")
	}
	allowed := make(map[string]struct{}, len(allowedHosts))
	for _, host := range allowedHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if !validAllowedModelHost(host) {
			return nil, fmt.Errorf("model outbound allowlist contains an invalid host")
		}
		allowed[host] = struct{}{}
	}
	return &ModelConnectionService{store: store, sealer: sealer, client: client, allowedHost: allowed}, nil
}

func (s *ModelConnectionService) List(ctx context.Context, principal identity.Principal) ([]ModelConnectionView, error) {
	if err := validateModelListPrincipal(principal); err != nil {
		return nil, err
	}
	records, err := s.store.ListModelConnections(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	views := make([]ModelConnectionView, 0, len(records))
	for _, record := range records {
		views = append(views, modelConnectionView(record))
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Shared != views[j].Shared {
			return views[i].Shared
		}
		return views[i].Name < views[j].Name
	})
	return views, nil
}

func validateModelListPrincipal(principal identity.Principal) error {
	if principal.Kind == identity.KindClient && domain.ValidUserID(principal.UserID) &&
		principal.Role == identity.RoleUser && domain.ValidClientInstallationID(principal.ClientInstallationID) &&
		principal.HostScope == "" && len(principal.HostIDs) == 0 && len(principal.Scopes) == 0 {
		return nil
	}
	return validateModelPrincipal(principal)
}

func (s *ModelConnectionService) Get(ctx context.Context, principal identity.Principal, id string) (ModelConnectionView, error) {
	if err := validateModelPrincipal(principal); err != nil {
		return ModelConnectionView{}, err
	}
	if !domain.ValidModelConnectionID(id) {
		return ModelConnectionView{}, domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	record, err := s.store.GetModelConnection(ctx, id)
	if err != nil {
		return ModelConnectionView{}, err
	}
	if !canUseModelConnection(principal, record) {
		return ModelConnectionView{}, domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	return modelConnectionView(record), nil
}

func (s *ModelConnectionService) Create(ctx context.Context, principal identity.Principal, command ModelConnectionCommand) (ModelConnectionView, error) {
	if err := validateModelPrincipal(principal); err != nil {
		return ModelConnectionView{}, err
	}
	if command.Shared && principal.Role != identity.RoleAdmin {
		return ModelConnectionView{}, domain.NewError(domain.CodeForbidden, "administrator role required for shared model connections")
	}
	if err := validateModelConnectionCommand(command, s.allowedHost, true); err != nil {
		return ModelConnectionView{}, err
	}
	id, err := domain.NewModelConnectionID()
	if err != nil {
		return ModelConnectionView{}, fmt.Errorf("generate model connection id: %w", err)
	}
	ciphertext, err := s.sealer.Seal([]byte(command.APIKey))
	if err != nil {
		return ModelConnectionView{}, fmt.Errorf("encrypt model API key: %w", err)
	}
	ownerID := principal.UserID
	if command.Shared {
		ownerID = ""
	}
	record, err := s.store.CreateModelConnection(ctx, ModelConnectionRecord{
		ID: id, OwnerID: ownerID, Name: strings.TrimSpace(command.Name), BaseURL: strings.TrimRight(strings.TrimSpace(command.BaseURL), "/"),
		ModelName: strings.TrimSpace(command.ModelName), APIKeyCipher: ciphertext,
	})
	if err != nil {
		return ModelConnectionView{}, err
	}
	return modelConnectionView(record), nil
}

func (s *ModelConnectionService) Update(ctx context.Context, principal identity.Principal, id string, command ModelConnectionCommand) (ModelConnectionView, error) {
	if err := validateModelPrincipal(principal); err != nil {
		return ModelConnectionView{}, err
	}
	if !domain.ValidModelConnectionID(id) {
		return ModelConnectionView{}, domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	current, err := s.store.GetModelConnection(ctx, id)
	if err != nil {
		return ModelConnectionView{}, err
	}
	if !canManageModelConnection(principal, current) {
		return ModelConnectionView{}, domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	command.Shared = current.OwnerID == ""
	if err := validateModelConnectionCommand(command, s.allowedHost, false); err != nil {
		return ModelConnectionView{}, err
	}
	changedEndpoint := strings.TrimRight(strings.TrimSpace(command.BaseURL), "/") != current.BaseURL || strings.TrimSpace(command.ModelName) != current.ModelName
	if command.APIKey != "" {
		if err := validateAPIKey(command.APIKey); err != nil {
			return ModelConnectionView{}, err
		}
		current.APIKeyCipher, err = s.sealer.Seal([]byte(command.APIKey))
		if err != nil {
			return ModelConnectionView{}, fmt.Errorf("encrypt model API key: %w", err)
		}
		changedEndpoint = true
	}
	current.Name = strings.TrimSpace(command.Name)
	current.BaseURL = strings.TrimRight(strings.TrimSpace(command.BaseURL), "/")
	current.ModelName = strings.TrimSpace(command.ModelName)
	if changedEndpoint {
		current.Capabilities = ModelCapabilities{ModelID: current.ID}
		current.CheckedAt = nil
	}
	updated, err := s.store.UpdateModelConnection(ctx, current)
	if err != nil {
		return ModelConnectionView{}, err
	}
	return modelConnectionView(updated), nil
}

func (s *ModelConnectionService) Delete(ctx context.Context, principal identity.Principal, id string) error {
	if err := validateModelPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidModelConnectionID(id) {
		return domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	current, err := s.store.GetModelConnection(ctx, id)
	if err != nil {
		return err
	}
	if !canManageModelConnection(principal, current) {
		return domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	return s.store.DeleteModelConnection(ctx, id)
}

func (s *ModelConnectionService) Check(ctx context.Context, principal identity.Principal, id string) (ModelCapabilities, error) {
	if err := validateModelPrincipal(principal); err != nil {
		return ModelCapabilities{}, err
	}
	if !domain.ValidModelConnectionID(id) {
		return ModelCapabilities{}, domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	record, err := s.store.GetModelConnection(ctx, id)
	if err != nil {
		return ModelCapabilities{}, err
	}
	if !canManageModelConnection(principal, record) {
		return ModelCapabilities{}, domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	key, err := s.sealer.Open(record.APIKeyCipher)
	if err != nil {
		return ModelCapabilities{}, fmt.Errorf("decrypt model API key: %w", err)
	}
	capabilities, err := s.client.Check(ctx, ModelEndpoint{BaseURL: record.BaseURL, APIKey: string(key), ModelID: record.ModelName})
	if err != nil {
		return ModelCapabilities{}, modelConnectionCheckError(err)
	}
	checkedAt := time.Now().UTC()
	capabilities.ModelID = record.ID
	record.Capabilities = capabilities
	record.CheckedAt = &checkedAt
	if _, err := s.store.UpdateModelConnection(ctx, record); err != nil {
		return ModelCapabilities{}, err
	}
	return capabilities, nil
}

func modelConnectionCheckError(err error) error {
	var clientErr *ModelClientError
	if !errors.As(err, &clientErr) {
		return err
	}
	code := domain.CodeUpstreamError
	switch clientErr.Kind {
	case ModelErrorRateLimited:
		code = domain.CodeRateLimited
	case ModelErrorTimeout:
		code = domain.CodeUpstreamTimeout
	}
	result := domain.NewError(code, "model connection check failed")
	result.Details["reason"] = string(clientErr.Kind)
	return result
}

func (s *ModelConnectionService) IsConfiguredModel(ctx context.Context, ownerID, modelID string) (bool, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidModelConnectionID(modelID) {
		return false, nil
	}
	record, err := s.store.GetModelConnection(ctx, modelID)
	if domain.CodeOf(err) == domain.CodeNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if record.OwnerID != "" && record.OwnerID != ownerID {
		return false, nil
	}
	return record.CheckedAt != nil && record.Capabilities.Text && record.Capabilities.Tools, nil
}

func (s *ModelConnectionService) ResolveEndpoint(ctx context.Context, ownerID, modelID string) (ModelEndpoint, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidModelConnectionID(modelID) {
		return ModelEndpoint{}, domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	record, err := s.store.GetModelConnection(ctx, modelID)
	if err != nil {
		return ModelEndpoint{}, err
	}
	if (record.OwnerID != "" && record.OwnerID != ownerID) || record.CheckedAt == nil || !record.Capabilities.Text || !record.Capabilities.Tools {
		return ModelEndpoint{}, domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	key, err := s.sealer.Open(record.APIKeyCipher)
	if err != nil {
		return ModelEndpoint{}, fmt.Errorf("decrypt model API key: %w", err)
	}
	return ModelEndpoint{BaseURL: record.BaseURL, APIKey: string(key), ModelID: record.ModelName}, nil
}

func validateModelPrincipal(principal identity.Principal) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) ||
		(principal.Role != identity.RoleUser && principal.Role != identity.RoleAdmin) || principal.HostScope != "" ||
		len(principal.HostIDs) != 0 || len(principal.Scopes) != 0 {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return nil
}

func canUseModelConnection(principal identity.Principal, record ModelConnectionRecord) bool {
	return record.OwnerID == "" || record.OwnerID == principal.UserID
}

func canManageModelConnection(principal identity.Principal, record ModelConnectionRecord) bool {
	return record.OwnerID == principal.UserID || (record.OwnerID == "" && principal.Role == identity.RoleAdmin)
}

func modelConnectionView(record ModelConnectionRecord) ModelConnectionView {
	return ModelConnectionView{
		ID: record.ID, Name: record.Name, BaseURL: record.BaseURL, ModelName: record.ModelName,
		Shared: record.OwnerID == "", HasAPIKey: len(record.APIKeyCipher) > 0,
		Capabilities: record.Capabilities, CheckedAt: record.CheckedAt, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

func validateModelConnectionCommand(command ModelConnectionCommand, allowed map[string]struct{}, requireKey bool) error {
	name := strings.TrimSpace(command.Name)
	modelName := strings.TrimSpace(command.ModelName)
	baseURL := strings.TrimSpace(command.BaseURL)
	if name == "" || len(name) > 80 || modelName == "" || len(modelName) > 256 {
		return domain.InvalidArgument("name", "model connection name and model are required and must be within limits")
	}
	if err := validateModelBaseURL(baseURL, allowed); err != nil {
		return domain.InvalidArgument("base_url", err.Error())
	}
	if requireKey || command.APIKey != "" {
		if err := validateAPIKey(command.APIKey); err != nil {
			return err
		}
	}
	return nil
}

func validateAPIKey(value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 4096 || strings.TrimSpace(value) != value {
		return domain.InvalidArgument("api_key", "API key is required and must not contain surrounding whitespace")
	}
	return nil
}

func validateModelBaseURL(value string, allowed map[string]struct{}) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || len(value) > 2048 || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(value, "#") {
		return fmt.Errorf("base URL must be HTTPS without credentials, query, or fragment")
	}
	port := parsed.Port()
	host := strings.ToLower(parsed.Hostname())
	if (port != "" && port != "443") || (!strings.EqualFold(parsed.Host, host) && !strings.EqualFold(parsed.Host, host+":443")) {
		return fmt.Errorf("base URL port must be 443")
	}
	if net.ParseIP(host) != nil {
		return fmt.Errorf("base URL host must be an allowlisted DNS name")
	}
	if _, ok := allowed[host]; !ok {
		return fmt.Errorf("base URL host is not allowed")
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("base URL path contains an invalid segment")
		}
	}
	return nil
}

func validAllowedModelHost(host string) bool {
	if host == "" || len(host) > 253 || !strings.Contains(host, ".") || net.ParseIP(host) != nil || strings.ContainsAny(host, ":/@?#*\\") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}
