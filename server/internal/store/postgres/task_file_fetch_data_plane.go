package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

type taskFileFetchTransferAuthorization struct {
	Fetch           app.TaskFileFetch
	OperationStatus task.OperationStatus
	TicketHash      []byte
	TicketIsLive    bool
}

func (s *Store) AcceptTaskFileFetchManifest(ctx context.Context, taskID, fetchID string, ticketHash []byte,
	request app.TaskFileFetchManifestRequest) (app.TaskFileFetchManifestReceipt, error) {
	if ctx == nil || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) || len(ticketHash) != sha256.Size ||
		!validPostgresSHA256(request.SourceVersion) || !validPostgresSHA256(request.SourceSHA256) ||
		request.SizeBytes < 1 || request.SizeBytes > app.TaskFileFetchMaxFileBytes || !app.TaskFileFetchMediaTypeValid(request.MediaType) {
		return app.TaskFileFetchManifestReceipt{}, domain.InvalidArgument("manifest", "Task file fetch manifest is invalid")
	}
	var receipt app.TaskFileFetchManifestReceipt
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		transfer, err := lockTaskFileFetchForPC(ctx, tx, taskID, fetchID, ticketHash)
		if err != nil {
			return err
		}
		fetch := transfer.Fetch
		if app.TaskFileFetchMediaTypeForPath(fetch.RelativePath) != request.MediaType {
			return domain.InvalidArgument("media_type", "Task file fetch media type does not match the selected file")
		}
		switch fetch.Status {
		case task.FileFetchCompleted:
			if !sameTaskFileFetchManifest(fetch, request) {
				return domain.NewError(domain.CodeConflict, "Task file fetch manifest changed after completion")
			}
			confirmed, err := listTaskFileFetchSequences(ctx, tx, fetch.OwnerID, taskID, fetchID)
			if err != nil {
				return err
			}
			receipt = manifestReceipt(fetch, confirmed)
			return nil
		case task.FileFetchTransferring, task.FileFetchVerifying:
			if !sameTaskFileFetchManifest(fetch, request) {
				return domain.NewError(domain.CodeConflict, "Task file fetch manifest changed during transfer")
			}
			confirmed, err := listTaskFileFetchSequences(ctx, tx, fetch.OwnerID, taskID, fetchID)
			if err != nil {
				return err
			}
			receipt = manifestReceipt(fetch, confirmed)
			return nil
		case task.FileFetchQueued:
		default:
			return domain.NewError(domain.CodeInvalidState, "Task file fetch cannot accept a manifest in its current state")
		}
		if !transfer.TicketIsLive {
			return domain.NewError(domain.CodeTransferExpired, "Task file fetch ticket has expired")
		}
		if err := lockTaskFileFetchOwnerQuota(ctx, tx, fetch.OwnerID); err != nil {
			return err
		}
		var stagingBytes int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(size_bytes),0) FROM task_file_fetches
WHERE owner_id=$1 AND status IN ('transferring','verifying')`, fetch.OwnerID).Scan(&stagingBytes); err != nil {
			return fmt.Errorf("sum Task file fetch staging bytes: %w", err)
		}
		if stagingBytes+request.SizeBytes > app.TaskFileFetchStagingBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "Task file fetch staging quota has been reached")
		}
		command, err := tx.Exec(ctx, `UPDATE task_file_fetches SET source_version=$4,source_sha256=$5,size_bytes=$6,
