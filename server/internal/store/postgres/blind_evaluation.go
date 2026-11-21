package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/evaluation"

	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateBlindEvaluation(ctx context.Context, suite evaluation.Suite, pack evaluation.BlindPack, mapping evaluation.BlindMapping,
	manifests []evaluation.RunManifest, createdBy string, implementers, reviewers, arbitrators []string) error {
	suiteJSON, err := json.Marshal(suite)
	if err != nil {
		return fmt.Errorf("encode blind evaluation suite: %w", err)
	}
	packJSON, err := json.Marshal(pack)
	if err != nil {
		return fmt.Errorf("encode blind evaluation pack: %w", err)
	}
	mappingJSON, err := json.Marshal(mapping)
	if err != nil {
		return fmt.Errorf("encode blind evaluation mapping: %w", err)
	}
	return withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO blind_evaluation_packs(pack_id,suite_version,suite_json,pack_json,created_by)
VALUES ($1,$2,$3::jsonb,$4::jsonb,$5)`, pack.ID, suite.Version, suiteJSON, packJSON, createdBy); err != nil {
			return fmt.Errorf("create blind evaluation pack: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO blind_evaluation_mappings(pack_id,mapping_json) VALUES ($1,$2::jsonb)`, pack.ID, mappingJSON); err != nil {
			return fmt.Errorf("store blind evaluation mapping: %w", err)
		}
		for _, manifest := range manifests {
			manifestJSON, err := json.Marshal(manifest)
			if err != nil {
				return fmt.Errorf("encode blind evaluation run manifest: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO blind_evaluation_run_manifests(pack_id,run_id,manifest_json) VALUES ($1,$2,$3::jsonb)`,
				pack.ID, manifest.RunID, manifestJSON); err != nil {
				return fmt.Errorf("store blind evaluation run manifest: %w", err)
			}
		}
		for _, roster := range []struct {
			role string
			ids  []string
		}{{"implementer", implementers}, {"reviewer", reviewers}, {"arbitrator", arbitrators}} {
			for _, userID := range roster.ids {
				if _, err := tx.Exec(ctx, `INSERT INTO blind_evaluation_participants(pack_id,user_id,role) VALUES ($1,$2,$3)`, pack.ID, userID, roster.role); err != nil {
					return fmt.Errorf("assign blind evaluation %s: %w", roster.role, err)
				}
			}
		}
		return nil
	})
}

func (s *Store) GetBlindReviewBundle(ctx context.Context, packID, reviewerID string) (app.BlindReviewBundle, error) {
	return s.getBlindEvaluationParticipantBundle(ctx, packID, reviewerID, "reviewer")
}

func (s *Store) GetBlindArbitrationBundle(ctx context.Context, packID, arbitratorID string) (app.BlindReviewBundle, error) {
	return s.getBlindEvaluationParticipantBundle(ctx, packID, arbitratorID, "arbitrator")
}

