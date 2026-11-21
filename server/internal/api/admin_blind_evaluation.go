package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/evaluation"
)

type BlindEvaluationManager interface {
	Create(context.Context, identity.Principal, evaluation.Suite, []evaluation.Run, []string, []string, []string) (evaluation.BlindPack, error)
	ReviewerBundle(context.Context, identity.Principal, string) (app.BlindReviewerBundle, error)
	SubmitReview(context.Context, identity.Principal, string, string, []evaluation.BlindCaseReview) (evaluation.BlindReview, error)
	ArbitrationBundle(context.Context, identity.Principal, string) (app.BlindArbitrationBundle, error)
	SubmitArbitration(context.Context, identity.Principal, string, string, string, []evaluation.BlindCaseReview) (evaluation.BlindArbitration, error)
	Mapping(context.Context, identity.Principal, string) (evaluation.BlindMapping, error)
	RunManifests(context.Context, identity.Principal, string) ([]evaluation.RunManifest, error)
	Panel(context.Context, identity.Principal, string) (app.BlindPanelStatus, error)
}

type createBlindEvaluationRequest struct {
	Suite        evaluation.Suite `json:"suite"`
	Runs         []evaluation.Run `json:"runs"`
	Implementers []string         `json:"implementers"`
	Reviewers    []string         `json:"reviewers"`
	Arbitrators  []string         `json:"arbitrators"`
}

type submitBlindReviewRequest struct {
	Candidate string                       `json:"candidate"`
	Cases     []evaluation.BlindCaseReview `json:"cases"`
}

type submitBlindArbitrationRequest struct {
	Candidate string                       `json:"candidate"`
	Reason    string                       `json:"reason"`
	Cases     []evaluation.BlindCaseReview `json:"cases"`
}

type blindEvaluationPanelResponse struct {
	PackID           string `json:"pack_id"`
	Ready            bool   `json:"ready"`
	PendingReason    string `json:"pending_reason,omitempty"`
	ReviewCount      int    `json:"review_count"`
	ArbitrationCount int    `json:"arbitration_count"`
}

type blindReviewResponse struct {
	Candidate   string                       `json:"candidate"`
	SubmittedAt time.Time                    `json:"submitted_at"`
	Cases       []evaluation.BlindCaseReview `json:"cases"`
}

type blindReviewerBundleResponse struct {
	Pack    evaluation.BlindPack  `json:"pack"`
	Reviews []blindReviewResponse `json:"reviews"`
}

type blindArbitrationReviewResponse struct {
	Reviewer  string                       `json:"reviewer"`
	Candidate string                       `json:"candidate"`
	Cases     []evaluation.BlindCaseReview `json:"cases"`
}

type blindArbitrationBundleResponse struct {
	Pack                        evaluation.BlindPack             `json:"pack"`
	ExpectedReviewsPerCandidate int                              `json:"expected_reviews_per_candidate"`
	Reviews                     []blindArbitrationReviewResponse `json:"reviews"`
	Arbitrations                []blindArbitrationResultResponse `json:"arbitrations"`
}

type blindArbitrationResultResponse struct {
	Candidate string                       `json:"candidate"`
	Reason    string                       `json:"reason"`
	Cases     []evaluation.BlindCaseReview `json:"cases"`
}

type blindReviewReceiptResponse struct {
	Candidate   string    `json:"candidate"`
	SubmittedAt time.Time `json:"submitted_at"`
}

type blindArbitrationReceiptResponse struct {
	Candidate  string    `json:"candidate"`
	ResolvedAt time.Time `json:"resolved_at"`
}

func (h *api) registerBlindEvaluationRoutes(group *gin.RouterGroup) {
	if h.deps.BlindEvaluations == nil {
		return
	}
	group.POST("/admin/evaluation-batches", h.createBlindEvaluation)
	group.GET("/admin/evaluation-batches/:pack_id", h.getBlindEvaluationPanel)
	group.GET("/admin/evaluation-batches/:pack_id/mapping", h.getBlindEvaluationMapping)
	group.GET("/admin/evaluation-batches/:pack_id/manifests", h.getBlindEvaluationManifests)
	group.GET("/evaluation-batches/:pack_id/review", h.getBlindReviewBundle)
	group.POST("/evaluation-batches/:pack_id/reviews", h.submitBlindReview)
	group.GET("/evaluation-batches/:pack_id/arbitration", h.getBlindArbitrationBundle)
	group.POST("/evaluation-batches/:pack_id/arbitrations", h.submitBlindArbitration)
}

