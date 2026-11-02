package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type personalAttachmentStoreStub struct {
	createCalls         int
	activateCalls       int
	chunkCalls          int
	commitCalls         int
	cancelCalls         int
	openCalls           int
	cleanupCalls        int
	principal           identity.Principal
	upload              personal.AttachmentUpload
	idempotencyKey      string
	reservationID       string
	chunk               personal.AttachmentChunk
	chunkBody           string
	chunkReadBytes      int
	commitUploadID      string
	commitKey           string
	cancelUploadID      string
	openAttachmentID    string
	cleanupNow          time.Time
	cleanupLimit        int32
	createReceipt       personal.AttachmentUploadReceipt
	activateReceipt     personal.AttachmentUploadReceipt
	chunkReceipt        personal.AttachmentChunkReceipt
	commitReceipt       personal.AttachmentCommitReceipt
	cancelReservation   personal.AttachmentReservation
	openReader          io.ReadCloser
	openMetadata        personal.AttachmentMetadata
	cleanupReservations []personal.AttachmentReservation
	acknowledged        personal.AttachmentReservation
	createErr           error
	activateErr         error
	chunkErr            error
	commitErr           error
	cancelErr           error
	openErr             error
	cleanupErr          error
	acknowledgeErr      error
	acknowledgeCalls    int
}

func (s *personalAttachmentStoreStub) CreatePersonalAttachmentUpload(_ context.Context, principal identity.Principal, upload personal.AttachmentUpload, key string) (personal.AttachmentUploadReceipt, error) {
	s.createCalls++
	s.principal, s.upload, s.idempotencyKey = principal, upload, key
	if s.createErr != nil {
		return personal.AttachmentUploadReceipt{}, s.createErr
	}
	if s.createReceipt.UploadID == "" {
		count, _ := personal.AttachmentChunkCount(upload.SizeBytes)
		return personal.AttachmentUploadReceipt{UploadID: upload.UploadID, State: personal.AttachmentUploadPending, ChunkCount: count}, nil
	}
	return s.createReceipt, nil
}

func (s *personalAttachmentStoreStub) ActivatePersonalAttachmentUpload(_ context.Context, principal identity.Principal, uploadID, reservationID string) (personal.AttachmentUploadReceipt, error) {
	s.activateCalls++
	s.principal, s.commitUploadID, s.reservationID = principal, uploadID, reservationID
	if s.activateErr != nil {
		return personal.AttachmentUploadReceipt{}, s.activateErr
	}
	if s.activateReceipt.UploadID == "" {
		return personal.AttachmentUploadReceipt{
			UploadID: uploadID, ReservationID: reservationID, State: personal.AttachmentUploadActive,
			ChunkCount: attachmentChunkCount(s.upload.SizeBytes),
		}, nil
	}
	return s.activateReceipt, nil
}

func (s *personalAttachmentStoreStub) PutPersonalAttachmentChunk(_ context.Context, principal identity.Principal, chunk personal.AttachmentChunk, body io.Reader) (personal.AttachmentChunkReceipt, error) {
	s.chunkCalls++
	s.principal, s.chunk = principal, chunk
	if body != nil {
		data, err := io.ReadAll(body)
		if err != nil {
			return personal.AttachmentChunkReceipt{}, err
		}
		s.chunkBody = string(data)
		s.chunkReadBytes = len(data)
	}
	if s.chunkErr != nil {
		return personal.AttachmentChunkReceipt{}, s.chunkErr
	}
	if s.chunkReceipt.UploadID == "" {
		return personal.AttachmentChunkReceipt{UploadID: chunk.UploadID, Sequence: chunk.Sequence}, nil
	}
	return s.chunkReceipt, nil
}

func (s *personalAttachmentStoreStub) CommitPersonalAttachment(_ context.Context, principal identity.Principal, uploadID, key string) (personal.AttachmentCommitReceipt, error) {
	s.commitCalls++
	s.principal, s.commitUploadID, s.commitKey = principal, uploadID, key
	return s.commitReceipt, s.commitErr
}

func (s *personalAttachmentStoreStub) CancelPersonalAttachmentUpload(_ context.Context, principal identity.Principal, uploadID string) (personal.AttachmentReservation, error) {
	s.cancelCalls++
	s.principal, s.cancelUploadID = principal, uploadID
	return s.cancelReservation, s.cancelErr
}

