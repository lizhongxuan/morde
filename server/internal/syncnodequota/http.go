package syncnodequota

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

const maxRequestBytes = 16 << 10

var operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)
var reservationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type Operation string

const (
	OperationUpsert          Operation = "upsert"
	OperationDelete          Operation = "delete"
	OperationDirectoryUpsert Operation = "directory_upsert"
	OperationDirectoryDelete Operation = "directory_delete"
)

type ReserveRequest struct {
	WorkspaceID string    `json:"workspace_id"`
	FolderID    string    `json:"folder_id"`
	OperationID string    `json:"operation_id"`
	Path        string    `json:"path"`
	Operation   Operation `json:"operation"`
	SizeBytes   int64     `json:"size_bytes"`
}

type Receipt struct {
	ReservationID  string `json:"reservation_id"`
	State          string `json:"state"`
	BaseRevision   int64  `json:"base_revision"`
	ResultRevision *int64 `json:"result_revision,omitempty"`
}

type Service interface {
	Reserve(context.Context, string, ReserveRequest) (Receipt, error)
	Settle(context.Context, string, string) (Receipt, error)
	Abort(context.Context, string, string) (Receipt, error)
}

type Handler struct {
	ownerID string
	service Service
}

func NewHandler(ownerID string, service Service) (http.Handler, error) {
	if !domain.ValidUserID(ownerID) || service == nil {
		return nil, errors.New("sync node quota handler requires a valid owner and service")
	}
	return &Handler{ownerID: ownerID, service: service}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, domain.CodeInvalidArgument, "query parameters are not supported")
		return
	}
	if r.URL.Path == "/v1/reservations" {
		h.reserve(w, r)
		return
	}
	const prefix = "/v1/reservations/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeError(w, domain.CodeNotFound, "endpoint not found")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) != 2 || !reservationIDPattern.MatchString(parts[0]) {
		writeError(w, domain.CodeNotFound, "endpoint not found")
		return
	}
	switch parts[1] {
	case "settle":
		h.settle(w, r, parts[0])
	case "abort":
		h.abort(w, r, parts[0])
	default:
		writeError(w, domain.CodeNotFound, "endpoint not found")
	}
}