func (s *Store) getBlindEvaluationParticipantBundle(ctx context.Context, packID, userID, role string) (app.BlindReviewBundle, error) {
	var suiteJSON, packJSON []byte
	var implementers, reviewers []string
	err := s.pool.QueryRow(ctx, `SELECT pack.suite_json,pack.pack_json,
  COALESCE((SELECT array_agg(participant.user_id ORDER BY participant.user_id)
    FROM blind_evaluation_participants participant
    WHERE participant.pack_id=pack.pack_id AND participant.role='implementer'),'{}'::text[]),
  COALESCE((SELECT array_agg(participant.user_id ORDER BY participant.user_id)
    FROM blind_evaluation_participants participant
    WHERE participant.pack_id=pack.pack_id AND participant.role='reviewer'),'{}'::text[])
FROM blind_evaluation_packs pack
WHERE pack.pack_id=$1 AND EXISTS (
  SELECT 1 FROM blind_evaluation_participants participant
  WHERE participant.pack_id=pack.pack_id AND participant.user_id=$2 AND participant.role=$3
)`, packID, userID, role).Scan(&suiteJSON, &packJSON, &implementers, &reviewers)
	if err != nil {
		return app.BlindReviewBundle{}, storeError("get blind evaluation participant bundle", "blind evaluation", err)
	}
	bundle := app.BlindReviewBundle{Implementers: implementers, Reviewers: reviewers}
	if err := decodeBlindEvaluationJSON(suiteJSON, &bundle.Suite); err != nil {
		return app.BlindReviewBundle{}, err
	}
	if err := decodeBlindEvaluationJSON(packJSON, &bundle.Pack); err != nil {
		return app.BlindReviewBundle{}, err
	}
	query, args := `SELECT review_json FROM blind_evaluation_reviews WHERE pack_id=$1 ORDER BY candidate_label,reviewer_id`, []any{packID}
	if role == "reviewer" {
		query = `SELECT review_json FROM blind_evaluation_reviews WHERE pack_id=$1 AND reviewer_id=$2 ORDER BY candidate_label`
		args = append(args, userID)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return app.BlindReviewBundle{}, fmt.Errorf("list blind evaluation reviews: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var review evaluation.BlindReview
		if err := rows.Scan(&raw); err != nil {
			return app.BlindReviewBundle{}, fmt.Errorf("read blind evaluation review: %w", err)
		}
		if err := decodeBlindEvaluationJSON(raw, &review); err != nil {
			return app.BlindReviewBundle{}, err
		}
		bundle.Reviews = append(bundle.Reviews, review)
	}
	if err := rows.Err(); err != nil {
		return app.BlindReviewBundle{}, fmt.Errorf("list blind evaluation reviews: %w", err)
	}
	return bundle, nil
}

func (s *Store) GetBlindEvaluationPanel(ctx context.Context, packID string) (app.BlindEvaluationPanelRecord, error) {
	var suiteJSON, packJSON []byte
	if err := s.pool.QueryRow(ctx, `SELECT suite_json,pack_json FROM blind_evaluation_packs WHERE pack_id=$1`, packID).Scan(&suiteJSON, &packJSON); err != nil {
		return app.BlindEvaluationPanelRecord{}, storeError("get blind evaluation panel", "blind evaluation", err)
	}
	record := app.BlindEvaluationPanelRecord{}
	if err := decodeBlindEvaluationJSON(suiteJSON, &record.Suite); err != nil {
		return app.BlindEvaluationPanelRecord{}, err
	}
	if err := decodeBlindEvaluationJSON(packJSON, &record.Pack); err != nil {
		return app.BlindEvaluationPanelRecord{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT user_id,role FROM blind_evaluation_participants WHERE pack_id=$1 ORDER BY role,user_id`, packID)
	if err != nil {
		return app.BlindEvaluationPanelRecord{}, fmt.Errorf("list blind evaluation participants: %w", err)
	}
	for rows.Next() {
		var userID, role string
		if err := rows.Scan(&userID, &role); err != nil {
			rows.Close()
			return app.BlindEvaluationPanelRecord{}, fmt.Errorf("read blind evaluation participant: %w", err)
		}
		switch role {
		case "implementer":
			record.Implementers = append(record.Implementers, userID)
		case "reviewer":
			record.Reviewers = append(record.Reviewers, userID)
		case "arbitrator":
			record.Arbitrators = append(record.Arbitrators, userID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.BlindEvaluationPanelRecord{}, fmt.Errorf("read blind evaluation participants: %w", err)
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT manifest_json FROM blind_evaluation_run_manifests WHERE pack_id=$1 ORDER BY run_id`, packID)
	if err != nil {
		return app.BlindEvaluationPanelRecord{}, fmt.Errorf("list blind evaluation run manifests: %w", err)
	}
	for rows.Next() {
		var raw []byte
		var manifest evaluation.RunManifest
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return app.BlindEvaluationPanelRecord{}, fmt.Errorf("read blind evaluation run manifest: %w", err)
		}
		if err := decodeBlindEvaluationJSON(raw, &manifest); err != nil {
			rows.Close()
			return app.BlindEvaluationPanelRecord{}, err
		}
		record.RunManifests = append(record.RunManifests, manifest)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.BlindEvaluationPanelRecord{}, fmt.Errorf("read blind evaluation run manifests: %w", err)
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `SELECT review_json FROM blind_evaluation_reviews WHERE pack_id=$1 ORDER BY candidate_label,reviewer_id`, packID)
	if err != nil {
		return app.BlindEvaluationPanelRecord{}, fmt.Errorf("list blind evaluation reviews: %w", err)
	}
	for rows.Next() {
		var raw []byte
		var review evaluation.BlindReview
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return app.BlindEvaluationPanelRecord{}, fmt.Errorf("read blind evaluation review: %w", err)
		}
		if err := decodeBlindEvaluationJSON(raw, &review); err != nil {
			rows.Close()
			return app.BlindEvaluationPanelRecord{}, err
		}
		record.Reviews = append(record.Reviews, review)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.BlindEvaluationPanelRecord{}, fmt.Errorf("read blind evaluation reviews: %w", err)
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `SELECT arbitration_json FROM blind_evaluation_arbitrations WHERE pack_id=$1 ORDER BY candidate_label`, packID)
	if err != nil {
		return app.BlindEvaluationPanelRecord{}, fmt.Errorf("list blind evaluation arbitrations: %w", err)
	}
	for rows.Next() {
		var raw []byte
		var arbitration evaluation.BlindArbitration
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return app.BlindEvaluationPanelRecord{}, fmt.Errorf("read blind evaluation arbitration: %w", err)
		}
		if err := decodeBlindEvaluationJSON(raw, &arbitration); err != nil {
			rows.Close()
			return app.BlindEvaluationPanelRecord{}, err
		}
		record.Arbitrations = append(record.Arbitrations, arbitration)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.BlindEvaluationPanelRecord{}, fmt.Errorf("read blind evaluation arbitrations: %w", err)
	}
	rows.Close()
	return record, nil
}