func (s *personalAttachmentStoreStub) OpenPersonalAttachment(_ context.Context, principal identity.Principal, attachmentID string) (io.ReadCloser, personal.AttachmentMetadata, error) {
	s.openCalls++
	s.principal, s.openAttachmentID = principal, attachmentID
	return s.openReader, s.openMetadata, s.openErr
}

func (s *personalAttachmentStoreStub) CleanupExpiredPersonalAttachmentUploads(_ context.Context, now time.Time, limit int32) ([]personal.AttachmentReservation, error) {
	s.cleanupCalls++
	s.cleanupNow, s.cleanupLimit = now, limit
	return s.cleanupReservations, s.cleanupErr
}

func (s *personalAttachmentStoreStub) AcknowledgePersonalAttachmentReservationReleased(_ context.Context, reservation personal.AttachmentReservation) error {
	s.acknowledgeCalls++
	s.acknowledged = reservation
	return s.acknowledgeErr
}

type personalAttachmentQuotaStub struct {
	reserveCalls  int
	releaseCalls  int
	principal     identity.Principal
	uploadID      string
	sizeBytes     int64
	ownerID       string
	reservationID string
	reserveErr    error
	releaseErr    error
}

func (s *personalAttachmentQuotaStub) ReservePersonalAttachment(_ context.Context, principal identity.Principal, uploadID string, sizeBytes int64) (string, error) {
	s.reserveCalls++
	s.principal, s.uploadID, s.sizeBytes = principal, uploadID, sizeBytes
	if s.reservationID == "" {
		s.reservationID = "r7654321"
	}
	return s.reservationID, s.reserveErr
}

func (s *personalAttachmentQuotaStub) ReleasePersonalAttachmentReservation(_ context.Context, ownerID, reservationID string) error {
	s.releaseCalls++
	s.ownerID, s.reservationID = ownerID, reservationID
	return s.releaseErr
}

func personalAttachmentPrincipal(kind identity.Kind) identity.Principal {
	value := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: kind, TokenID: "t1234567"}
	if kind == identity.KindClient {
		value.ClientInstallationID = "i1234567"
	}
	return value
}

func TestPersonalAttachmentServiceCreateDerivesOwnerAndActivatesReservation(t *testing.T) {
	store := &personalAttachmentStoreStub{}
	quota := &personalAttachmentQuotaStub{}
	service, err := NewPersonalAttachmentService(store, quota)
	if err != nil {
		t.Fatal(err)
	}
	principal := personalAttachmentPrincipal(identity.KindClient)
	command := PersonalAttachmentUploadCommand{
		IdempotencyKey: "library-upload-001", ObjectID: "library-entry-1", BaseRevision: 3,
		SizeBytes: 5, SHA256: strings.Repeat("a", 64), MediaType: "image/png",
	}
	receipt, err := service.CreatePersonalAttachmentUpload(context.Background(), principal, command)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != personal.AttachmentUploadActive || receipt.UploadID != store.upload.UploadID ||
		receipt.ReservationID != quota.reservationID || receipt.ChunkCount != 1 || store.createCalls != 1 ||
		store.activateCalls != 1 || quota.reserveCalls != 1 || store.upload.OwnerID != principal.UserID ||
		store.upload.ObjectType != personal.ObjectTypeLibraryItem || store.upload.ObjectID != command.ObjectID ||
		store.upload.BaseRevision != command.BaseRevision || store.idempotencyKey != command.IdempotencyKey ||
		len(store.upload.RequestHash) != 32 || quota.uploadID != store.upload.UploadID || quota.sizeBytes != command.SizeBytes {
		t.Fatalf("create receipt=%+v upload=%+v quota=%+v store=%+v", receipt, store.upload, quota, store)
	}
}