media_type=$7,status='transferring',waiting_reason=NULL,confirmed_bytes=0,
staging_expires_at=clock_timestamp()+$8::interval,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND status='queued'`, fetch.OwnerID, taskID, fetchID,
			request.SourceVersion, request.SourceSHA256, request.SizeBytes, request.MediaType, app.TaskFileFetchStagingLifetime.String())
		if err != nil {
			return fmt.Errorf("pin Task file fetch manifest: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Task file fetch changed before its manifest was saved")
		}
		updated, err := getTaskFileFetchDataForUpdate(ctx, tx, fetch.OwnerID, taskID, fetchID)
		if err != nil {
			return fmt.Errorf("read pinned Task file fetch manifest: %w", err)
		}
		confirmed, err := listTaskFileFetchSequences(ctx, tx, updated.OwnerID, taskID, fetchID)
		if err != nil {
			return err
		}
		receipt = manifestReceipt(updated, confirmed)
		return nil
	})
	return receipt, err
}

func (s *Store) PutTaskFileFetchChunk(ctx context.Context, taskID, fetchID string, ticketHash []byte,
	sequence int32, digest string, content []byte) (app.TaskFileFetchChunkReceipt, error) {
	if ctx == nil || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) || len(ticketHash) != sha256.Size ||
		sequence < 0 || !validPostgresSHA256(digest) || len(content) < 1 || int64(len(content)) > app.TaskFileFetchChunkBytes {
		return app.TaskFileFetchChunkReceipt{}, domain.InvalidArgument("chunk", "Task file fetch chunk is invalid")
	}
	var receipt app.TaskFileFetchChunkReceipt
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		transfer, err := lockTaskFileFetchForPC(ctx, tx, taskID, fetchID, ticketHash)
		if err != nil {
			return err
		}
		fetch := transfer.Fetch
		if fetch.Status != task.FileFetchCompleted && !transfer.TicketIsLive {
			return domain.NewError(domain.CodeTransferExpired, "Task file fetch ticket has expired")
		}
		if fetch.SourceVersion == "" || fetch.SourceSHA256 == "" || fetch.SizeBytes < 1 ||
			fetch.Status != task.FileFetchTransferring && fetch.Status != task.FileFetchVerifying && fetch.Status != task.FileFetchCompleted {
			return domain.NewError(domain.CodeInvalidState, "Task file fetch has no active manifest")
		}
		if !validPostgresSHA256(fetch.SourceSHA256) {
			return domain.NewError(domain.CodeInternal, "Task file fetch manifest digest is invalid")
		}
		chunkCount := taskFileFetchChunkCount(fetch.SizeBytes)
		if sequence >= chunkCount {
			return domain.InvalidArgument("sequence", "Task file fetch chunk sequence is outside the manifest")
		}
		expectedSize := taskFileFetchChunkSize(fetch.SizeBytes, sequence)
		if int64(len(content)) != expectedSize || digestBytes(content) != digest {
			return domain.NewError(domain.CodeConflict, "Task file fetch chunk does not match the manifest")
		}
		var storedContent []byte
		var storedDigest []byte
		err = tx.QueryRow(ctx, `SELECT content,content_sha256 FROM task_file_fetch_chunks
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND sequence=$4`, fetch.OwnerID, taskID, fetchID, sequence).
			Scan(&storedContent, &storedDigest)
		if err == nil {
			if subtle.ConstantTimeCompare(storedContent, content) != 1 || hex.EncodeToString(storedDigest) != digest {
				return domain.NewError(domain.CodeConflict, "Task file fetch chunk changed after confirmation")
			}
			var confirmed int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(content)),0) FROM task_file_fetch_chunks
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`, fetch.OwnerID, taskID, fetchID).Scan(&confirmed); err != nil {
				return fmt.Errorf("sum replayed Task file fetch chunks: %w", err)
			}
			receipt = app.TaskFileFetchChunkReceipt{FetchID: fetchID, Sequence: sequence, ConfirmedBytes: confirmed, Replayed: true}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read Task file fetch chunk: %w", err)
		}
		if fetch.Status != task.FileFetchTransferring {
			return domain.NewError(domain.CodeInvalidState, "Task file fetch no longer accepts new chunks")
		}
		if fetch.StagingExpiresAt == nil || !transfer.TicketIsLive {
			return domain.NewError(domain.CodeTransferExpired, "Task file fetch staging has expired")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_file_fetch_chunks (owner_id,task_id,fetch_id,sequence,content,content_sha256)
VALUES ($1,$2,$3,$4,$5,$6)`, fetch.OwnerID, taskID, fetchID, sequence, content, mustDecodePostgresSHA256(digest)); err != nil {
			return fmt.Errorf("store Task file fetch chunk: %w", err)
		}
		var confirmed int64
		var count int32
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(content)),0),count(*) FROM task_file_fetch_chunks
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`, fetch.OwnerID, taskID, fetchID).Scan(&confirmed, &count); err != nil {
			return fmt.Errorf("sum Task file fetch chunks: %w", err)
		}
		status := task.FileFetchTransferring
		if count == chunkCount {
			status = task.FileFetchVerifying
		}
		command, err := tx.Exec(ctx, `UPDATE task_file_fetches SET confirmed_bytes=$4,status=$5,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND status='transferring'`, fetch.OwnerID, taskID, fetchID, confirmed, string(status))
		if err != nil {
			return fmt.Errorf("advance Task file fetch progress: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Task file fetch changed while storing a chunk")
		}
		receipt = app.TaskFileFetchChunkReceipt{FetchID: fetchID, Sequence: sequence, ConfirmedBytes: confirmed}
		return nil
	})
	return receipt, err
}

