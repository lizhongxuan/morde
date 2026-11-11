package task

// FileFetchStatus describes a PC file transfer separately from Task execution
// status. A waiting file fetch does not change the Task state.
type FileFetchStatus string

const (
	FileFetchQueued        FileFetchStatus = "queued"
	FileFetchWaitingDevice FileFetchStatus = "waiting_device"
	FileFetchTransferring  FileFetchStatus = "transferring"
	FileFetchVerifying     FileFetchStatus = "verifying"
	FileFetchCompleted     FileFetchStatus = "completed"
	FileFetchCancelling    FileFetchStatus = "cancelling"
	FileFetchCancelled     FileFetchStatus = "cancelled"
	FileFetchFailed        FileFetchStatus = "failed"
)

func ValidFileFetchStatus(value FileFetchStatus) bool {
	switch value {
	case FileFetchQueued, FileFetchWaitingDevice, FileFetchTransferring, FileFetchVerifying,
		FileFetchCompleted, FileFetchCancelling, FileFetchCancelled, FileFetchFailed:
		return true
	default:
		return false
	}
}

// FileFetchContentState describes the retained-copy lifecycle independently
// from the PC transfer result. A completed fetch remains completed after its
// owner deletes the retained copy or retention expires.
type FileFetchContentState string

const (
	FileFetchContentNone     FileFetchContentState = "none"
	FileFetchContentRetained FileFetchContentState = "retained"
	FileFetchContentDeleted  FileFetchContentState = "deleted"
	FileFetchContentExpired  FileFetchContentState = "expired"
)

func ValidFileFetchContentState(value FileFetchContentState) bool {
	switch value {
	case FileFetchContentNone, FileFetchContentRetained, FileFetchContentDeleted, FileFetchContentExpired:
		return true
	default:
		return false
	}
}

// FileFetchParseStatus records whether retained bytes were safely made
// available as Task model context. It is independent from transfer status.
type FileFetchParseStatus string

const (
	FileFetchParsePending     FileFetchParseStatus = "pending"
	FileFetchParseExtracted   FileFetchParseStatus = "extracted"
	FileFetchParseRawOnly     FileFetchParseStatus = "raw_only"
	FileFetchParseFailed      FileFetchParseStatus = "failed"
	FileFetchParseUnavailable FileFetchParseStatus = "unavailable"
)

func ValidFileFetchParseStatus(value FileFetchParseStatus) bool {
	switch value {
	case FileFetchParsePending, FileFetchParseExtracted, FileFetchParseRawOnly, FileFetchParseFailed, FileFetchParseUnavailable:
		return true
	default:
		return false
	}
}
