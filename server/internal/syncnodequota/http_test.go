package syncnodequota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"morde/server/internal/domain"
)

type fakeService struct {
	owner       string
	request     ReserveRequest
	reserveCall int
	settleID    string
	settleCall  int
	abortID     string
	abortCall   int
	reserveErr  error
	settleErr   error
	abortErr    error
	abortState  string
}

func (s *fakeService) Reserve(_ context.Context, owner string, request ReserveRequest) (Receipt, error) {
	s.owner, s.request = owner, request
	s.reserveCall++
	return Receipt{ReservationID: "r1234567", State: "reserved", BaseRevision: 4}, s.reserveErr
}

func (s *fakeService) Settle(_ context.Context, owner, reservationID string) (Receipt, error) {
	s.owner, s.settleID = owner, reservationID
	s.settleCall++
	return Receipt{ReservationID: reservationID, State: "settled", BaseRevision: 4}, s.settleErr
}

func (s *fakeService) Abort(_ context.Context, owner, reservationID string) (Receipt, error) {
	s.owner, s.abortID = owner, reservationID
	s.abortCall++
	state := s.abortState
	if state == "" {
		state = "aborted"
	}
	return Receipt{ReservationID: reservationID, State: state, BaseRevision: 4}, s.abortErr
}

func TestHandlerBindsOwnerAndReservesValidatedRelativePath(t *testing.T) {
	service := &fakeService{}
	handler, err := NewHandler("u1234567", service)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/reservations", strings.NewReader(`{"workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"sha256-abc","path":"src/file.txt","operation":"upsert","size_bytes":8}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.reserveCall != 1 || service.owner != "u1234567" {
		t.Fatalf("reserve response = %d, calls=%d, owner=%q", response.Code, service.reserveCall, service.owner)
	}
	if service.request.FolderID != "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || service.request.Path != "src/file.txt" || service.request.Operation != OperationUpsert || service.request.SizeBytes != 8 {
		t.Fatalf("reserve request = %+v", service.request)
	}
	if !strings.Contains(response.Body.String(), `"reservation_id":"r1234567"`) {
		t.Fatalf("receipt response = %q", response.Body.String())
	}
}

func TestHandlerRejectsInvalidReservationBeforeCallingService(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"absolute path", `{"workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"id1","path":"/etc/passwd","operation":"upsert","size_bytes":1}`},
		{"missing folder id", `{"workspace_id":"w1234567","operation_id":"id1","path":"file","operation":"upsert","size_bytes":1}`},
		{"invalid folder id", `{"workspace_id":"w1234567","folder_id":"morde-bad","operation_id":"id1","path":"file","operation":"upsert","size_bytes":1}`},
		{"traversal", `{"workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"id1","path":"../escape","operation":"upsert","size_bytes":1}`},
		{"invalid operation id", `{"workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"bad/id","path":"file","operation":"upsert","size_bytes":1}`},
		{"unknown owner field", `{"owner_id":"u7654321","workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"id1","path":"file","operation":"upsert","size_bytes":1}`},
		{"delete with size", `{"workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"id1","path":"file","operation":"delete","size_bytes":1}`},
		{"over single-file limit", `{"workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"id1","path":"file","operation":"upsert","size_bytes":314572801}`},
		{"trailing JSON", `{"workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"id1","path":"file","operation":"upsert","size_bytes":1}{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeService{}
			handler, err := NewHandler("u1234567", service)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/reservations", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || service.reserveCall != 0 {
				t.Fatalf("invalid reservation response = %d, calls=%d", response.Code, service.reserveCall)
			}
		})
	}
}

func TestHandlerAcceptsDirectoryReservationsWithoutByteQuota(t *testing.T) {
	for _, operation := range []Operation{OperationDirectoryUpsert, OperationDirectoryDelete} {
		request := ReserveRequest{
			WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", OperationID: "directory-op",
			Path: "empty/archive", Operation: operation,
		}
		if err := validateReserveRequest(request); err != nil {
			t.Fatalf("validateReserveRequest(%q) = %v", operation, err)
		}
		request.SizeBytes = 1
		if err := validateReserveRequest(request); err == nil {
			t.Fatalf("validateReserveRequest(%q) accepted directory bytes", operation)
		}
	}
}

func TestHandlerRejectsLargeBodiesAndQueryParameters(t *testing.T) {
	for _, target := range []string{"/v1/reservations", "/v1/reservations?owner_id=u7654321"} {
		service := &fakeService{}
		handler, err := NewHandler("u1234567", service)
		if err != nil {
			t.Fatal(err)
		}
		body := `{"workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"id1","path":"file","operation":"upsert","size_bytes":1}`
		if target == "/v1/reservations" {
			body = strings.Repeat(" ", maxRequestBytes+1)
		}
		request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := http.StatusBadRequest
		if target == "/v1/reservations" {
			want = http.StatusRequestEntityTooLarge
		}
		if response.Code != want || service.reserveCall != 0 {
			t.Fatalf("request %q response = %d, calls=%d", target, response.Code, service.reserveCall)
		}
	}
}

