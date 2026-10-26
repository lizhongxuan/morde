package runtimeclient

import "morde-runtime/contract"

const (
	ProtocolVersion = contract.ProtocolVersion
	ProtocolHeader  = contract.ProtocolHeader
)

type Mode = contract.Mode

const (
	ModeBlocked = contract.ModeBlocked
	ModeAI      = contract.ModeAI
	ModeHuman   = contract.ModeHuman
)

type OperationStatus = contract.OperationStatus

const (
	OperationAccepted   = contract.OperationAccepted
	OperationRunning    = contract.OperationRunning
	OperationSucceeded  = contract.OperationSucceeded
	OperationFailed     = contract.OperationFailed
	OperationCancelling = contract.OperationCancelling
	OperationCancelled  = contract.OperationCancelled
	OperationUnknown    = contract.OperationUnknown
)

type QuiesceStatus = contract.QuiesceStatus

const (
	QuiesceDraining = contract.QuiesceDraining
	QuiesceDrained  = contract.QuiesceDrained
	QuiesceFailed   = contract.QuiesceFailed
)

type HostFence = contract.HostFence

func WorkspaceScopedCapability(capability string) bool {
	return contract.WorkspaceScopedCapability(capability)
}

type CapabilitiesRequest = contract.CapabilitiesRequest
type CapabilitiesResponse = contract.CapabilitiesResponse
type ApplicationLoginStatus = contract.ApplicationLoginStatus

const (
	ApplicationLoginLoggedIn      = contract.ApplicationLoginLoggedIn
	ApplicationLoginNotLoggedIn   = contract.ApplicationLoginNotLoggedIn
	ApplicationLoginUnknown       = contract.ApplicationLoginUnknown
	ApplicationLoginNotApplicable = contract.ApplicationLoginNotApplicable
)

type ApplicationStatusItem = contract.ApplicationStatusItem
type ApplicationsResponse = contract.ApplicationsResponse
type ApplicationInstallRequest = contract.ApplicationInstallRequest
type ApplicationInstallResponse = contract.ApplicationInstallResponse
type ApplicationInstallOperation = contract.ApplicationInstallOperation

type ApplicationLaunchStatus = contract.ApplicationLaunchStatus

const (
	ApplicationLaunchStarting = contract.ApplicationLaunchStarting
	ApplicationLaunchRunning  = contract.ApplicationLaunchRunning
	ApplicationLaunchStopping = contract.ApplicationLaunchStopping
	ApplicationLaunchStopped  = contract.ApplicationLaunchStopped
	ApplicationLaunchExited   = contract.ApplicationLaunchExited
	ApplicationLaunchFailed   = contract.ApplicationLaunchFailed
	ApplicationLaunchUnknown  = contract.ApplicationLaunchUnknown
)

type ApplicationLaunchOperation = contract.ApplicationLaunchOperation
type ApplicationLaunchRequest = contract.ApplicationLaunchRequest
type ApplicationLaunchResponse = contract.ApplicationLaunchResponse

func ValidApplicationStatusItem(value ApplicationStatusItem) bool {
	return contract.ValidApplicationStatusItem(value) &&
		(value.InstallOperation == nil || ValidApplicationInstallOperation(*value.InstallOperation))
}

func ValidApplicationInstallOperation(value ApplicationInstallOperation) bool {
	return contract.ValidApplicationInstallOperation(value) && isUTC(value.CreatedAt) && isUTC(value.UpdatedAt)
}

func ValidApplicationLaunchOperation(value ApplicationLaunchOperation) bool {
	return contract.ValidApplicationLaunchOperation(value) && isUTC(value.CreatedAt) && isUTC(value.UpdatedAt)
}

type DrainedReceipt = contract.DrainedReceipt
type SetControlRequest = contract.SetControlRequest
type SetControlResponse = contract.SetControlResponse
type SetTaskLeaseRequest = contract.SetTaskLeaseRequest
type SetTaskLeaseResponse = contract.SetTaskLeaseResponse
type WorkspaceBinding = contract.WorkspaceBinding
type SetWorkspaceBindingsRequest = contract.SetWorkspaceBindingsRequest
type SetWorkspaceBindingsResponse = contract.SetWorkspaceBindingsResponse
type ExecuteRequest = contract.ExecuteRequest
type Operation = contract.Operation
type OperationResponse = contract.OperationResponse
type OperationQuery = contract.OperationQuery
type CancelOperationRequest = contract.CancelOperationRequest
type QuiesceRequest = contract.QuiesceRequest
type QuiesceResponse = contract.QuiesceResponse
