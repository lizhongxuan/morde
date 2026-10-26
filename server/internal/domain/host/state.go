package host

import "morde/server/internal/domain"

func ValidateTransition(from, to State) error {
	valid := false
	switch from {
	case StateCreating:
		valid = to == StateRunning || to == StateStopped || to == StateFailed || to == StateDeleting
	case StateRunning:
		valid = to == StateStopping || to == StateFailed || to == StateDeleting
	case StateStopping:
		valid = to == StateStopped || to == StateDeleting
	case StateStopped:
		valid = to == StateCreating || to == StateDeleting
	case StateFailed:
		valid = to == StateDeleting
	case StateDeleting:
		valid = to == StateDeleted
	}
	if valid {
		return nil
	}

	err := domain.NewError(domain.CodeInvalidState, "invalid host state transition")
	err.Details["state"] = string(from)
	return err
}
