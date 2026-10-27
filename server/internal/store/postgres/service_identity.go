package postgres

import (
	"context"
	"fmt"
)

func (s *Store) ServiceInstanceID(ctx context.Context) (string, error) {
	id, err := s.q.GetServiceInstanceID(ctx)
	if err != nil {
		return "", fmt.Errorf("read service instance identity: %w", err)
	}
	return id, nil
}