func (s *Store) CommitTaskFileFetch(ctx context.Context, taskID, fetchID string, ticketHash []byte,
	request app.TaskFileFetchCommitRequest, candidateContentID string) (app.TaskFileFetchCommitReceipt, error) {
	if ctx == nil || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) || len(ticketHash) != sha256.Size ||
		!validPostgresSHA256(request.SourceVersion) || !validPostgresSHA256(request.SourceSHA256) ||
		request.SizeBytes < 1 || request.SizeBytes > app.TaskFileFetchMaxFileBytes || !domain.ValidOperationID(candidateContentID) {
		return app.TaskFileFetchCommitReceipt{}, domain.InvalidArgument("commit", "Task file fetch commit is invalid")
	}
	var receipt app.TaskFileFetchCommitReceipt
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		transfer, err := lockTaskFileFetchForPC(ctx, tx, taskID, fetchID, ticketHash)
		if err != nil {
			return err
		}
		fetch := transfer.Fetch
		if fetch.Status == task.FileFetchCompleted {
			if fetch.SourceVersion != request.SourceVersion || fetch.SourceSHA256 != request.SourceSHA256 || fetch.SizeBytes != request.SizeBytes {
				return domain.NewError(domain.CodeConflict, "Task file fetch commit does not match its completed manifest")
			}
			receipt = taskFileFetchCommitReceipt(fetch)
			return nil
		}
		if fetch.Status != task.FileFetchVerifying || fetch.SourceVersion != request.SourceVersion ||
			fetch.SourceSHA256 != request.SourceSHA256 || fetch.SizeBytes != request.SizeBytes || fetch.ConfirmedBytes != fetch.SizeBytes {
			return domain.NewError(domain.CodeConflict, "Task file fetch commit does not match the verified manifest")
		}
		if !transfer.TicketIsLive || fetch.StagingExpiresAt == nil {
			return domain.NewError(domain.CodeTransferExpired, "Task file fetch staging has expired")
		}
		if err := lockTaskFileFetchOwnerQuota(ctx, tx, fetch.OwnerID); err != nil {
			return err
		}
		var retainedBytes int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(size_bytes),0) FROM task_file_fetches
WHERE owner_id=$1 AND content_state='retained'`, fetch.OwnerID).Scan(&retainedBytes); err != nil {
			return fmt.Errorf("sum retained Task file fetch bytes: %w", err)
		}
		if retainedBytes+fetch.SizeBytes > app.TaskFileFetchRetainedBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "Task file fetch retained storage quota has been reached")
		}
		if err := verifyTaskFileFetchChunks(ctx, tx, fetch); err != nil {
			return err
		}
		var contentID string
		var expiresAt time.Time
		if err := tx.QueryRow(ctx, `UPDATE task_file_fetches SET status='completed',content_id=$4,
