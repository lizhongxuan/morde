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

    func testStaleApprovalIsExcludedWhenDetailNoLongerWaitsForUser() throws {
        let stale = try task(
            id: "q1234567",
            status: "completed",
            pending: #"{"id":"r1234567","version":1,"kind":"approval","title":"Approve change","object":"Workspace file","content":"Save the generated output?","impact":"Publishes one file."}"#,
            clarification: "null"
        )

        let summary = MIFApprovalInboxSummary.make(
            taskIDs: [stale.id], details: [stale.id: stale], failedTaskIDs: [], pageReadFailed: false
        )

        XCTAssertTrue(summary.approvals.isEmpty)
        XCTAssertTrue(summary.isConfirmedEmpty)
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

    private func task(id: String, status: String = "waiting_user", pending: String,
                      clarification: String) throws -> MordeTask {
        let json = #"{"id":"\#(id)","conversation_id":"c1234567","objective":"Review the requested change","status":"\#(status)","updated_at":"2026-11-04T00:00:00Z","pending_request":\#(pending),"clarification_request":\#(clarification)}"#
        return try JSONDecoder().decode(MordeTask.self, from: Data(json.utf8))
    }
}
