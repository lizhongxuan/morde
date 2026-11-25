package app

import (
	"context"
	"errors"
	"testing"

	"morde/server/internal/domain"
)

type taskSettlementFailureStore struct {
	*modelGatewayStoreTest
	state                  string
	reservedTokens         int64
	actualTokens           int64
	knownSettlements       int
	unknownSettlements     int
	knownSettlementErr     error
	unknownSettlementErr   error
	commitBeforeKnownError bool
}

func (s *taskSettlementFailureStore) ReserveModelCall(ctx context.Context, value ModelCallReservation) error {
	if s.state != "" {
		return domain.NewError(domain.CodeConflict, "model call was already dispatched; reconcile its result")
	}
	s.state = "dispatched"
	s.reservedTokens = value.ReservedTokens
	return s.modelGatewayStoreTest.ReserveModelCall(ctx, value)
}

func (s *taskSettlementFailureStore) SettleModelCall(ctx context.Context, value ModelCallSettlement) error {
	if value.Unknown {
		s.unknownSettlements++
		if s.unknownSettlementErr != nil {
			return s.unknownSettlementErr
		}
		switch s.state {
		case "dispatched":
			s.state = "unknown"
			return s.modelGatewayStoreTest.SettleModelCall(ctx, value)
		case "unknown":
			return nil
		default:
			return domain.NewError(domain.CodeIdempotencyConflict, "model settlement was already recorded differently")
		}
	}

	s.knownSettlements++
	if s.state != "dispatched" {
		return domain.NewError(domain.CodeIdempotencyConflict, "model call is not awaiting settlement")
	}
	if !s.commitBeforeKnownError {
		return s.knownSettlementErr
	}
	s.state = "settled"
	s.reservedTokens = 0
	s.actualTokens = value.Usage.InputTokens + value.Usage.OutputTokens
	if err := s.modelGatewayStoreTest.SettleModelCall(ctx, value); err != nil {
		return err
	}
	return s.knownSettlementErr
}

func TestTaskModelGatewaySettlementFailurePreservesUsageAndDoesNotRedispatch(t *testing.T) {
	response := ModelResponse{Text: "The task is complete.", StopReason: "stop", ProviderReportedModel: "test-model",
		Usage: ModelUsage{Known: true, InputTokens: 11, OutputTokens: 7}}
	rawRequest := []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"Finish this task."}]}],"tools":[],"max_tokens":16}`)

	t.Run("unknown fallback keeps the full hold", func(t *testing.T) {
		service, base, client, call := taskGatewayFixture(t, rawRequest, response)
		store := &taskSettlementFailureStore{modelGatewayStoreTest: base,
			knownSettlementErr: errors.New("settlement transaction failed")}
		service.store = store

		if _, err := service.Complete(context.Background(), call); err == nil {
			t.Fatal("Task model call returned a response after its known settlement failed")
		}
		if store.state != "unknown" || store.reservedTokens != base.reservation.ReservedTokens || store.actualTokens != 0 ||
			store.knownSettlements != 1 || store.unknownSettlements != 1 || client.calls != 1 || base.abandonN != 0 {
			t.Fatalf("settlement failure released or replayed the model call: state=%s reserved=%d actual=%d known=%d unknown=%d provider=%d abandon=%d",
				store.state, store.reservedTokens, store.actualTokens, store.knownSettlements, store.unknownSettlements, client.calls, base.abandonN)
		}

		if _, err := service.Complete(context.Background(), call); err == nil || client.calls != 1 {
			t.Fatalf("unknown Task model call was redispatched: provider=%d err=%v", client.calls, err)
		}
	})

	t.Run("failed unknown fallback leaves dispatched hold", func(t *testing.T) {
		service, base, client, call := taskGatewayFixture(t, rawRequest, response)
		store := &taskSettlementFailureStore{modelGatewayStoreTest: base,
			knownSettlementErr:   errors.New("settlement transaction failed"),
			unknownSettlementErr: errors.New("unknown settlement also failed")}
		service.store = store

		if _, err := service.Complete(context.Background(), call); err == nil {
			t.Fatal("Task model call returned a response after both settlements failed")
		}
		if store.state != "dispatched" || store.reservedTokens != base.reservation.ReservedTokens || store.actualTokens != 0 ||
			store.knownSettlements != 1 || store.unknownSettlements != 1 || client.calls != 1 || base.abandonN != 0 {
			t.Fatalf("failed unknown fallback did not preserve the dispatched hold: state=%s reserved=%d actual=%d known=%d unknown=%d provider=%d abandon=%d",
				store.state, store.reservedTokens, store.actualTokens, store.knownSettlements, store.unknownSettlements, client.calls, base.abandonN)
		}

		if _, err := service.Complete(context.Background(), call); err == nil || client.calls != 1 {
			t.Fatalf("still-dispatched Task model call was redispatched: provider=%d err=%v", client.calls, err)
		}
	})

	t.Run("unknown fallback cannot overwrite a committed settlement", func(t *testing.T) {
		service, base, client, call := taskGatewayFixture(t, rawRequest, response)
		store := &taskSettlementFailureStore{modelGatewayStoreTest: base,
			knownSettlementErr: errors.New("settlement commit response was lost"), commitBeforeKnownError: true}
		service.store = store

		if _, err := service.Complete(context.Background(), call); err == nil {
			t.Fatal("gateway hid the lost settlement acknowledgement")
		}
		if store.state != "settled" || store.reservedTokens != 0 || store.actualTokens != 18 ||
			store.knownSettlements != 1 || store.unknownSettlements != 1 || client.calls != 1 || base.abandonN != 0 {
			t.Fatalf("Unknown fallback changed a committed settlement: state=%s reserved=%d actual=%d known=%d unknown=%d provider=%d abandon=%d",
				store.state, store.reservedTokens, store.actualTokens, store.knownSettlements, store.unknownSettlements, client.calls, base.abandonN)
		}

		recovered, err := service.Complete(context.Background(), call)
		if err != nil || recovered.Text != response.Text || client.calls != 1 || store.actualTokens != 18 {
			t.Fatalf("committed response was not recovered exactly once: response=%+v provider=%d actual=%d err=%v",
				recovered, client.calls, store.actualTokens, err)
		}
	})
}