func TestPersonalAttachmentServiceCreateReplaysActiveSessionAndConflicts(t *testing.T) {
	store := &personalAttachmentStoreStub{createReceipt: personal.AttachmentUploadReceipt{
		UploadID: "pu_0123456789abcdef0123456789abcdef", ReservationID: "r7654321", State: personal.AttachmentUploadActive, ChunkCount: 1, Replay: true,
	}}
	quota := &personalAttachmentQuotaStub{}
	service, err := NewPersonalAttachmentService(store, quota)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := service.CreatePersonalAttachmentUpload(context.Background(), personalAttachmentPrincipal(identity.KindSession), PersonalAttachmentUploadCommand{
		IdempotencyKey: "library-upload-001", ObjectID: "library-entry-1", SizeBytes: 5,
		SHA256: strings.Repeat("a", 64), MediaType: "image/png",
	})
	if err != nil || !receipt.Replay || quota.reserveCalls != 0 || store.activateCalls != 0 {
		t.Fatalf("active upload replay = %+v, %v; quota calls=%d activation calls=%d", receipt, err, quota.reserveCalls, store.activateCalls)
	}
	store.createReceipt.State = personal.AttachmentUploadCommitted
	if committed, err := service.CreatePersonalAttachmentUpload(context.Background(), personalAttachmentPrincipal(identity.KindSession), PersonalAttachmentUploadCommand{
		IdempotencyKey: "library-upload-001", ObjectID: "library-entry-1", SizeBytes: 5,
		SHA256: strings.Repeat("a", 64), MediaType: "image/png",
	}); err != nil || committed.State != personal.AttachmentUploadCommitted || quota.reserveCalls != 0 || store.activateCalls != 0 {
		t.Fatalf("committed upload replay = %+v, %v; quota calls=%d activation calls=%d", committed, err, quota.reserveCalls, store.activateCalls)
	}

	store.createErr = domain.NewError(domain.CodeIdempotencyConflict, "upload key conflict")
	if _, err := service.CreatePersonalAttachmentUpload(context.Background(), personalAttachmentPrincipal(identity.KindSession), PersonalAttachmentUploadCommand{
		IdempotencyKey: "library-upload-001", ObjectID: "library-entry-1", SizeBytes: 5,
		SHA256: strings.Repeat("b", 64), MediaType: "image/png",
	}); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("create key conflict error = %v", err)
	}
}

func TestPersonalAttachmentServiceFailsClosedWithoutQuotaAndOnQuotaRejection(t *testing.T) {
	if _, err := NewPersonalAttachmentService(&personalAttachmentStoreStub{}, nil); err == nil {
		t.Fatal("service accepted missing DATA-05 quota port")
	}
	store := &personalAttachmentStoreStub{}
	quota := &personalAttachmentQuotaStub{reserveErr: domain.NewError(domain.CodeQuotaExceeded, "quota exceeded")}
	service, err := NewPersonalAttachmentService(store, quota)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreatePersonalAttachmentUpload(context.Background(), personalAttachmentPrincipal(identity.KindSession), PersonalAttachmentUploadCommand{
		IdempotencyKey: "library-upload-001", ObjectID: "library-entry-1", SizeBytes: 5,
		SHA256: strings.Repeat("a", 64), MediaType: "image/png",
	})
	if domain.CodeOf(err) != domain.CodeQuotaExceeded || store.createCalls != 1 || store.activateCalls != 0 {
		t.Fatalf("quota rejection error=%v create=%d activate=%d", err, store.createCalls, store.activateCalls)
	}
	if _, err := service.CreatePersonalAttachmentUpload(context.Background(), personalAttachmentPrincipal(identity.KindSession), PersonalAttachmentUploadCommand{
		IdempotencyKey: "library-upload-002", ObjectID: "library-entry-1", SizeBytes: personal.MaxAttachmentBytes + 1,
		SHA256: strings.Repeat("a", 64), MediaType: "image/png",
	}); domain.CodeOf(err) != domain.CodeRequestTooLarge || store.createCalls != 1 {
		t.Fatalf("oversized request error=%v create calls=%d", err, store.createCalls)
	}
}

