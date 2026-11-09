import XCTest
@testable import Minis

final class MIFApprovalViewsTests: XCTestCase {
    func testSummaryCountsPendingApprovalAndExcludesClarification() throws {
        let approval = try task(
            id: "q1234567",
            pending: #"{"id":"r1234567","version":2,"kind":"approval","title":"Approve change","object":"Workspace file","content":"Save the generated output?","impact":"Publishes one file."}"#,
            clarification: "null"
        )
        let clarification = try task(
            id: "q2345678",
            pending: "null",
            clarification: #"{"id":"r2345678","version":1,"question":"Do you need a file?"}"#
        )

        let summary = MIFApprovalInboxSummary.make(
            taskIDs: [approval.id, clarification.id],
            details: [approval.id: approval, clarification.id: clarification],
            failedTaskIDs: [], pageReadFailed: false
        )

        XCTAssertEqual(summary.approvals.map(\.id), ["q1234567:r1234567:2"])
        XCTAssertEqual(summary.unresolvedCount, 0)
        XCTAssertFalse(summary.isPartial)
    }

    func testEventChainIsAssignedOnlyToFirstPendingApproval() throws {
        let first = try task(
            id: "q1234567",
            pending: #"{"id":"r1234567","version":1,"kind":"approval","title":"First approval","object":"Object","content":"Content","impact":"Impact"}"#,
            clarification: "null"
        )
        let second = try task(
            id: "q2345678",
            pending: #"{"id":"r2345678","version":1,"kind":"approval","title":"Second approval","object":"Object","content":"Content","impact":"Impact"}"#,
            clarification: "null"
        )

        let summary = MIFApprovalInboxSummary.make(
            taskIDs: [first.id, second.id],
            details: [first.id: first, second.id: second],
            failedTaskIDs: [], pageReadFailed: false
        )

        XCTAssertEqual(summary.eventChainApprovalID, summary.approvals.first?.id)
        XCTAssertNotEqual(summary.eventChainApprovalID, summary.approvals.last?.id)

        let emptySummary = MIFApprovalInboxSummary.make(
            taskIDs: [], details: [:], failedTaskIDs: [], pageReadFailed: false
        )
        XCTAssertNil(emptySummary.eventChainApprovalID)
    }

    func testPartialReadShowsKnownApprovalsButNeverConfirmsZero() throws {
        let approval = try task(
            id: "q1234567",
            pending: #"{"id":"r1234567","version":1,"kind":"approval","title":"Approve change","object":"Workspace file","content":"Save the generated output?","impact":"Publishes one file."}"#,
            clarification: "null"
        )

        let summary = MIFApprovalInboxSummary.make(
            taskIDs: [approval.id, "q2345678"],
            details: [approval.id: approval],
            failedTaskIDs: ["q2345678"], pageReadFailed: true
        )

        XCTAssertEqual(summary.approvals.map(\.task.id), [approval.id])
        XCTAssertTrue(summary.isPartial)
        XCTAssertFalse(summary.isConfirmedEmpty)
        XCTAssertGreaterThan(summary.unresolvedCount, 0)
    }

    func testClarificationOnlyIsAConfirmedEmptyApprovalInbox() throws {
        let clarification = try task(
            id: "q2345678",
            pending: "null",
            clarification: #"{"id":"r2345678","version":1,"question":"Do you need a file?"}"#
        )

        let summary = MIFApprovalInboxSummary.make(
            taskIDs: [clarification.id], details: [clarification.id: clarification],
            failedTaskIDs: [], pageReadFailed: false
        )

        XCTAssertTrue(summary.approvals.isEmpty)
        XCTAssertTrue(summary.isConfirmedEmpty)
        XCTAssertFalse(summary.isPartial)
    }

    func testFilteredApprovalPageRequiresWaitingUserStatus() {
        XCTAssertTrue(MIFApprovalInboxSummary.isPendingPageStatus("waiting_user"))
        XCTAssertFalse(MIFApprovalInboxSummary.isPendingPageStatus("completed"))
        XCTAssertFalse(MIFApprovalInboxSummary.isPendingPageStatus("unknown"))
    }

    func testApprovalWithoutKindMatchesTaskPageResponseContract() throws {
        let approval = try task(
            id: "q3456789",
            pending: #"{"id":"r3456789","version":1,"title":"Approve request","object":"Workspace file","content":"Save this file?","impact":"Publishes one file."}"#,
            clarification: "null"
        )

        let summary = MIFApprovalInboxSummary.make(
            taskIDs: [approval.id], details: [approval.id: approval], failedTaskIDs: [], pageReadFailed: false
        )

        XCTAssertEqual(summary.approvals.map(\.task.id), [approval.id])
        XCTAssertFalse(summary.isPartial)
    }

