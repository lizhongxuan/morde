package conversation

import "morde/server/internal/domain"

type MainConversationStatus string

const (
	MainConversationReady             MainConversationStatus = "ready"
	MainConversationUninitialized     MainConversationStatus = "uninitialized"
	MainConversationSelectionRequired MainConversationStatus = "selection_required"
)

type MainConversation struct {
	Status         MainConversationStatus
	ConversationID string
	Version        int64
}

func (value MainConversation) Validate() error {
	if value.Version < 0 {
		return domain.InvalidArgument("main_conversation", "main conversation version is invalid")
	}
	switch value.Status {
	case MainConversationReady:
		if !domain.ValidConversationID(value.ConversationID) || value.Version < 1 {
			return domain.InvalidArgument("main_conversation", "ready main conversation is incomplete")
		}
	case MainConversationUninitialized, MainConversationSelectionRequired:
		if value.ConversationID != "" || value.Version != 0 {
			return domain.InvalidArgument("main_conversation", "unselected main conversation must have no id or version")
		}
	default:
		return domain.InvalidArgument("main_conversation", "main conversation status is invalid")
	}
	return nil
}