func (s *Store) GetBlindEvaluationMapping(ctx context.Context, packID string) (evaluation.BlindMapping, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT mapping_json FROM blind_evaluation_mappings WHERE pack_id=$1`, packID).Scan(&raw); err != nil {
		return evaluation.BlindMapping{}, storeError("get blind evaluation mapping", "blind evaluation mapping", err)
	}
	var mapping evaluation.BlindMapping
	if err := decodeBlindEvaluationJSON(raw, &mapping); err != nil {
		return evaluation.BlindMapping{}, err
	}
	return mapping, nil
}

func (s *Store) PutBlindReview(ctx context.Context, review evaluation.BlindReview) (evaluation.BlindReview, error) {
	encoded, err := json.Marshal(review)
	if err != nil {
		return evaluation.BlindReview{}, fmt.Errorf("encode blind review: %w", err)
	}
	var storedJSON []byte
	err = s.pool.QueryRow(ctx, `INSERT INTO blind_evaluation_reviews(pack_id,candidate_label,reviewer_id,review_json)
VALUES ($1,$2,$3,$4::jsonb) ON CONFLICT (pack_id,candidate_label,reviewer_id) DO NOTHING RETURNING review_json`,
		review.PackID, review.Candidate, review.ReviewerID, encoded).Scan(&storedJSON)
	if err == nil {
		var stored evaluation.BlindReview
		if err := decodeBlindEvaluationJSON(storedJSON, &stored); err != nil {
			return evaluation.BlindReview{}, err
		}
		return stored, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return evaluation.BlindReview{}, fmt.Errorf("store blind review: %w", err)
	}
	var existingJSON []byte
	if err := s.pool.QueryRow(ctx, `SELECT review_json FROM blind_evaluation_reviews WHERE pack_id=$1 AND candidate_label=$2 AND reviewer_id=$3`,
		review.PackID, review.Candidate, review.ReviewerID).Scan(&existingJSON); err != nil {
		return evaluation.BlindReview{}, storeError("read existing blind review", "blind review", err)
	}
	var existing evaluation.BlindReview
	if err := decodeBlindEvaluationJSON(existingJSON, &existing); err != nil {
		return evaluation.BlindReview{}, err
	}
	if !reflect.DeepEqual(existing.Cases, review.Cases) {
		return evaluation.BlindReview{}, domain.NewError(domain.CodeConflict, "blind review is already submitted with different judgments")
	}
	return existing, nil
}

func (s *Store) PutBlindArbitration(ctx context.Context, arbitration evaluation.BlindArbitration) (evaluation.BlindArbitration, error) {
	encoded, err := json.Marshal(arbitration)
	if err != nil {
		return evaluation.BlindArbitration{}, fmt.Errorf("encode blind arbitration: %w", err)
	}
	var storedJSON []byte
	err = s.pool.QueryRow(ctx, `INSERT INTO blind_evaluation_arbitrations(pack_id,candidate_label,arbitrator_id,arbitration_json)
VALUES ($1,$2,$3,$4::jsonb) ON CONFLICT (pack_id,candidate_label) DO NOTHING RETURNING arbitration_json`,
		arbitration.PackID, arbitration.Candidate, arbitration.ArbitratorID, encoded).Scan(&storedJSON)
	if err == nil {
		var stored evaluation.BlindArbitration
		if err := decodeBlindEvaluationJSON(storedJSON, &stored); err != nil {
			return evaluation.BlindArbitration{}, err
		}
		return stored, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return evaluation.BlindArbitration{}, fmt.Errorf("store blind arbitration: %w", err)
	}
	var existingJSON []byte
	if err := s.pool.QueryRow(ctx, `SELECT arbitration_json FROM blind_evaluation_arbitrations WHERE pack_id=$1 AND candidate_label=$2`,
		arbitration.PackID, arbitration.Candidate).Scan(&existingJSON); err != nil {
		return evaluation.BlindArbitration{}, storeError("read existing blind arbitration", "blind arbitration", err)
	}
	var existing evaluation.BlindArbitration
	if err := decodeBlindEvaluationJSON(existingJSON, &existing); err != nil {
		return evaluation.BlindArbitration{}, err
	}
	if existing.ArbitratorID != arbitration.ArbitratorID || existing.Reason != arbitration.Reason || !reflect.DeepEqual(existing.Cases, arbitration.Cases) {
		return evaluation.BlindArbitration{}, domain.NewError(domain.CodeConflict, "blind arbitration is already submitted with different resolution")
	}
	return existing, nil
}

func decodeBlindEvaluationJSON(raw []byte, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode persisted blind evaluation data: %w", err)
	}
	return nil
}

var _ app.BlindEvaluationStore = (*Store)(nil)
