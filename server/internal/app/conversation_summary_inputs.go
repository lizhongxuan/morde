package app

import "context"

type ConversationSummaryInputCursor struct {
	Identity         ConversationSummaryIdentity
	BoundarySequence int64
	SummaryVersion   int64
	SummarySHA256    []byte
	Sequence         int64
	KindOrder        int
	ID               string
	SourceOrder      int
	ByteOffset       int64
}

type ConversationSummaryInputPageRequest struct {
	Claim               ConversationRouteClaim
	ContextWindowTokens int
	Cursor              *ConversationSummaryInputCursor
}

type ConversationSummaryInputPage struct {
	Identity         ConversationSummaryIdentity
	BoundarySequence int64
	Head             ConversationSummaryHead
	Summary          *StoredConversationSummary
	Sources          []ConversationSummaryPromptSource
	Next             *ConversationSummaryInputCursor
}

type ConversationSummaryInputPageStore interface {
	LoadConversationSummaryInputPage(context.Context, ConversationSummaryInputPageRequest) (ConversationSummaryInputPage, error)
}