func (h *api) createBlindEvaluation(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	var request createBlindEvaluationRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	pack, err := h.deps.BlindEvaluations.Create(c.Request.Context(), principal, request.Suite, request.Runs,
		request.Implementers, request.Reviewers, request.Arbitrators)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, pack)
}

func (h *api) getBlindEvaluationPanel(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	status, err := h.deps.BlindEvaluations.Panel(c.Request.Context(), principal, c.Param("pack_id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, blindEvaluationPanelResponse{PackID: status.PackID, Ready: status.Ready,
		PendingReason: status.PendingReason, ReviewCount: status.ReviewCount, ArbitrationCount: status.ArbitrationCount})
}

func (h *api) getBlindEvaluationMapping(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	mapping, err := h.deps.BlindEvaluations.Mapping(c.Request.Context(), principal, c.Param("pack_id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, mapping)
}

func (h *api) getBlindEvaluationManifests(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	manifests, err := h.deps.BlindEvaluations.RunManifests(c.Request.Context(), principal, c.Param("pack_id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": manifests})
}

func (h *api) getBlindReviewBundle(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	bundle, err := h.deps.BlindEvaluations.ReviewerBundle(c.Request.Context(), principal, c.Param("pack_id"))
	if err != nil {
		writeError(c, err)
		return
	}
	response := blindReviewerBundleResponse{Pack: bundle.Pack, Reviews: make([]blindReviewResponse, 0, len(bundle.Reviews))}
	for _, review := range bundle.Reviews {
		response.Reviews = append(response.Reviews, blindReviewResponse{Candidate: review.Candidate,
			SubmittedAt: review.SubmittedAt, Cases: review.Cases})
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) submitBlindReview(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	var request submitBlindReviewRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.Candidate == "" || request.Cases == nil {
		writeError(c, domain.InvalidArgument("blind_review", "candidate and cases are required"))
		return
	}
	review, err := h.deps.BlindEvaluations.SubmitReview(c.Request.Context(), principal, c.Param("pack_id"), request.Candidate, request.Cases)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, blindReviewReceiptResponse{Candidate: review.Candidate, SubmittedAt: review.SubmittedAt})
}

func (h *api) getBlindArbitrationBundle(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	bundle, err := h.deps.BlindEvaluations.ArbitrationBundle(c.Request.Context(), principal, c.Param("pack_id"))
	if err != nil {
		writeError(c, err)
		return
	}
	response := blindArbitrationBundleResponse{Pack: bundle.Pack, ExpectedReviewsPerCandidate: bundle.ExpectedReviewsPerCandidate,
		Reviews:      make([]blindArbitrationReviewResponse, 0, len(bundle.Reviews)),
		Arbitrations: make([]blindArbitrationResultResponse, 0, len(bundle.Arbitrations))}
	for _, review := range bundle.Reviews {
		response.Reviews = append(response.Reviews, blindArbitrationReviewResponse{Reviewer: review.Reviewer,
			Candidate: review.Candidate, Cases: review.Cases})
	}
	for _, arbitration := range bundle.Arbitrations {
		response.Arbitrations = append(response.Arbitrations, blindArbitrationResultResponse{Candidate: arbitration.Candidate,
			Reason: arbitration.Reason, Cases: arbitration.Cases})
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) submitBlindArbitration(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	var request submitBlindArbitrationRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.Candidate == "" || request.Reason == "" || request.Cases == nil {
		writeError(c, domain.InvalidArgument("blind_arbitration", "candidate, reason and cases are required"))
		return
	}
	arbitration, err := h.deps.BlindEvaluations.SubmitArbitration(c.Request.Context(), principal, c.Param("pack_id"),
		request.Candidate, request.Reason, request.Cases)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, blindArbitrationReceiptResponse{Candidate: arbitration.Candidate, ResolvedAt: arbitration.ResolvedAt})
}