content_state='retained',expires_at=clock_timestamp()+$5::interval,staging_expires_at=NULL,
waiting_reason=NULL,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND status='verifying'
RETURNING content_id,expires_at`, fetch.OwnerID, taskID, fetchID, candidateContentID, app.TaskFileFetchRetention.String()).Scan(&contentID, &expiresAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeConflict, "Task file fetch changed before commit")
			}
			return fmt.Errorf("commit Task file fetch content: %w", err)
		}
		fetch.Status = task.FileFetchCompleted
		fetch.ContentID = contentID
		fetch.ExpiresAt = &expiresAt
		fetch.ContentState = task.FileFetchContentRetained
		fetch.StagingExpiresAt = nil
		receipt = taskFileFetchCommitReceipt(fetch)
		return nil
	})
	return receipt, err
}

func (s *Store) CancelTaskFileFetch(ctx context.Context, ownerID, taskID, fetchID string) (app.TaskFileFetch, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) {
		return app.TaskFileFetch{}, domain.InvalidArgument("file_fetch", "Task file fetch identity is invalid")
	}
	var result app.TaskFileFetch
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskHubOperationTask(ctx, tx, taskID, ownerID); err != nil {
			return err
		}
		fetch, err := getTaskFileFetchDataForUpdate(ctx, tx, ownerID, taskID, fetchID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "file fetch not found")
		}
		if err != nil {
			return fmt.Errorf("lock Task file fetch for cancellation: %w", err)
		}
		switch fetch.Status {
		case task.FileFetchCompleted:
			return domain.NewError(domain.CodeInvalidState, "completed Task file fetch cannot be cancelled")
		case task.FileFetchCancelled:
			result = fetch
			return nil
		case task.FileFetchFailed:
			return domain.NewError(domain.CodeInvalidState, "failed Task file fetch cannot be cancelled")
		case task.FileFetchQueued, task.FileFetchWaitingDevice, task.FileFetchTransferring, task.FileFetchVerifying, task.FileFetchCancelling:
		default:
			return domain.NewError(domain.CodeInvalidState, "Task file fetch cannot be cancelled in its current state")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM task_file_fetch_chunks WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`, ownerID, taskID, fetchID); err != nil {
			return fmt.Errorf("remove cancelled Task file fetch chunks: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_file_fetches SET status='cancelled',waiting_reason=NULL,ticket_hash=NULL,
ticket_expires_at=NULL,staging_expires_at=NULL,confirmed_bytes=0,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND status IN ('queued','waiting_device','transferring','verifying','cancelling')`, ownerID, taskID, fetchID); err != nil {
			return fmt.Errorf("cancel Task file fetch: %w", err)
		}
		result, err = getTaskFileFetchDataForUpdate(ctx, tx, ownerID, taskID, fetchID)
		return err
	})
	return result, err
}

func (s *Store) ListTaskFileFetches(ctx context.Context, ownerID, taskID string) ([]app.TaskFileFetch, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) {
		return nil, domain.InvalidArgument("file_fetch", "Task file fetch identity is invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+taskFileFetchProjectionColumns+`
FROM task_file_fetches f JOIN client_installations i ON i.id=f.source_installation_id AND i.owner_id=f.owner_id
WHERE f.owner_id=$1 AND f.task_id=$2 ORDER BY f.updated_at DESC,f.fetch_id DESC LIMIT $3`, ownerID, taskID, app.TaskFileFetchMaxPerTask)
	if err != nil {
		return nil, fmt.Errorf("list Task file fetches: %w", err)
	}
	defer rows.Close()
	values := make([]app.TaskFileFetch, 0)
	for rows.Next() {
		value, err := scanTaskFileFetch(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Task file fetch list: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Task file fetch list: %w", err)
	}
	return values, nil
}

func (s *Store) OpenTaskFileFetchContent(ctx context.Context, ownerID, taskID, fetchID string) (io.ReadCloser, app.TaskFileFetch, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) {
		return nil, app.TaskFileFetch{}, domain.InvalidArgument("file_fetch", "Task file fetch identity is invalid")
	}
	var body []byte
	var result app.TaskFileFetch
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskHubOperationTask(ctx, tx, taskID, ownerID); err != nil {
			return err
		}
		fetch, err := getTaskFileFetchDataForUpdate(ctx, tx, ownerID, taskID, fetchID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "file fetch not found")
		}
		if err != nil {
			return fmt.Errorf("lock Task file fetch content: %w", err)
		}
		if fetch.Status != task.FileFetchCompleted || fetch.ContentState != task.FileFetchContentRetained || fetch.ExpiresAt == nil {
			return domain.NewError(domain.CodeNotFound, "file fetch content not found")
		}
		if err := readTaskFileFetchContent(ctx, tx, fetch, &body); err != nil {
			return err
		}
		result = fetch
		return nil
	})
	if err != nil {
		return nil, app.TaskFileFetch{}, err
	}
	return io.NopCloser(bytes.NewReader(body)), result, nil
}

func (s *Store) DeleteTaskFileFetchContent(ctx context.Context, ownerID, taskID, fetchID string) (app.TaskFileFetch, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) {
		return app.TaskFileFetch{}, domain.InvalidArgument("file_fetch", "Task file fetch identity is invalid")
	}
	var result app.TaskFileFetch
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskHubOperationTask(ctx, tx, taskID, ownerID); err != nil {
			return err
		}
		fetch, err := getTaskFileFetchDataForUpdate(ctx, tx, ownerID, taskID, fetchID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "file fetch not found")
		}
		if err != nil {
			return fmt.Errorf("lock Task file fetch content for deletion: %w", err)
		}
		if fetch.ContentState == task.FileFetchContentDeleted {
			result = fetch
			return nil
		}
		if fetch.Status != task.FileFetchCompleted || fetch.ContentID == "" || fetch.ExpiresAt == nil {
			return domain.NewError(domain.CodeNotFound, "file fetch content not found")
		}
		var contentLive bool
		if err := tx.QueryRow(ctx, `SELECT expires_at>clock_timestamp() FROM task_file_fetches WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`,
			ownerID, taskID, fetchID).Scan(&contentLive); err != nil {
			return fmt.Errorf("check Task file fetch content expiry: %w", err)
		}
		if !contentLive || fetch.ContentState == task.FileFetchContentExpired {
			if _, err := tx.Exec(ctx, `DELETE FROM task_file_fetch_chunks WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`, ownerID, taskID, fetchID); err != nil {
				return fmt.Errorf("remove expired Task file fetch content: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE task_file_fetches SET content_state='expired',ticket_hash=NULL,ticket_expires_at=NULL,
updated_at=clock_timestamp() WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND content_state='retained'`, ownerID, taskID, fetchID); err != nil {
				return fmt.Errorf("mark Task file fetch content expired: %w", err)
			}
		} else {
			if _, err := tx.Exec(ctx, `DELETE FROM task_file_fetch_chunks WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`, ownerID, taskID, fetchID); err != nil {
				return fmt.Errorf("delete Task file fetch content: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE task_file_fetches SET content_state='deleted',deleted_at=clock_timestamp(),
ticket_hash=NULL,ticket_expires_at=NULL,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND content_state='retained'`, ownerID, taskID, fetchID); err != nil {
				return fmt.Errorf("mark Task file fetch content deleted: %w", err)
			}
		}
		result, err = getTaskFileFetchDataForUpdate(ctx, tx, ownerID, taskID, fetchID)
		return err
	})
	return result, err
}

func (s *Store) SetTaskFileFetchParseResult(ctx context.Context, ownerID, taskID, fetchID string,
	parseStatus task.FileFetchParseStatus, parseError string) (app.TaskFileFetch, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidOperationID(fetchID) ||
		!validPostgresTaskFileFetchParseResult(parseStatus, parseError) {
		return app.TaskFileFetch{}, domain.InvalidArgument("file_fetch_parse", "Task file fetch parse result is invalid")
	}
	var result app.TaskFileFetch
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskHubOperationTask(ctx, tx, taskID, ownerID); err != nil {
			return err
		}
		fetch, err := getTaskFileFetchDataForUpdate(ctx, tx, ownerID, taskID, fetchID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "file fetch not found")
		}
		if err != nil {
			return fmt.Errorf("lock Task file fetch parse result: %w", err)
		}
		if fetch.Status != task.FileFetchCompleted {
			return domain.NewError(domain.CodeInvalidState, "Task file fetch parse result requires a completed transfer")
		}
		if fetch.ParseStatus != task.FileFetchParsePending {
			if fetch.ParseStatus == parseStatus && fetch.ParseError == parseError {
				result = fetch
				return nil
			}
			return domain.NewError(domain.CodeConflict, "Task file fetch parse result is already final")
		}
		command, err := tx.Exec(ctx, `UPDATE task_file_fetches SET parse_status=$4,parse_error=$5,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND status='completed' AND parse_status='pending'`,
			ownerID, taskID, fetchID, string(parseStatus), nullableString(parseError))
		if err != nil {
			return fmt.Errorf("save Task file fetch parse result: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Task file fetch parse result changed before save")
		}
		result, err = getTaskFileFetchDataForUpdate(ctx, tx, ownerID, taskID, fetchID)
		return err
	})
	return result, err
}

func (s *Store) CleanupExpiredTaskFileFetches(ctx context.Context, now time.Time, limit int32) (int32, error) {
	if ctx == nil || now.IsZero() || limit < 1 || limit > app.TaskFileFetchCleanupBatch {
		return 0, domain.InvalidArgument("cleanup", "Task file fetch cleanup parameters are invalid")
	}
	var affected int32
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT owner_id,task_id,fetch_id,status,content_state
FROM task_file_fetches
WHERE (status IN ('transferring','verifying') AND staging_expires_at<=$1)
   OR (content_state='retained' AND expires_at<=$1)
ORDER BY COALESCE(staging_expires_at,expires_at,ticket_expires_at),fetch_id
FOR UPDATE SKIP LOCKED LIMIT $2`, now.UTC(), limit)
		if err != nil {
			return fmt.Errorf("select expired Task file fetches: %w", err)
		}
		type candidate struct {
			ownerID, taskID, fetchID, status, contentState string
		}
		candidates := make([]candidate, 0, limit)
		for rows.Next() {
			var value candidate
			if err := rows.Scan(&value.ownerID, &value.taskID, &value.fetchID, &value.status, &value.contentState); err != nil {
				rows.Close()
				return fmt.Errorf("read expired Task file fetch: %w", err)
			}
			candidates = append(candidates, value)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate expired Task file fetches: %w", err)
		}
		rows.Close()
		for _, value := range candidates {
			if value.contentState == string(task.FileFetchContentRetained) {
				if _, err := tx.Exec(ctx, `DELETE FROM task_file_fetch_chunks WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`, value.ownerID, value.taskID, value.fetchID); err != nil {
					return fmt.Errorf("delete expired Task file fetch content: %w", err)
				}
				if _, err := tx.Exec(ctx, `UPDATE task_file_fetches SET content_state='expired',ticket_hash=NULL,ticket_expires_at=NULL,
updated_at=clock_timestamp() WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND content_state='retained'`, value.ownerID, value.taskID, value.fetchID); err != nil {
					return fmt.Errorf("mark Task file fetch content expired: %w", err)
				}
			} else if value.status == string(task.FileFetchTransferring) || value.status == string(task.FileFetchVerifying) {
				if _, err := tx.Exec(ctx, `DELETE FROM task_file_fetch_chunks WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`, value.ownerID, value.taskID, value.fetchID); err != nil {
					return fmt.Errorf("delete expired Task file fetch staging: %w", err)
				}
				if _, err := tx.Exec(ctx, `UPDATE task_file_fetches SET status='failed',waiting_reason=NULL,ticket_hash=NULL,
ticket_expires_at=NULL,staging_expires_at=NULL,confirmed_bytes=0,updated_at=clock_timestamp()
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 AND status IN ('transferring','verifying')`, value.ownerID, value.taskID, value.fetchID); err != nil {
					return fmt.Errorf("expire Task file fetch staging: %w", err)
				}
			}
		}
		affected = int32(len(candidates))
		return nil
	})
	return affected, err
}

