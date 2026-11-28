package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func (s *Store) LimitConversationRouteOutputBudget(ctx context.Context, call app.ModelCallReservation,
	projectRate *project.RateSnapshot) (app.ConversationRouteOutputBudgetLimits, error) {
	if ctx == nil || call.Source != app.ModelCallRoute || call.Route == nil ||
		call.Route.Claim.Input.ResponseMode != app.ConversationResponseLongForm ||
		call.OwnerID != call.Route.Claim.Message.ActorID || call.ProjectID != call.Route.Claim.Input.ProjectID ||
		!domain.ValidModelConnectionID(call.ModelID) || call.ModelID != call.Route.Context.ModelID ||
		call.RouteOutputBudget == nil || call.RouteOutputBudget.Stage != app.ConversationResponseLongForm ||
		call.RouteOutputBudget.ContextSafeTokens < 1 || call.RouteOutputBudget.InputReservedTokens < 1 ||
		call.RouteOutputBudget.SendTokens < 1 || call.RouteOutputBudget.InputReservedTokens > call.ReservedTokens ||
		call.RouteOutputBudget.SendTokens != call.ReservedTokens-call.RouteOutputBudget.InputReservedTokens ||
		(call.ProjectID != "" && !call.PersonalModel && (projectRate == nil || projectRate.Validate() != nil ||
			projectRate.ModelID != call.ModelID || projectRate.Model != call.ProviderModel)) ||
		((call.ProjectID == "" || call.PersonalModel) && projectRate != nil) {
		return app.ConversationRouteOutputBudgetLimits{}, domain.InvalidArgument("route_model", "long-form budget limit request is invalid")
	}
	limits := app.ConversationRouteOutputBudgetLimits{}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, call.OwnerID); err != nil {
			return err
		}
		if err := lockConversationRouteClaim(ctx, tx, call.Route.Claim); err != nil {
			return err
		}
		if err := checkModelCaller(ctx, tx, call); err != nil {
			return err
		}
		var cancelled bool
		if err := tx.QueryRow(ctx, `SELECT cancel_requested_at IS NOT NULL FROM conversation_message_routes
WHERE message_id=$1 AND actor_id=$2 AND state='processing' AND lease_owner=$3 AND lease_version=$4 FOR UPDATE`,
			call.Route.Claim.Message.ID, call.OwnerID, call.Route.Claim.LeaseOwner, call.Route.Claim.LeaseVersion).Scan(&cancelled); err != nil {
			return domain.NewError(domain.CodeConflict, "long-form conversation route is no longer current")
		} else if cancelled {
			return domain.NewError(domain.CodeConflict, "long-form conversation reply was cancelled before dispatch")
		}

		limits.AccountSafeTokens = int64(app.MaxModelOutputTokens)
		if !call.PersonalModel {
			limit, _, err := lockPlatformModelBudget(ctx, tx, call.OwnerID)
			if err != nil {
				return err
			}
			var used int64
			var unknown bool
			if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN state='settled' THEN input_tokens+output_tokens
WHEN state='rejected' THEN 0 ELSE reserved_tokens END),0),COALESCE(bool_or(state='unknown'),false)
FROM model_call_ledger WHERE owner_id=$1 AND NOT personal_model`, call.OwnerID).Scan(&used, &unknown); err != nil {
				return fmt.Errorf("read route model account budget: %w", err)
			}
			if unknown {
				return domain.NewError(domain.CodeInvalidState, "model usage is unknown and requires reconciliation")
			}
			if used < 0 || used > limit || call.RouteOutputBudget.InputReservedTokens > limit-used {
				limits.AccountSafeTokens = 0
			} else {
				limits.AccountSafeTokens = min(limit-used-call.RouteOutputBudget.InputReservedTokens, int64(app.MaxModelOutputTokens))
			}
		}

		limits.ProjectSafeTokens = int64(app.MaxModelOutputTokens)
		if call.ProjectID != "" && !call.PersonalModel {
			if _, _, err := lockProjectForActorInTx(ctx, tx, call.OwnerID, call.ProjectID, project.RoleMember, false); err != nil {
				return err
			}
			budget, configured, err := loadCurrentProjectBudgetInTx(ctx, tx, call.ProjectID)
			if err != nil {
				return err
			}
			if !configured {
				limits.ProjectSafeTokens = 0
				return nil
			}
			if budget.Currency != projectRate.Currency {
				return domain.NewError(domain.CodeInvalidState, "project model rate currency does not match the budget")
			}
			totals, err := loadProjectBudgetTotalsInTx(ctx, tx, call.ProjectID)
			if err != nil {
				return err
			}
			if totals.HasUnknown {
				return domain.NewError(domain.CodeInvalidState, "project model usage is unknown and requires reconciliation")
			}
			if totals.LedgerCount > 0 && totals.LedgerCurrency != budget.Currency {
				return domain.NewError(domain.CodeInternal, "project budget currency does not match its ledger")
			}
			remaining := int64(0)
			if totals.SpentMicros <= budget.LimitMicros && totals.ReservedMicros <= budget.LimitMicros-totals.SpentMicros {
				remaining = budget.LimitMicros - totals.SpentMicros - totals.ReservedMicros
			}
			limits.ProjectSafeTokens, err = projectRouteOutputTokenLimit(*projectRate, call.RouteOutputBudget.InputReservedTokens,
				call.RouteOutputBudget.ContextSafeTokens, remaining)
			if err != nil {
				return fmt.Errorf("calculate project route output budget: %w", err)
			}
		}
		return nil
	})
	return limits, err
}

func projectRouteOutputTokenLimit(rate project.RateSnapshot, inputTokens, candidateTokens, availableMicros int64) (int64, error) {
	if rate.Validate() != nil || inputTokens < 1 || candidateTokens < 1 || availableMicros < 0 {
		return 0, domain.InvalidArgument("route_model", "project route output budget inputs are invalid")
	}
	canAfford := func(outputTokens int64) (bool, error) {
		cost, err := rate.EstimateTokenCost(project.Usage{InputTokens: inputTokens, OutputTokens: outputTokens})
		if err != nil {
			return false, err
		}
		return cost <= availableMicros, nil
	}
	low, high := int64(0), candidateTokens
	for low < high {
		middle := low + (high-low+1)/2
		affordable, err := canAfford(middle)
		if err != nil {
			return 0, err
		}
		if affordable {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low, nil
}
