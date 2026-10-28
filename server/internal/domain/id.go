package domain

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

var ErrIDAttemptsExhausted = errors.New("id collision retries exhausted")

func NewHostID() (string, error)               { return newID('h') }
func NewWorkspaceID() (string, error)          { return newID('w') }
func NewUserID() (string, error)               { return newID('u') }
func NewTokenID() (string, error)              { return newID('t') }
func NewClientInstallationID() (string, error) { return newID('i') }
func NewConversationID() (string, error)       { return newID('c') }
func NewTaskID() (string, error)               { return newID('q') }
func NewMessageID() (string, error)            { return newID('m') }
func NewStepID() (string, error)               { return newID('s') }
func NewOperationID() (string, error)          { return newID('o') }
func NewBatchItemID() (string, error)          { return newID('b') }
func NewArtifactID() (string, error)           { return newID('a') }
func NewPendingRequestID() (string, error)     { return newID('r') }
func NewTaskActionID() (string, error)         { return newID('x') }
func NewModelConnectionID() (string, error)    { return newID('p') }
func NewMCPConnectorID() (string, error)       { return newID('k') }

func newID(prefix byte) (string, error) {
	var id [8]byte
	id[0] = prefix
	max := big.NewInt(int64(len(idAlphabet)))
	for i := 1; i < len(id); i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate id: %w", err)
		}
		id[i] = idAlphabet[n.Int64()]
	}
	return string(id[:]), nil
}

func ValidHostID(id string) bool               { return validID(id, 'h') }
func ValidWorkspaceID(id string) bool          { return validID(id, 'w') }
func ValidUserID(id string) bool               { return validID(id, 'u') }
func ValidTokenID(id string) bool              { return validID(id, 't') }
func ValidClientInstallationID(id string) bool { return validID(id, 'i') }
func ValidConversationID(id string) bool       { return validID(id, 'c') }
func ValidTaskID(id string) bool               { return validID(id, 'q') }
func ValidMessageID(id string) bool            { return validID(id, 'm') }
func ValidStepID(id string) bool               { return validID(id, 's') }
func ValidOperationID(id string) bool          { return validID(id, 'o') }
func ValidBatchItemID(id string) bool          { return validID(id, 'b') }
func ValidArtifactID(id string) bool           { return validID(id, 'a') }
func ValidPendingRequestID(id string) bool     { return validID(id, 'r') }
func ValidTaskActionID(id string) bool         { return validID(id, 'x') }
func ValidModelConnectionID(id string) bool    { return validID(id, 'p') }
func ValidMCPConnectorID(id string) bool       { return validID(id, 'k') }

func validID(id string, prefix byte) bool {
	if len(id) != 8 || id[0] != prefix {
		return false
	}
	for i := 1; i < len(id); i++ {
		if !strings.ContainsRune(idAlphabet, rune(id[i])) {
			return false
		}
	}
	return true
}

func ValidTemplateID(id string) bool {
	if len(id) < 1 || len(id) > 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

func WorkspaceRootVolumeName(hostID string) string { return "workspaces-" + hostID }

func RuntimeVolumeName(hostID string) string { return "runtime-" + hostID }

func ProfileVolumeName(hostID string) string { return "profile-" + hostID }

func HostAppsVolumeName(hostID string) string { return "apps-" + hostID }

func ValidPreviewPort(port string) bool {
	if port == "" || len(port) > 5 || port[0] == '0' {
		return false
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n <= 65535
}

// ValidPreviewTargetPort rejects ports used by Host control services.
func ValidPreviewTargetPort(port int) bool {
	switch port {
	case 8080, 5900, 6080, 8090, 8091, 8100, 9222:
		return false
	default:
		return port >= 1 && port <= 65535
	}
}

func CreateWithIDRetry[T any](generate func() (string, error), create func(string) (T, error), isConflict func(error) bool) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		id, err := generate()
		if err != nil {
			return zero, err
		}
		value, err := create(id)
		if err == nil {
			return value, nil
		}
		if isConflict == nil || !isConflict(err) {
			return zero, err
		}
		lastErr = err
	}
	return zero, fmt.Errorf("%w: %w", ErrIDAttemptsExhausted, lastErr)
}