    func testUnknownPendingRequestKindIsNotShownAsAnApproval() throws {
        let task = try task(
            id: "q1234567",
            pending: #"{"id":"r1234567","version":1,"kind":"future_kind","title":"Request","object":"Object","content":"Content","impact":"Impact"}"#,
            clarification: "null"
        )

        let summary = MIFApprovalInboxSummary.make(
            taskIDs: [task.id], details: [task.id: task], failedTaskIDs: [], pageReadFailed: false
        )

        XCTAssertTrue(summary.approvals.isEmpty)
        XCTAssertTrue(summary.isPartial)
        XCTAssertFalse(summary.isConfirmedEmpty)
    }

    func testNonWaitingApprovalIsUnresolvedUntilDetailClearsPendingRequest() throws {
        let stale = try task(
            id: "q1234567",
            status: "completed",
            pending: #"{"id":"r1234567","version":1,"kind":"approval","title":"Approve change","object":"Workspace file","content":"Save the generated output?","impact":"Publishes one file."}"#,
            clarification: "null"
        )

        let conflictingSummary = MIFApprovalInboxSummary.make(
            taskIDs: [stale.id], details: [stale.id: stale], failedTaskIDs: [], pageReadFailed: false
        )

        XCTAssertTrue(conflictingSummary.approvals.isEmpty)
        XCTAssertTrue(conflictingSummary.isPartial)
        XCTAssertFalse(conflictingSummary.isConfirmedEmpty)
        XCTAssertGreaterThan(conflictingSummary.unresolvedCount, 0)

        let refreshedWithoutPending = try task(
            id: stale.id,
            status: "completed",
            pending: "null",
            clarification: "null"
        )
        let clearedSummary = MIFApprovalInboxSummary.make(
            taskIDs: [refreshedWithoutPending.id],
            details: [refreshedWithoutPending.id: refreshedWithoutPending],
            failedTaskIDs: [],
            pageReadFailed: false
        )

        XCTAssertTrue(clearedSummary.approvals.isEmpty)
        XCTAssertTrue(clearedSummary.isConfirmedEmpty)
    }

    func testMediaApprovalRequiresPreviewAndCannotBeDecidedFromInbox() throws {
        let ordinary = try task(
            id: "q1234567",
            pending: #"{"id":"r1234567","version":1,"kind":"approval","title":"Approve change","object":"Workspace file","content":"Save the generated output?","impact":"Publishes one file."}"#,
            clarification: "null"
        )
        let media = try task(
            id: "q2345678",
            pending: #"{"id":"r2345678","version":1,"kind":"approval","title":"Save image","object":"Image","content":"Review the image.","impact":"Publish to Workspace.","media_preview_operation_id":"o1234567"}"#,
            clarification: "null"
        )

        XCTAssertTrue(MIFApprovalInboxPolicy.canRespond(pending: try XCTUnwrap(ordinary.pending_request)))
        XCTAssertFalse(MIFApprovalInboxPolicy.canRespond(pending: try XCTUnwrap(media.pending_request)))
    }

    func testAcceptedDecisionIsScopedAndPersistsAcrossRefreshes() {
        var ledger = MIFApprovalSubmissionLedger()
        ledger.use(scopeKey: "account-a")
        ledger.markAccepted("task:approval:1", scopeKey: "account-a")

        XCTAssertTrue(ledger.contains("task:approval:1", scopeKey: "account-a"))

        ledger.use(scopeKey: "account-a")
        XCTAssertTrue(ledger.contains("task:approval:1", scopeKey: "account-a"))

        ledger.use(scopeKey: "account-b")
        XCTAssertFalse(ledger.contains("task:approval:1", scopeKey: "account-b"))
        XCTAssertFalse(ledger.contains("task:approval:1", scopeKey: "account-a"))
    }

    private func task(id: String, status: String = "waiting_user", pending: String,
                      clarification: String) throws -> MordeTask {
        let json = #"{"id":"\#(id)","conversation_id":"c1234567","objective":"Review the requested change","status":"\#(status)","step_count":0,"completed_step_count":0,"updated_at":"2026-11-04T00:00:00Z","pending_request":\#(pending),"clarification_request":\#(clarification)}"#
        return try JSONDecoder().decode(MordeTask.self, from: Data(json.utf8))
    }
}
