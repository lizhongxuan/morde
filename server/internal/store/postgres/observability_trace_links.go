package postgres

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"morde/server/internal/domain"
)

func (s *Store) LinkAcceptedMessageTrace(ctx context.Context, traceID, spanID, requestID, messageID string) error {
	if !validLowerHexIdentifier(traceID, 32) || !validLowerHexIdentifier(spanID, 16) || requestID == "" || len(requestID) > 255 ||
		!domain.ValidMessageID(messageID) {
		return domain.InvalidArgument("trace_link", "conversation trace link is invalid")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO observability_trace_links(trace_id,span_id,request_id,message_id)
VALUES($1,$2,$3,$4) ON CONFLICT(trace_id,span_id,message_id) DO NOTHING`, traceID, spanID, requestID, messageID)
	if err != nil {
		return fmt.Errorf("write conversation trace link: %w", err)
	}
	return nil
}

func validLowerHexIdentifier(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	for _, item := range decoded {
		if item != 0 {
			return true
		}
	}
	return false
}