func lockTaskFileFetchForPC(ctx context.Context, tx pgx.Tx, taskID, fetchID string, ticketHash []byte) (taskFileFetchTransferAuthorization, error) {
	var ownerID string
	err := tx.QueryRow(ctx, `SELECT owner_id FROM task_file_fetches WHERE task_id=$1 AND fetch_id=$2`, taskID, fetchID).Scan(&ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
		}
		return taskFileFetchTransferAuthorization{}, fmt.Errorf("read Task file fetch owner: %w", err)
	}
	fetch, err := getTaskFileFetch(ctx, tx, ownerID, taskID, fetchID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
		}
		return taskFileFetchTransferAuthorization{}, fmt.Errorf("read Task file fetch transfer identity: %w", err)
	}
	if err := lockTaskHubOperationTask(ctx, tx, taskID, fetch.OwnerID); err != nil {
		return taskFileFetchTransferAuthorization{}, err
	}
	operation := app.TaskHubOperation{
		OwnerID: fetch.OwnerID, TaskID: fetch.TaskID, Key: fetch.OperationKey,
		TargetInstallationID: fetch.SourceInstallationID, Action: hub.ActionFileFetch,
		TaskAuthorizationVersion: fetch.TaskAuthorizationVersion, TargetGrantVersion: fetch.TargetGrantVersion,
		TaskRevision: fetch.TaskRevision, RequestHash: make([]byte, sha256.Size),
	}
	if fetch.Status != task.FileFetchCompleted {
		if err := lockTaskHubTarget(ctx, tx, operation); err != nil {
			return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
		}
	}
	locked, err := getTaskFileFetchDataForUpdate(ctx, tx, fetch.OwnerID, taskID, fetchID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
		}
		return taskFileFetchTransferAuthorization{}, fmt.Errorf("lock Task file fetch transfer record: %w", err)
	}
	var persistedHash []byte
	var ticketLive bool
	if err := tx.QueryRow(ctx, `SELECT ticket_hash,COALESCE(ticket_expires_at>clock_timestamp(),false)
FROM task_file_fetches WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3`, fetch.OwnerID, taskID, fetchID).
		Scan(&persistedHash, &ticketLive); err != nil {
		return taskFileFetchTransferAuthorization{}, fmt.Errorf("read Task file fetch ticket authorization: %w", err)
	}
	if len(persistedHash) != sha256.Size || subtle.ConstantTimeCompare(persistedHash, ticketHash) != 1 {
		return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	if !ticketLive {
		return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeTransferExpired, "Task file fetch ticket has expired")
	}
	var operationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM task_hub_operations
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3 AND action='file.fetch'
  AND target_installation_id=$4 AND target_grant_version=$5 AND task_authorization_version=$6 AND task_revision=$7`,
		locked.OwnerID, locked.TaskID, locked.OperationKey, locked.SourceInstallationID, locked.TargetGrantVersion,
		locked.TaskAuthorizationVersion, locked.TaskRevision).Scan(&operationStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
		}
		return taskFileFetchTransferAuthorization{}, fmt.Errorf("read Task file fetch operation status: %w", err)
	}
	if locked.Status == task.FileFetchCompleted {
		if operationStatus != string(task.OperationDispatched) && operationStatus != string(task.OperationUnknown) && operationStatus != string(task.OperationSucceeded) {
			return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
		}
		return taskFileFetchTransferAuthorization{Fetch: locked, OperationStatus: task.OperationStatus(operationStatus), TicketHash: persistedHash,
			TicketIsLive: ticketLive}, nil
	}
	if operationStatus != string(task.OperationDispatched) && operationStatus != string(task.OperationUnknown) {
		return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	if !ticketLive {
		return taskFileFetchTransferAuthorization{Fetch: locked, OperationStatus: task.OperationStatus(operationStatus), TicketHash: persistedHash}, nil
	}
	gate, err := requireOwnerExecutionEnabled(ctx, tx, locked.OwnerID, nil)
	if err != nil {
		return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	taskRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: locked.TaskID, OwnerID: locked.OwnerID})
	if err != nil {
		return taskFileFetchTransferAuthorization{}, fmt.Errorf("read current Task for file fetch: %w", err)
	}
	if taskRow.ExecutionLocation != "server" || taskRow.ExecutionEpoch != gate.epoch || taskRow.AuthVersion != locked.TaskAuthorizationVersion {
		return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeConflict, "Task authorization changed during file fetch")
	}
	currentAuthorization, err := requireCurrentTaskAuthorization(ctx, tx, taskRow)
	if err != nil {
		return taskFileFetchTransferAuthorization{}, err
	}
	if !currentAuthorization {
		return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeNotFound, "file fetch not found")
	}
	if taskRow.CurrentRevision != locked.TaskRevision || taskRow.DesiredRevision != locked.TaskRevision ||
		(taskRow.Status != string(task.StatusRunning) && taskRow.Status != string(task.StatusWaitingUser)) {
		return taskFileFetchTransferAuthorization{}, domain.NewError(domain.CodeConflict, "Task revision or state changed during file fetch")
	}
	return taskFileFetchTransferAuthorization{Fetch: locked, OperationStatus: task.OperationStatus(operationStatus), TicketHash: persistedHash, TicketIsLive: true}, nil
}

func lockTaskFileFetchOwnerQuota(ctx context.Context, tx pgx.Tx, ownerID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, ownerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "Task file fetch owner not found")
	}
	if err != nil {
		return fmt.Errorf("lock Task file fetch owner quota: %w", err)
	}
	return nil
}

func getTaskFileFetchDataForUpdate(ctx context.Context, tx pgx.Tx, ownerID, taskID, fetchID string) (app.TaskFileFetch, error) {
	return scanTaskFileFetch(tx.QueryRow(ctx, `SELECT `+taskFileFetchProjectionColumns+`