func TestPersonalAttachmentServiceCreateResumesPendingReservationAndRejectsInvalidPrincipals(t *testing.T) {
	store := &personalAttachmentStoreStub{createReceipt: personal.AttachmentUploadReceipt{
		UploadID: "pu_0123456789abcdef0123456789abcdef", State: personal.AttachmentUploadPending, ChunkCount: 1,
	}}
	quota := &personalAttachmentQuotaStub{}
	service, err := NewPersonalAttachmentService(store, quota)
	if err != nil {
		t.Fatal(err)
	}
	principal := personalAttachmentPrincipal(identity.KindSession)
	if _, err := service.CreatePersonalAttachmentUpload(context.Background(), principal, PersonalAttachmentUploadCommand{
		IdempotencyKey: "library-upload-001", ObjectID: "library-entry-1", SizeBytes: 5,
		SHA256: strings.Repeat("a", 64), MediaType: "image/png",
	}); err != nil {
		t.Fatalf("pending upload did not resume: %v", err)
	}
	for _, denied := range []identity.Principal{
		{UserID: principal.UserID, Role: identity.RoleUser, Kind: identity.KindAPIKey},
		{UserID: principal.UserID, Role: identity.RoleUser, Kind: identity.KindDevice},
		{UserID: principal.UserID, Role: identity.RoleUser, Kind: identity.KindHostCookie, HostScope: "h1234567"},
		{UserID: "invalid", Role: identity.RoleUser, Kind: identity.KindSession},
	} {
		if _, err := service.CreatePersonalAttachmentUpload(context.Background(), denied, PersonalAttachmentUploadCommand{
			IdempotencyKey: "library-upload-002", ObjectID: "library-entry-2", SizeBytes: 5,
			SHA256: strings.Repeat("a", 64), MediaType: "image/png",
		}); err == nil {
			t.Errorf("create accepted invalid principal %+v", denied)
		}
	}
	if store.createCalls != 1 {
		t.Fatalf("invalid principal reached store %d times", store.createCalls)
	}
}

