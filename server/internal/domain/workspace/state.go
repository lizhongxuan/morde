package workspace

import "morde/server/internal/domain"

func ValidateTransition(from, to State) error {
	valid := false
	switch from {
	case StateReady:
		valid = to == StateDeleting
	case StateDeleting:
		valid = to == StateDeleted
	}
	if valid {
		return nil
	}

	err := domain.NewError(domain.CodeInvalidState, "invalid workspace state transition")
	err.Details["state"] = string(from)
	return err
}