FROM task_file_fetches f JOIN client_installations i ON i.id=f.source_installation_id AND i.owner_id=f.owner_id
WHERE f.owner_id=$1 AND f.task_id=$2 AND f.fetch_id=$3 FOR UPDATE OF f`, ownerID, taskID, fetchID))
}

func sameTaskFileFetchManifest(fetch app.TaskFileFetch, request app.TaskFileFetchManifestRequest) bool {
	return fetch.SourceVersion == request.SourceVersion && fetch.SourceSHA256 == request.SourceSHA256 &&
		fetch.SizeBytes == request.SizeBytes && fetch.MediaType == request.MediaType
}

func manifestReceipt(fetch app.TaskFileFetch, confirmed []int32) app.TaskFileFetchManifestReceipt {
	if confirmed == nil {
		confirmed = []int32{}
	}
	return app.TaskFileFetchManifestReceipt{FetchID: fetch.ID, SourceVersion: fetch.SourceVersion, Status: string(fetch.Status), ConfirmedSequences: confirmed}
}

func listTaskFileFetchSequences(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, ownerID, taskID, fetchID string) ([]int32, error) {
	rows, err := q.Query(ctx, `SELECT sequence FROM task_file_fetch_chunks WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 ORDER BY sequence`, ownerID, taskID, fetchID)
	if err != nil {
		return nil, fmt.Errorf("list Task file fetch confirmed chunks: %w", err)
	}
	defer rows.Close()
	sequences := make([]int32, 0)
	for rows.Next() {
		var sequence int32
		if err := rows.Scan(&sequence); err != nil {
			return nil, fmt.Errorf("scan Task file fetch confirmed chunk: %w", err)
		}
		sequences = append(sequences, sequence)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Task file fetch confirmed chunks: %w", err)
	}
	return sequences, nil
}

func taskFileFetchCommitReceipt(fetch app.TaskFileFetch) app.TaskFileFetchCommitReceipt {
	var expiresAt time.Time
	if fetch.ExpiresAt != nil {
		expiresAt = fetch.ExpiresAt.UTC()
	}
	return app.TaskFileFetchCommitReceipt{
		FetchID: fetch.ID, TaskRevision: fetch.TaskRevision, TargetGrantVersion: fetch.TargetGrantVersion,
		Status: string(task.FileFetchCompleted), SourceVersion: fetch.SourceVersion, SourceSHA256: fetch.SourceSHA256,
		SizeBytes: fetch.SizeBytes, ContentID: fetch.ContentID, ExpiresAt: expiresAt,
	}
}

func taskFileFetchChunkCount(sizeBytes int64) int32 {
	return int32((sizeBytes + app.TaskFileFetchChunkBytes - 1) / app.TaskFileFetchChunkBytes)
}

func taskFileFetchChunkSize(sizeBytes int64, sequence int32) int64 {
	remaining := sizeBytes - int64(sequence)*app.TaskFileFetchChunkBytes
	if remaining > app.TaskFileFetchChunkBytes {
		return app.TaskFileFetchChunkBytes
	}
	return remaining
}

func verifyTaskFileFetchChunks(ctx context.Context, tx pgx.Tx, fetch app.TaskFileFetch) error {
	rows, err := tx.Query(ctx, `SELECT sequence,content,content_sha256 FROM task_file_fetch_chunks
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 ORDER BY sequence`, fetch.OwnerID, fetch.TaskID, fetch.ID)
	if err != nil {
		return fmt.Errorf("read Task file fetch chunks for commit: %w", err)
	}
	defer rows.Close()
	hasher := sha256.New()
	count := int32(0)
	var total int64
	for rows.Next() {
		var sequence int32
		var content, digest []byte
		if err := rows.Scan(&sequence, &content, &digest); err != nil {
			return fmt.Errorf("scan Task file fetch chunk for commit: %w", err)
		}
		if sequence != count || sequence >= taskFileFetchChunkCount(fetch.SizeBytes) || int64(len(content)) != taskFileFetchChunkSize(fetch.SizeBytes, sequence) ||
			digestBytes(content) != hex.EncodeToString(digest) {
			return domain.NewError(domain.CodeConflict, "Task file fetch chunks are incomplete or changed")
		}
		_, _ = hasher.Write(content)
		total += int64(len(content))
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate Task file fetch chunks for commit: %w", err)
	}
	if count != taskFileFetchChunkCount(fetch.SizeBytes) || total != fetch.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != fetch.SourceSHA256 {
		return domain.NewError(domain.CodeConflict, "Task file fetch total size or digest does not match its manifest")
	}
	return nil
}

func readTaskFileFetchContent(ctx context.Context, tx pgx.Tx, fetch app.TaskFileFetch, target *[]byte) error {
	rows, err := tx.Query(ctx, `SELECT sequence,content,content_sha256 FROM task_file_fetch_chunks
