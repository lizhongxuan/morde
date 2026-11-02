package api

import (
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/record"
)

type recordConsentView struct {
	OwnerID   string         `json:"owner_id"`
	Purpose   record.Purpose `json:"purpose"`
	Enabled   bool           `json:"enabled"`
	Version   int64          `json:"version"`
	UpdatedAt *time.Time     `json:"updated_at,omitempty"`
}

type recordQuotaView struct {
	OwnerID       string    `json:"owner_id"`
	RetentionDays int32     `json:"retention_days"`
	LimitBytes    int64     `json:"limit_bytes"`
	UsedBytes     int64     `json:"used_bytes"`
	ReservedBytes int64     `json:"reserved_bytes"`
	Version       int64     `json:"version"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type recordGrantView struct {
	OwnerID    string                 `json:"owner_id"`
	AdminID    string                 `json:"admin_id"`
	Capability record.AdminCapability `json:"capability"`
	Granted    bool                   `json:"granted"`
	GrantedBy  string                 `json:"granted_by"`
	Version    int64                  `json:"version"`
	UpdatedAt  time.Time              `json:"updated_at"`
}

type recordMetadataView struct {
	ID              string          `json:"id"`
	OwnerID         string          `json:"owner_id"`
	Purpose         record.Purpose  `json:"purpose"`
	Kind            record.ItemKind `json:"kind"`
	ConsentVersion  int64           `json:"consent_version"`
	CreatedAt       time.Time       `json:"created_at"`
	ExpiresAt       time.Time       `json:"expires_at"`
	PayloadBytes    int64           `json:"payload_bytes"`
	AttachmentBytes int64           `json:"attachment_bytes"`
	AttachmentCount int32           `json:"attachment_count"`
	State           string          `json:"state"`
}

type recordAttachmentView struct {
	ID        string `json:"id"`
	RecordID  string `json:"record_id"`
	FileName  string `json:"file_name"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type recordConsentsResponse struct {
	Items []recordConsentView `json:"items"`
}

type recordAdminSettingsResponse struct {
	OwnerID  string              `json:"owner_id"`
	Consents []recordConsentView `json:"consents"`
	Quota    recordQuotaView     `json:"quota"`
	Grants   []recordGrantView   `json:"grants"`
}

type recordAdminAccessResponse struct {
	Capabilities []record.AdminCapability `json:"capabilities"`
	Consents     []recordConsentView      `json:"consents"`
}

type recordPageResponse struct {
	Items      []recordMetadataView `json:"items"`
	NextBefore string               `json:"next_before"`
}

type recordContentResponse struct {
	Metadata recordMetadataView `json:"metadata"`
	Payload  json.RawMessage    `json:"payload"`
}

type recordDeleteResponse struct {
	ID          string             `json:"id"`
	OwnerID     string             `json:"owner_id"`
	RecordID    string             `json:"record_id"`
	State       record.DeleteState `json:"state"`
	Attempts    int32              `json:"attempts"`
	RequestedAt time.Time          `json:"requested_at"`
	CompletedAt *time.Time         `json:"completed_at,omitempty"`
}

type recordConsentRequest struct {
	Purpose record.Purpose `json:"purpose"`
	Enabled *bool          `json:"enabled"`
}

type recordQuotaRequest struct {
	ExpectedVersion *int64 `json:"expected_version"`
	RetentionDays   *int32 `json:"retention_days"`
	LimitBytes      *int64 `json:"limit_bytes"`
}

type recordGrantRequest struct {
	AdminID    string                 `json:"admin_id"`
	Capability record.AdminCapability `json:"capability"`
	Granted    *bool                  `json:"granted"`
}

func (h *api) registerAdminRecords(group *gin.RouterGroup) {
	group.GET("/me/record-consents", h.listRecordConsents)
	group.PUT("/me/record-consents", h.setRecordConsent)
	group.GET("/admin/users/:id/record-settings", h.getAdminRecordSettings)
	group.GET("/admin/users/:id/record-access", h.getAdminRecordAccess)
	group.PUT("/admin/users/:id/record-quota", h.setAdminRecordQuota)
	group.PUT("/admin/users/:id/record-grants", h.setAdminRecordGrant)
	group.GET("/admin/users/:id/records", h.listAdminRecords)
	group.GET("/admin/users/:id/records/:record_id", h.getAdminRecord)
	group.GET("/admin/users/:id/records/:record_id/attachments", h.listAdminRecordAttachments)
	group.GET("/admin/users/:id/records/:record_id/attachments/:attachment_id", h.getAdminRecordAttachment)
	group.GET("/admin/users/:id/records/:record_id/export", h.exportAdminRecord)
	group.DELETE("/admin/users/:id/records/:record_id", h.deleteAdminRecord)
}

func (h *api) listRecordConsents(c *gin.Context) {
	if h.deps.RecordConsents == nil {
		serviceUnavailable(c)
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	values, err := h.deps.RecordConsents.List(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := recordConsentsResponse{Items: make([]recordConsentView, 0, len(values))}
	for _, value := range values {
		response.Items = append(response.Items, recordConsentViewOf(value))
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) setRecordConsent(c *gin.Context) {
	if h.deps.RecordConsents == nil {
		serviceUnavailable(c)
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	var request recordConsentRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if !request.Purpose.Valid() || request.Enabled == nil {
		writeError(c, domain.InvalidArgument("consent", "record consent purpose and enabled value are required"))
		return
	}
	value, err := h.deps.RecordConsents.Set(c.Request.Context(), principal, request.Purpose, *request.Enabled)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, recordConsentViewOf(value))
}

func (h *api) getAdminRecordSettings(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	ownerID, ok := recordOwnerPath(c)
	if !ok {
		return
	}
	if h.deps.RecordAccess == nil {
		serviceUnavailable(c)
		return
	}
	settings, err := h.deps.RecordAccess.AdminSettings(c.Request.Context(), principal, ownerID)
	if err != nil {
		writeError(c, err)
		return
	}
	response := recordAdminSettingsResponse{
		OwnerID:  ownerID,
		Consents: make([]recordConsentView, 0, len(settings.Consents)),
		Quota:    recordQuotaViewOf(settings.Quota),
		Grants:   make([]recordGrantView, 0, len(settings.Grants)),
	}
	for _, value := range settings.Consents {
		response.Consents = append(response.Consents, recordConsentViewOf(value))
	}
	for _, value := range settings.Grants {
		response.Grants = append(response.Grants, recordGrantViewOf(value))
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) getAdminRecordAccess(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	ownerID, ok := recordOwnerPath(c)
	if !ok {
		return
	}
	if h.deps.RecordAccess == nil {
		serviceUnavailable(c)
		return
	}
	settings, err := h.deps.RecordAccess.AccessSettings(c.Request.Context(), principal, ownerID)
	if err != nil {
		writeError(c, err)
		return
	}
	response := recordAdminAccessResponse{
		Capabilities: make([]record.AdminCapability, 0, len(settings.Grants)),
		Consents:     make([]recordConsentView, 0, len(settings.Consents)),
	}
	for _, value := range settings.Grants {
		if value.Granted {
			response.Capabilities = append(response.Capabilities, value.Capability)
		}
	}
	for _, value := range settings.Consents {
		response.Consents = append(response.Consents, recordConsentViewOf(value))
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) setAdminRecordQuota(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	ownerID, ok := recordOwnerPath(c)
	if !ok {
		return
	}
	if h.deps.RecordConsents == nil {
		serviceUnavailable(c)
		return
	}
	var request recordQuotaRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.ExpectedVersion == nil || *request.ExpectedVersion < 1 || request.RetentionDays == nil || request.LimitBytes == nil {
		writeError(c, domain.InvalidArgument("record_quota", "expected_version, retention_days, and limit_bytes are required"))
		return
	}
	value, err := h.deps.RecordConsents.SetQuota(c.Request.Context(), principal, ownerID, *request.ExpectedVersion, *request.RetentionDays, *request.LimitBytes)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, recordQuotaViewOf(value))
}

func (h *api) setAdminRecordGrant(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	ownerID, ok := recordOwnerPath(c)
	if !ok {
		return
	}
	if h.deps.RecordAccess == nil {
		serviceUnavailable(c)
		return
	}
	var request recordGrantRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if !domain.ValidUserID(request.AdminID) || !request.Capability.Valid() || request.Granted == nil {
		writeError(c, domain.InvalidArgument("record_grant", "admin_id, capability, and granted value are required"))
		return
	}
	value, err := h.deps.RecordAccess.SetGrant(c.Request.Context(), principal, ownerID, request.AdminID, request.Capability, *request.Granted)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, recordGrantViewOf(value))
}

func (h *api) listAdminRecords(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	ownerID, ok := recordOwnerPath(c)
	if !ok {
		return
	}
	if h.deps.RecordAccess == nil {
		serviceUnavailable(c)
		return
	}
	query, ok := recordQuery(c)
	if !ok {
		return
	}
	page, err := h.deps.RecordAccess.List(c.Request.Context(), principal, ownerID, query)
	if err != nil {
		writeError(c, err)
		return
	}
	response := recordPageResponse{Items: make([]recordMetadataView, 0, len(page.Items)), NextBefore: page.NextBefore}
	for _, value := range page.Items {
		response.Items = append(response.Items, recordMetadataViewOf(value))
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) getAdminRecord(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	ownerID, recordID, ok := recordPath(c)
	if !ok {
		return
	}
	if h.deps.RecordAccess == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.RecordAccess.Read(c.Request.Context(), principal, ownerID, recordID)
	if err != nil {
		writeError(c, err)
		return
	}
	payload, ok := safeRecordJSON(value.Payload)
	if !ok {
		writeError(c, domain.NewError(domain.CodeForbidden, "record payload contains unsupported or secret-like content"))
		return
	}
	c.JSON(http.StatusOK, recordContentResponse{Metadata: recordMetadataViewOf(value.Metadata), Payload: payload})
}

func (h *api) exportAdminRecord(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	ownerID, recordID, ok := recordPath(c)
	if !ok {
		return
	}
	if h.deps.RecordAccess == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.RecordAccess.Export(c.Request.Context(), principal, ownerID, recordID)
	if err != nil {
		writeError(c, err)
		return
	}
	payload, ok := safeRecordJSON(value.Payload)
	if !ok {
		writeError(c, domain.NewError(domain.CodeForbidden, "record export contains unsupported or secret-like content"))
		return
	}
	filename := "record-" + recordID + ".json"
	c.Header("Content-Type", "application/json")
	c.Header("Content-Length", strconv.Itoa(len(payload)))
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "application/json", payload)
}

func (h *api) listAdminRecordAttachments(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	ownerID, recordID, ok := recordPath(c)
	if !ok {
		return
	}
	if h.deps.RecordAccess == nil {
		serviceUnavailable(c)
		return
	}
	values, err := h.deps.RecordAccess.ListAttachments(c.Request.Context(), principal, ownerID, recordID)
	if err != nil {
		writeError(c, err)
		return
	}
	response := struct {
		Items []recordAttachmentView `json:"items"`
	}{Items: make([]recordAttachmentView, 0, len(values))}
	for _, value := range values {
		response.Items = append(response.Items, recordAttachmentViewOf(value))
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) getAdminRecordAttachment(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	ownerID, recordID, ok := recordPath(c)
	if !ok {
		return
	}
	attachmentID := c.Param("attachment_id")
	if !record.ValidAttachmentID(attachmentID) {
		writeError(c, domain.InvalidArgument("attachment_id", "record attachment id is invalid"))
		return
	}
	if h.deps.RecordAccess == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.RecordAccess.ReadAttachment(c.Request.Context(), principal, ownerID, recordID, attachmentID)
	if err != nil {
		writeError(c, err)
		return
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": value.Metadata.FileName})
	if disposition == "" {
		disposition = "attachment"
	}
	c.Header("Content-Type", value.Metadata.MediaType)
	c.Header("Content-Length", strconv.Itoa(len(value.Content)))
	c.Header("Content-Disposition", disposition)
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("ETag", `"`+value.Metadata.SHA256+`"`)
	c.Data(http.StatusOK, value.Metadata.MediaType, value.Content)
}

func (h *api) deleteAdminRecord(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	ownerID, recordID, ok := recordPath(c)
	if !ok {
		return
	}
	if h.deps.RecordAccess == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.RecordAccess.Delete(c.Request.Context(), principal, ownerID, recordID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, recordDeleteResponse{
		ID: value.ID, OwnerID: value.OwnerID, RecordID: value.RecordID,
		State: value.State, Attempts: value.Attempts, RequestedAt: value.RequestedAt, CompletedAt: value.CompletedAt,
	})
}

func recordOwnerPath(c *gin.Context) (string, bool) {
	ownerID := c.Param("id")
	if !domain.ValidUserID(ownerID) {
		writeError(c, domain.InvalidArgument("id", "record owner id is invalid"))
		return "", false
	}
	return ownerID, true
}

func recordPath(c *gin.Context) (string, string, bool) {
	ownerID, ok := recordOwnerPath(c)
	if !ok {
		return "", "", false
	}
	recordID := c.Param("record_id")
	if !record.ValidRecordID(recordID) {
		writeError(c, domain.InvalidArgument("record_id", "record id is invalid"))
		return "", "", false
	}
	return ownerID, recordID, true
}

func recordQuery(c *gin.Context) (record.Query, bool) {
	purposeText, ok := singleQuery(c, "purpose")
	if !ok {
		return record.Query{}, false
	}
	purpose := record.Purpose(purposeText)
	if purposeText != "" && !purpose.Valid() {
		writeError(c, domain.InvalidArgument("purpose", "record purpose is invalid"))
		return record.Query{}, false
	}
	beforeID, ok := singleQuery(c, "before_id")
	if !ok {
		return record.Query{}, false
	}
	limit := int64(50)
	if values, exists := c.Request.URL.Query()["limit"]; exists {
		if len(values) != 1 {
			writeError(c, domain.InvalidArgument("limit", "record page limit must appear once"))
			return record.Query{}, false
		}
		var err error
		limit, err = strconv.ParseInt(values[0], 10, 32)
		if err != nil || limit < 1 || limit > 100 {
			writeError(c, domain.InvalidArgument("limit", "record page limit must be between 1 and 100"))
			return record.Query{}, false
		}
	}
	return record.Query{Purpose: purpose, BeforeID: beforeID, Limit: int32(limit)}, true
}

func recordConsentViewOf(value record.Consent) recordConsentView {
	var updatedAt *time.Time
	if !value.UpdatedAt.IsZero() {
		t := value.UpdatedAt
		updatedAt = &t
	}
	return recordConsentView{OwnerID: value.OwnerID, Purpose: value.Purpose, Enabled: value.Enabled, Version: value.Version, UpdatedAt: updatedAt}
}

func recordQuotaViewOf(value record.Quota) recordQuotaView {
	return recordQuotaView{
		OwnerID: value.OwnerID, RetentionDays: value.RetentionDays, LimitBytes: value.LimitBytes,
		UsedBytes: value.UsedBytes, ReservedBytes: value.ReservedBytes, Version: value.Version, UpdatedAt: value.UpdatedAt,
	}
}

func recordGrantViewOf(value record.AdminGrant) recordGrantView {
	return recordGrantView{
		OwnerID: value.OwnerID, AdminID: value.AdminID, Capability: value.Capability, Granted: value.Granted,
		GrantedBy: value.GrantedBy, Version: value.Version, UpdatedAt: value.UpdatedAt,
	}
}

func recordMetadataViewOf(value record.RecordMetadata) recordMetadataView {
	return recordMetadataView{
		ID: value.ID, OwnerID: value.OwnerID, Purpose: value.Purpose, Kind: value.Kind,
		ConsentVersion: value.ConsentVersion, CreatedAt: value.CreatedAt, ExpiresAt: value.ExpiresAt,
		PayloadBytes: value.PayloadBytes, AttachmentBytes: value.AttachmentBytes,
		AttachmentCount: value.AttachmentCount, State: value.State,
	}
}

func recordAttachmentViewOf(value record.AttachmentMetadata) recordAttachmentView {
	return recordAttachmentView{
		ID: value.ID, RecordID: value.RecordID, FileName: value.FileName,
		MediaType: value.MediaType, SizeBytes: value.SizeBytes, SHA256: value.SHA256,
	}
}

func safeRecordJSON(payload []byte) (json.RawMessage, bool) {
	if len(payload) == 0 || int64(len(payload)) > record.MaxCapturePayloadBytes || !json.Valid(payload) {
		return nil, false
	}
	if record.ContainsSensitiveContent(payload) {
		return nil, false
	}
	return json.RawMessage(payload), true
}