func TestHandlerSettlesAndAbortsUsingFixedOwner(t *testing.T) {
	service := &fakeService{}
	handler, err := NewHandler("u1234567", service)
	if err != nil {
		t.Fatal(err)
	}
	settle := httptest.NewRequest(http.MethodPost, "/v1/reservations/r1234567/settle", nil)
	settleResponse := httptest.NewRecorder()
	handler.ServeHTTP(settleResponse, settle)
	if settleResponse.Code != http.StatusOK || service.settleCall != 1 || service.settleID != "r1234567" || service.owner != "u1234567" {
		t.Fatalf("settle response = %d, calls=%d, id=%q, owner=%q", settleResponse.Code, service.settleCall, service.settleID, service.owner)
	}
	abort := httptest.NewRequest(http.MethodPost, "/v1/reservations/r1234567/abort", nil)
	abortResponse := httptest.NewRecorder()
	handler.ServeHTTP(abortResponse, abort)
	if abortResponse.Code != http.StatusOK || service.abortCall != 1 || service.abortID != "r1234567" || service.owner != "u1234567" || !strings.Contains(abortResponse.Body.String(), `"state":"aborted"`) {
		t.Fatalf("abort response = %d, calls=%d, id=%q, owner=%q", abortResponse.Code, service.abortCall, service.abortID, service.owner)
	}
}

func TestHandlerDoesNotExposeInternalServiceError(t *testing.T) {
	service := &fakeService{reserveErr: domain.NewError(domain.CodeQuotaExceeded, "database secret detail")}
	handler, err := NewHandler("u1234567", service)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/reservations", strings.NewReader(`{"workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"id1","path":"file","operation":"upsert","size_bytes":1}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "database secret detail") {
		t.Fatalf("service error response = %d %q", response.Code, response.Body.String())
	}
}

func TestHandlerRequiresServiceAndOwner(t *testing.T) {
	if _, err := NewHandler("bad", &fakeService{}); err == nil {
		t.Fatal("invalid owner accepted")
	}
	if _, err := NewHandler("u1234567", nil); err == nil {
		t.Fatal("nil service accepted")
	}
}

func TestHandlerPropagatesServiceErrorCode(t *testing.T) {
	service := &fakeService{settleErr: domain.NewError(domain.CodeWorkspaceBusy, "busy")}
	handler, err := NewHandler("u1234567", service)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/reservations/r1234567/settle", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), string(domain.CodeWorkspaceBusy)) {
		t.Fatalf("busy response = %d %q", response.Code, response.Body.String())
	}
	service.settleErr = errors.New("internal details")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "internal details") {
		t.Fatalf("internal response = %d %q", response.Code, response.Body.String())
	}
}