WHERE owner_id=$1 AND task_id=$2 AND fetch_id=$3 ORDER BY sequence`, fetch.OwnerID, fetch.TaskID, fetch.ID)
	if err != nil {
		return fmt.Errorf("read retained Task file fetch content: %w", err)
	}
	defer rows.Close()
	var content bytes.Buffer
	hasher := sha256.New()
	sequence := int32(0)
	for rows.Next() {
		var actual int32
		var chunk, digest []byte
		if err := rows.Scan(&actual, &chunk, &digest); err != nil {
			return fmt.Errorf("scan retained Task file fetch chunk: %w", err)
		}
		if actual != sequence || actual >= taskFileFetchChunkCount(fetch.SizeBytes) || int64(len(chunk)) != taskFileFetchChunkSize(fetch.SizeBytes, actual) ||
			digestBytes(chunk) != hex.EncodeToString(digest) {
			return domain.NewError(domain.CodeWorkspaceUnavailable, "Task file fetch content failed integrity verification")
		}
		_, _ = hasher.Write(chunk)
		_, _ = content.Write(chunk)
		sequence++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate retained Task file fetch content: %w", err)
	}
	if sequence != taskFileFetchChunkCount(fetch.SizeBytes) || int64(content.Len()) != fetch.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != fetch.SourceSHA256 {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "Task file fetch content failed integrity verification")
	}
	*target = content.Bytes()
	return nil
}

func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func mustDecodePostgresSHA256(value string) []byte {
	decoded, _ := hex.DecodeString(value)
	return decoded
}

func validPostgresTaskFileFetchParseResult(status task.FileFetchParseStatus, parseError string) bool {
	if !task.ValidFileFetchParseStatus(status) {
		return false
	}
	if status == task.FileFetchParseFailed || status == task.FileFetchParseUnavailable {
		switch parseError {
		case "invalid_encoding", "invalid_document", "result_too_large", "content_unavailable", "integrity_failed":
			return true
		default:
			return false
		}
	}
	return parseError == ""
}

var _ app.TaskFileFetchDataStore = (*Store)(nil)
