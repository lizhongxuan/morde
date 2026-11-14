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

    func testApprovalDisclosureContractRendersKnownValuesAndNotApplicableState() throws {
        let task = try task(
            id: "q3456789",
            pending: #"{"id":"r3456789","version":1,"kind":"approval","title":"Review","content":"Proceed?","impact":"Updates a file.","disclosures":{"risk":{"state":"known","summary":"May overwrite the selected file."},"this_cost":{"state":"known","basis":"estimate","amount_micros":1250000,"currency":"USD"},"cumulative_cost":{"state":"unknown","basis":null,"amount_micros":null,"currency":null},"alternatives":{"state":"known","options":["Preview only","Cancel"]},"revocation":{"state":"not_applicable","summary":null}}}"#,
            clarification: "null"
        )
        let disclosures = try XCTUnwrap(task.pending_request?.disclosures)

        XCTAssertEqual(MordeTaskApprovalDisclosurePresentation.text(disclosures.risk), "May overwrite the selected file.")
        XCTAssertEqual(MordeTaskApprovalDisclosurePresentation.cost(disclosures.this_cost, locale: Locale(identifier: "en_US")),
                       "\(AppLocalized("Estimate")): USD 1.25")
        XCTAssertEqual(MordeTaskApprovalDisclosurePresentation.cost(disclosures.cumulative_cost), AppLocalized("Unknown"))
        XCTAssertEqual(MordeTaskApprovalDisclosurePresentation.alternatives(disclosures.alternatives), "• Preview only\n• Cancel")
        XCTAssertEqual(MordeTaskApprovalDisclosurePresentation.text(disclosures.revocation), AppLocalized("Not applicable"))

        XCTAssertEqual(MordeTaskApprovalDisclosurePresentation.lines(disclosures), [
            "\(AppLocalized("Risk")): May overwrite the selected file.",
            "\(AppLocalized("Cost of this action")): \(AppLocalized("Estimate")): USD 1.25",
            "\(AppLocalized("Cumulative cost")): \(AppLocalized("Unknown"))",
            "\(AppLocalized("Alternatives")): • Preview only\n• Cancel",
            "\(AppLocalized("Revocation method")): \(AppLocalized("Not applicable"))",
        ])
    }

    func testUnknownApprovalDisclosuresAndMissingDisclosuresStayExplicitlyUnknown() throws {
        let unknown = try task(
            id: "q4567890",
            pending: #"{"id":"r4567890","version":1,"kind":"approval","title":"Review","content":"Proceed?","impact":"Updates a file.","disclosures":{"risk":{"state":"unknown","summary":null},"this_cost":{"state":"unknown","basis":null,"amount_micros":null,"currency":null},"cumulative_cost":{"state":"unknown","basis":null,"amount_micros":null,"currency":null},"alternatives":{"state":"unknown","options":[]},"revocation":{"state":"unknown","summary":null}}}"#,
            clarification: "null"
        )
        let unknownDisclosures = try XCTUnwrap(unknown.pending_request?.disclosures)
        let expectedUnknownLines = [
            "\(AppLocalized("Risk")): \(AppLocalized("Unknown"))",
            "\(AppLocalized("Cost of this action")): \(AppLocalized("Unknown"))",
            "\(AppLocalized("Cumulative cost")): \(AppLocalized("Unknown"))",
            "\(AppLocalized("Alternatives")): \(AppLocalized("Unknown"))",
            "\(AppLocalized("Revocation method")): \(AppLocalized("Unknown"))",
        ]
        XCTAssertEqual(MordeTaskApprovalDisclosurePresentation.lines(unknownDisclosures), expectedUnknownLines)

        let legacy = try task(
            id: "q5678901",
            pending: #"{"id":"r5678901","version":1,"kind":"approval","title":"Legacy","content":"Proceed?","impact":"Updates a file."}"#,
            clarification: "null"
        )
        XCTAssertNil(legacy.pending_request?.disclosures)
        XCTAssertEqual(MordeTaskApprovalDisclosurePresentation.lines(legacy.pending_request?.disclosures), expectedUnknownLines)
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

        XCTAssertTrue(MIFApprovalInboxPolicy.canRespond(pending: try XCTUnwrap(ordinary.pending_request), taskStatus: ordinary.status))
        XCTAssertFalse(MIFApprovalInboxPolicy.canRespond(pending: try XCTUnwrap(media.pending_request), taskStatus: media.status))
    }

    func testExpiredApprovalIsReadOnlyInInboxAndNotCountedAsPending() throws {
        let expired = try task(
            id: "q1234567", status: "running",
            pending: #"{"id":"r1234567","version":1,"kind":"approval","title":"Approve change","object":"Workspace file","content":"Save the output?","impact":"Publishes one file.","status":"expired","expires_at":"2000-01-01T00:00:00Z"}"#,
            clarification: "null"
        )

        let summary = MIFApprovalInboxSummary.make(
            taskIDs: [expired.id], details: [expired.id: expired], failedTaskIDs: [], pageReadFailed: false
        )
        let pending = try XCTUnwrap(expired.pending_request)

        XCTAssertEqual(summary.approvals.map(\.task.id), [expired.id])
        XCTAssertEqual(summary.pendingCount, 0)
        XCTAssertFalse(summary.isPartial)
        XCTAssertEqual(summary.confirmedCount, MIFApprovalCountSnapshot(count: 0, validUntil: nil))
        XCTAssertTrue(pending.isExpired)
        XCTAssertFalse(MIFApprovalInboxPolicy.canRespond(pending: pending, taskStatus: expired.status))
    }

    func testApprovalDeadlinePreventsActionsAndLegacyPendingRemainsCompatible() throws {
        let passed = try task(
            id: "q1234567",
            pending: #"{"id":"r1234567","version":1,"kind":"approval","title":"Expired by time","object":"Workspace file","content":"Save the output?","impact":"Publishes one file.","status":"pending","expires_at":"2000-01-01T00:00:00Z"}"#,
            clarification: "null"
        )
        let legacy = try task(
            id: "q2345678",
            pending: #"{"id":"r2345678","version":1,"kind":"approval","title":"Legacy approval","object":"Workspace file","content":"Save the output?","impact":"Publishes one file."}"#,
            clarification: "null"
        )

        XCTAssertFalse(MIFApprovalInboxPolicy.canRespond(pending: try XCTUnwrap(passed.pending_request), taskStatus: passed.status))
        let legacyPending = try XCTUnwrap(legacy.pending_request)
        XCTAssertNil(legacyPending.status)
        XCTAssertNil(legacyPending.expires_at)
        XCTAssertTrue(MIFApprovalInboxPolicy.canRespond(pending: legacyPending, taskStatus: legacy.status))
    }

    func testFractionalExpiryParsesAndMalformedExpiryFailsClosed() throws {
        let fractional = try task(
            id: "q1234567",
            pending: #"{"id":"r1234567","version":1,"kind":"approval","title":"Fractional expiry","object":"Workspace file","content":"Save the output?","impact":"Publishes one file.","status":"pending","expires_at":"2099-12-31T23:59:59.123456Z"}"#,
            clarification: "null"
        )
        let malformed = try task(
            id: "q2345678",
            pending: #"{"id":"r2345678","version":1,"kind":"approval","title":"Malformed expiry","object":"Workspace file","content":"Save the output?","impact":"Publishes one file.","status":"pending","expires_at":"tomorrow"}"#,
            clarification: "null"
        )

        let fractionalPending = try XCTUnwrap(fractional.pending_request)
        let malformedPending = try XCTUnwrap(malformed.pending_request)
        XCTAssertNotNil(fractionalPending.expirationDate)
        XCTAssertTrue(MIFApprovalInboxPolicy.canRespond(pending: fractionalPending, taskStatus: fractional.status,
                                                        at: Date(timeIntervalSince1970: 2_000_000_000)))
        XCTAssertTrue(malformedPending.hasInvalidExpiration)
        XCTAssertFalse(MIFApprovalInboxPolicy.canRespond(pending: malformedPending, taskStatus: malformed.status))
        let summary = MIFApprovalInboxSummary.make(taskIDs: [malformed.id], details: [malformed.id: malformed],
                                                   failedTaskIDs: [], pageReadFailed: false)
        XCTAssertTrue(summary.isPartial)
        XCTAssertFalse(summary.isConfirmedEmpty)
    }

    func testLocalDeadlineCannotConfirmAnApprovalExpiredOrAZeroCount() throws {
        let task = try task(
            id: "q1234567",
            pending: #"{"id":"r1234567","version":1,"kind":"approval","title":"Review","content":"Proceed?","impact":"A change","status":"pending","expires_at":"2030-01-01T00:00:00Z"}"#,
            clarification: "null"
        )
        let pending = try XCTUnwrap(task.pending_request)
        let deadline = try XCTUnwrap(pending.expirationDate)
        let before = MIFApprovalInboxSummary.make(taskIDs: [task.id], details: [task.id: task],
                                                 failedTaskIDs: [], pageReadFailed: false, at: deadline.addingTimeInterval(-1))
        let atDeadline = MIFApprovalInboxSummary.make(taskIDs: [task.id], details: [task.id: task],
                                                     failedTaskIDs: [], pageReadFailed: false, at: deadline)
        XCTAssertEqual(before.pendingCount, 1)
        XCTAssertFalse(before.isPartial)
        XCTAssertEqual(before.confirmedCount, MIFApprovalCountSnapshot(count: 1, validUntil: deadline))
        XCTAssertTrue(atDeadline.isPartial)
        XCTAssertFalse(atDeadline.isConfirmedEmpty)
        XCTAssertEqual(atDeadline.confirmedCount, .unknown)
        XCTAssertFalse(pending.isExpired)
        XCTAssertFalse(pending.canRespond(taskStatus: task.status, at: deadline))
    }

    @MainActor
    func testApprovalBadgeExpiresAfterInboxIsClosed() async throws {
        let tracker = MIFApprovalCountTracker()
        tracker.update(MIFApprovalCountSnapshot(count: 2, validUntil: Date().addingTimeInterval(0.05)))
        XCTAssertEqual(tracker.count, 2)
        for _ in 0..<50 where tracker.count != nil {
            try await Task.sleep(nanoseconds: 20_000_000)
        }
        XCTAssertNil(tracker.count)
    }

    @MainActor
    func testNewApprovalBadgeSnapshotCancelsOlderDeadline() async throws {
        let tracker = MIFApprovalCountTracker()
        tracker.update(MIFApprovalCountSnapshot(count: 1, validUntil: Date().addingTimeInterval(0.05)))
        tracker.update(MIFApprovalCountSnapshot(count: 3, validUntil: nil))
        try await Task.sleep(nanoseconds: 150_000_000)
        XCTAssertEqual(tracker.count, 3)
        tracker.clear()
        XCTAssertNil(tracker.count)
    }

    func testApprovalActionRequiresMatchingTaskStateAndValidRequest() throws {
        let approval = try task(
            id: "q1234567",
            pending: #"{"id":"r1234567","version":1,"kind":"approval","title":"Review","content":"Proceed?","impact":"A change"}"#,
            clarification: "null"
        )
        let pending = try XCTUnwrap(approval.pending_request)
        XCTAssertTrue(pending.canRespond(taskStatus: "waiting_user"))
        XCTAssertFalse(pending.canRespond(taskStatus: "running"))
        let invalid = try task(
            id: "q2345678",
            pending: #"{"id":"invalid","version":0,"kind":"approval","title":"Review","content":"Proceed?","impact":"A change"}"#,
            clarification: "null"
        )
        XCTAssertFalse(try XCTUnwrap(invalid.pending_request).canRespond(taskStatus: invalid.status))
        let summary = MIFApprovalInboxSummary.make(taskIDs: [invalid.id], details: [invalid.id: invalid],
                                                   failedTaskIDs: [], pageReadFailed: false)
        XCTAssertTrue(summary.isPartial)
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