func (h *Handler) reserve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request ReserveRequest
	if err := decodeRequest(w, r, &request); err != nil {
		if errors.Is(err, errRequestTooLarge) {
			writeErrorStatus(w, http.StatusRequestEntityTooLarge, domain.CodeRequestTooLarge, "request body is too large")
			return
		}
		writeError(w, domain.CodeInvalidArgument, "request body is invalid")
		return
	}
	if err := validateReserveRequest(request); err != nil {
		writeError(w, domain.CodeInvalidArgument, "reservation fields are invalid")
		return
	}
	receipt, err := h.service.Reserve(r.Context(), h.ownerID, request)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if !validReceipt(receipt) {
		writeError(w, domain.CodeInternal, "reservation service returned an invalid receipt")
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (h *Handler) settle(w http.ResponseWriter, r *http.Request, reservationID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !emptyBody(r) {
		writeError(w, domain.CodeInvalidArgument, "request body must be empty")
		return
	}
	receipt, err := h.service.Settle(r.Context(), h.ownerID, reservationID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if !validReceipt(receipt) {
		writeError(w, domain.CodeInternal, "reservation service returned an invalid receipt")
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (h *Handler) abort(w http.ResponseWriter, r *http.Request, reservationID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !emptyBody(r) {
		writeError(w, domain.CodeInvalidArgument, "request body must be empty")
		return
	}
	receipt, err := h.service.Abort(r.Context(), h.ownerID, reservationID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if !validReceipt(receipt) {
		writeError(w, domain.CodeInternal, "reservation service returned an invalid receipt")
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func validateReserveRequest(value ReserveRequest) error {
	if !domain.ValidWorkspaceID(value.WorkspaceID) || !workspace.ValidSyncFolderID(value.FolderID) || !operationIDPattern.MatchString(value.OperationID) ||
		workspace.ValidateRelativePath(value.Path) != nil || value.SizeBytes < 0 || value.SizeBytes > workspace.MaxSyncFileBytes {
		return errors.New("invalid reservation identity, path, or size")
	}
	switch value.Operation {
	case OperationUpsert:
	case OperationDelete:
		if value.SizeBytes != 0 {
			return errors.New("deletion size must be zero")
		}
	case OperationDirectoryUpsert, OperationDirectoryDelete:
		if value.SizeBytes != 0 {
			return errors.New("directory operation size must be zero")
		}
	default:
		return errors.New("invalid operation")
	}
	return nil
}

func validReceipt(value Receipt) bool {
	return reservationIDPattern.MatchString(value.ReservationID) && value.State != "" && len(value.State) <= 32 &&
		value.BaseRevision >= 0 && (value.ResultRevision == nil || *value.ResultRevision >= 0)
}

var errRequestTooLarge = errors.New("request body exceeds limit")

func decodeRequest(w http.ResponseWriter, r *http.Request, value any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("request content type must be application/json")
	}
	reader := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errRequestTooLarge
		}
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errRequestTooLarge
		}
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func emptyBody(r *http.Request) bool {
	if r.ContentLength > 0 {
		return false
	}
	if r.Body == nil {
		return true
	}
	var one [1]byte
	n, err := r.Body.Read(one[:])
	return n == 0 && err == io.EOF
}

type errorResponse struct {
	Code    domain.ErrorCode `json:"code"`
	Message string           `json:"message"`
}

func writeServiceError(w http.ResponseWriter, err error) {
	code := domain.CodeOf(err)
	status := http.StatusInternalServerError
	switch code {
	case domain.CodeInvalidArgument:
		status = http.StatusBadRequest
	case domain.CodeUnauthenticated:
		status = http.StatusUnauthorized
	case domain.CodeForbidden:
		status = http.StatusForbidden
	case domain.CodeNotFound:
		status = http.StatusNotFound
	case domain.CodeRequestTooLarge:
		status = http.StatusRequestEntityTooLarge
	case domain.CodeRateLimited:
		status = http.StatusTooManyRequests
	case domain.CodeQuotaExceeded, domain.CodeCapacityExceeded, domain.CodeWorkspaceBusy, domain.CodeWorkspaceConflict, domain.CodeInvalidState,
		domain.CodeConflict, domain.CodeIdempotencyConflict, domain.CodeTransferExpired:
		status = http.StatusConflict
	}
	writeErrorStatus(w, status, code, safeErrorMessage(code))
}

func safeErrorMessage(code domain.ErrorCode) string {
	switch code {
	case domain.CodeInvalidArgument:
		return "request is invalid"
	case domain.CodeUnauthenticated:
		return "request is not authorized"
	case domain.CodeForbidden:
		return "request is not allowed"
	case domain.CodeNotFound:
		return "reservation was not found"
	case domain.CodeQuotaExceeded:
		return "workspace quota was exceeded"
	case domain.CodeCapacityExceeded:
		return "workspace storage capacity was exceeded"
	case domain.CodeWorkspaceBusy:
		return "workspace is busy"
	case domain.CodeWorkspaceConflict:
		return "workspace changed"
	case domain.CodeInvalidState:
		return "reservation state is invalid"
	case domain.CodeIdempotencyConflict:
		return "operation identifier was reused"
	case domain.CodeRequestTooLarge:
		return "request body is too large"
	case domain.CodeRateLimited:
		return "request is rate limited"
	default:
		return "reservation request failed"
	}
}

func writeError(w http.ResponseWriter, code domain.ErrorCode, message string) {
	status := http.StatusBadRequest
	if code == domain.CodeNotFound {
		status = http.StatusNotFound
	}
	writeErrorStatus(w, status, code, message)
}

func writeErrorStatus(w http.ResponseWriter, status int, code domain.ErrorCode, message string) {
	writeJSON(w, status, errorResponse{Code: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeErrorStatus(w, http.StatusMethodNotAllowed, domain.CodeInvalidArgument, "method is not allowed")
}