func TestPersonalAttachmentServiceChunkValidationAndConflict(t *testing.T) {
	store := &personalAttachmentStoreStub{}
	service, err := NewPersonalAttachmentService(store, &personalAttachmentQuotaStub{})
	if err != nil {
		t.Fatal(err)
	}
	principal := personalAttachmentPrincipal(identity.KindSession)
	command := PersonalAttachmentChunkCommand{
		UploadID: "pu_0123456789abcdef0123456789abcdef", Sequence: 2, SizeBytes: 5, SHA256: strings.Repeat("a", 64), Body: strings.NewReader("hello"),
	}
	receipt, err := service.PutPersonalAttachmentChunk(context.Background(), principal, command)
	if err != nil || receipt.UploadID != command.UploadID || receipt.Sequence != command.Sequence || store.chunkBody != "hello" {
		t.Fatalf("chunk receipt=%+v err=%v stored body=%q", receipt, err, store.chunkBody)
	}
	before := store.chunkCalls
	command.SizeBytes = personal.MaxAttachmentChunkBytes + 1
	if _, err := service.PutPersonalAttachmentChunk(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeInvalidArgument || store.chunkCalls != before {
		t.Fatalf("oversized chunk error=%v store calls=%d", err, store.chunkCalls)
	}
	store.chunkErr = domain.NewError(domain.CodeConflict, "chunk sequence conflicts")
	command.SizeBytes = 5
	if _, err := service.PutPersonalAttachmentChunk(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("chunk conflict error = %v", err)
	}
}

func TestPersonalAttachmentServiceBoundsChunkReader(t *testing.T) {
	store := &personalAttachmentStoreStub{}
	service, err := NewPersonalAttachmentService(store, &personalAttachmentQuotaStub{})
	if err != nil {
		t.Fatal(err)
	}
	command := PersonalAttachmentChunkCommand{
		UploadID: "pu_0123456789abcdef0123456789abcdef", Sequence: 0,
		SizeBytes: personal.MaxAttachmentChunkBytes, SHA256: strings.Repeat("a", 64),
		Body: strings.NewReader(strings.Repeat("x", int(personal.MaxAttachmentChunkBytes)+32)),
	}
	if _, err := service.PutPersonalAttachmentChunk(context.Background(), personalAttachmentPrincipal(identity.KindSession), command); err != nil {
		t.Fatal(err)
	}
	if store.chunkReadBytes != int(personal.MaxAttachmentChunkBytes+1) {
		t.Fatalf("store consumed %d bytes, want at most chunk limit plus one", store.chunkReadBytes)
	}
}

func TestPersonalAttachmentServiceCommitReturnsMinimalReceipt(t *testing.T) {
	store := &personalAttachmentStoreStub{commitReceipt: personal.AttachmentCommitReceipt{
		AttachmentID: "pa_0123456789abcdef0123456789abcdef", ObjectType: personal.ObjectTypeLibraryItem, ObjectID: "library-entry-1",
		ResultRevision: 3, AccountChangeSequence: 9, Replay: true,
	}}
	service, err := NewPersonalAttachmentService(store, &personalAttachmentQuotaStub{})
	if err != nil {
		t.Fatal(err)
	}
	principal := personalAttachmentPrincipal(identity.KindClient)
	receipt, err := service.CommitPersonalAttachment(context.Background(), principal, "pu_0123456789abcdef0123456789abcdef", "library-commit-01")
	if err != nil || receipt != store.commitReceipt || store.commitUploadID != "pu_0123456789abcdef0123456789abcdef" || store.commitKey != "library-commit-01" {
		t.Fatalf("commit receipt=%+v err=%v", receipt, err)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil || strings.Contains(string(encoded), "body") {
		t.Fatalf("commit receipt contains body: %s err=%v", encoded, err)
	}
}

func TestPersonalAttachmentServiceQuotaRejectionReleasesReservation(t *testing.T) {
	principal := personalAttachmentPrincipal(identity.KindClient)
	store := &personalAttachmentStoreStub{
		commitErr: domain.NewError(domain.CodeQuotaExceeded, "personal data quota exceeded"),
		cancelReservation: personal.AttachmentReservation{
			OwnerID: principal.UserID, UploadID: "pu_0123456789abcdef0123456789abcdef", ReservationID: "r7654321", SizeBytes: 10,
		},
	}
	quota := &personalAttachmentQuotaStub{}
	service, err := NewPersonalAttachmentService(store, quota)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CommitPersonalAttachment(context.Background(), principal, "pu_0123456789abcdef0123456789abcdef", "library-commit-01")
	if domain.CodeOf(err) != domain.CodeQuotaExceeded || store.cancelCalls != 1 ||
		quota.releaseCalls != 1 || store.acknowledgeCalls != 1 {
		t.Fatalf("quota rejection error=%v cancel=%d release=%d ack=%d", err, store.cancelCalls, quota.releaseCalls, store.acknowledgeCalls)
	}
}

func TestPersonalAttachmentServiceCancelHandlesUnreservedAndReleasesIdempotently(t *testing.T) {
	store := &personalAttachmentStoreStub{}
	quota := &personalAttachmentQuotaStub{}
	service, err := NewPersonalAttachmentService(store, quota)
	if err != nil {
		t.Fatal(err)
	}
	principal := personalAttachmentPrincipal(identity.KindSession)
	store.cancelReservation = personal.AttachmentReservation{OwnerID: principal.UserID, UploadID: "pu_0123456789abcdef0123456789abcdef"}
	if err := service.CancelPersonalAttachmentUpload(context.Background(), principal, "pu_0123456789abcdef0123456789abcdef"); err != nil || quota.releaseCalls != 0 {
		t.Fatalf("cancel pending upload error=%v release calls=%d", err, quota.releaseCalls)
	}
	store.cancelReservation = personal.AttachmentReservation{
		OwnerID: principal.UserID, UploadID: "pu_0123456789abcdef0123456789abcdef", ReservationID: "r7654321", SizeBytes: 10,
	}
	quota.releaseErr = errors.New("temporary quota outage")
	if err := service.CancelPersonalAttachmentUpload(context.Background(), principal, "pu_0123456789abcdef0123456789abcdef"); err == nil || quota.releaseCalls != 1 {
		t.Fatalf("cancel reservation error=%v release calls=%d", err, quota.releaseCalls)
	}
	quota.releaseErr = nil
	store.acknowledgeErr = errors.New("temporary store outage")
	if err := service.CancelPersonalAttachmentUpload(context.Background(), principal, "pu_0123456789abcdef0123456789abcdef"); err == nil || quota.releaseCalls != 2 || store.acknowledgeCalls != 1 {
		t.Fatalf("cancel acknowledgement error=%v release calls=%d acknowledge calls=%d", err, quota.releaseCalls, store.acknowledgeCalls)
	}
	store.acknowledgeErr = nil
	if err := service.CancelPersonalAttachmentUpload(context.Background(), principal, "pu_0123456789abcdef0123456789abcdef"); err != nil || quota.releaseCalls != 3 || store.acknowledgeCalls != 2 {
		t.Fatalf("cancel retry error=%v release calls=%d", err, quota.releaseCalls)
	}
}

func TestPersonalAttachmentServiceOpenStreamsAuthorizedMetadata(t *testing.T) {
	reader := io.NopCloser(strings.NewReader("attachment bytes"))
	store := &personalAttachmentStoreStub{
		openReader: reader,
		openMetadata: personal.AttachmentMetadata{
			AttachmentID: "pa_0123456789abcdef0123456789abcdef", ObjectType: personal.ObjectTypeLibraryItem, ObjectID: "library-entry-1",
			MediaType: "application/octet-stream", SizeBytes: 16, SHA256: strings.Repeat("a", 64),
		},
	}
	service, err := NewPersonalAttachmentService(store, &personalAttachmentQuotaStub{})
	if err != nil {
		t.Fatal(err)
	}
	principal := personalAttachmentPrincipal(identity.KindClient)
	gotReader, metadata, err := service.OpenPersonalAttachment(context.Background(), principal, "pa_0123456789abcdef0123456789abcdef")
	if err != nil || gotReader != reader || metadata.AttachmentID != "pa_0123456789abcdef0123456789abcdef" || !reflect.DeepEqual(store.principal, principal) {
		t.Fatalf("open reader=%v metadata=%+v err=%v", gotReader, metadata, err)
	}
	data, err := io.ReadAll(gotReader)
	if err != nil || string(data) != "attachment bytes" {
		t.Fatalf("streamed data=%q err=%v", data, err)
	}
	if _, _, err := service.OpenPersonalAttachment(context.Background(), identity.Principal{UserID: principal.UserID, Kind: identity.KindDevice}, "pa_0123456789abcdef0123456789abcdef"); err == nil || store.openCalls != 1 {
		t.Fatalf("device open error=%v store calls=%d", err, store.openCalls)
	}
}

func TestPersonalAttachmentServiceCleanupRetriesReservationRelease(t *testing.T) {
	store := &personalAttachmentStoreStub{cleanupReservations: []personal.AttachmentReservation{{
		OwnerID: "u1234567", UploadID: "pu_0123456789abcdef0123456789abcdef", ReservationID: "r7654321", SizeBytes: 20,
	}}}
	quota := &personalAttachmentQuotaStub{releaseErr: errors.New("temporary release failure")}
	service, err := NewPersonalAttachmentService(store, quota)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if _, err := service.CleanupExpiredPersonalAttachmentUploads(context.Background(), now, 10); err == nil ||
		store.cleanupCalls != 1 || !store.cleanupNow.Equal(now) || store.cleanupLimit != 10 || quota.releaseCalls != 1 {
		t.Fatalf("cleanup error=%v store=%+v quota=%+v", err, store, quota)
	}
	quota.releaseErr = nil
	store.acknowledgeErr = errors.New("temporary store acknowledgement failure")
	if count, err := service.CleanupExpiredPersonalAttachmentUploads(context.Background(), now, 10); err == nil || count != 0 || quota.releaseCalls != 2 || store.acknowledgeCalls != 1 {
		t.Fatalf("cleanup acknowledgement count=%d err=%v release calls=%d acknowledge calls=%d", count, err, quota.releaseCalls, store.acknowledgeCalls)
	}
	store.acknowledgeErr = nil
	if count, err := service.CleanupExpiredPersonalAttachmentUploads(context.Background(), now, 10); err != nil || count != 1 || quota.releaseCalls != 3 || store.acknowledgeCalls != 2 {
		t.Fatalf("cleanup retry count=%d err=%v release calls=%d acknowledge calls=%d", count, err, quota.releaseCalls, store.acknowledgeCalls)
	}
	if _, err := service.CleanupExpiredPersonalAttachmentUploads(context.Background(), now, maxPersonalAttachmentCleanupLimit+1); domain.CodeOf(err) != domain.CodeInvalidArgument || store.cleanupCalls != 3 {
		t.Fatalf("invalid cleanup limit error=%v store calls=%d", err, store.cleanupCalls)
	}
}
