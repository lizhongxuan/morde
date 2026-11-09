import Foundation
import XCTest
@testable import Minis

final class MIFWorkItemViewsTests: XCTestCase {
    func testCategoryFiltersAndHistoricalUnclassifiedItems() throws {
        let bug = try workItem(state: "open", category: "bug", assigneeID: "u1234567")
        let request = try workItem(state: "blocked", category: "request", assigneeID: "u1234567")
        let historical = try workItem(state: "open", category: nil, assigneeID: "u1234567")

        XCTAssertEqual(MIFWorkItemPresentation.categoryLabel(bug.category), "缺陷")
        XCTAssertEqual(MIFWorkItemPresentation.categoryLabel(request.category), "需求")
        XCTAssertEqual(MIFWorkItemPresentation.categoryLabel(historical.category), "未分类")
        XCTAssertEqual(MIFWorkItemPresentation.categoryLabel("unclassified"), "未分类")
        XCTAssertEqual(MIFWorkItemPresentation.categoryMarkerLabel("bug"), "缺")
        XCTAssertEqual(MIFWorkItemPresentation.categoryMarkerLabel("request"), "需")
        XCTAssertNil(MIFWorkItemPresentation.categoryMarkerLabel("unclassified"))
        XCTAssertTrue(MIFWorkItemPresentation.matches(bug, filter: .all))
        XCTAssertTrue(MIFWorkItemPresentation.matches(bug, filter: .bug))
        XCTAssertFalse(MIFWorkItemPresentation.matches(bug, filter: .request))
        XCTAssertTrue(MIFWorkItemPresentation.matches(request, filter: .request))
        XCTAssertFalse(MIFWorkItemPresentation.matches(historical, filter: .bug))
        XCTAssertFalse(MIFWorkItemPresentation.matches(historical, filter: .request))
        XCTAssertTrue(MIFWorkItemPresentation.matches(historical, filter: .all))
        XCTAssertEqual(MIFWorkItemPresentation.stateLabel("open"), "待处理")
        XCTAssertNotEqual(MIFWorkItemPresentation.stateLabel("open"), "待认领")
    }

    func testUnclaimedFilterIsIndependentOfOpenState() throws {
        let blockedUnassigned = try workItem(state: "blocked", category: "bug", assigneeID: nil)
        let openAssigned = try workItem(state: "open", category: "request", assigneeID: "u1234567")

        XCTAssertTrue(MIFWorkItemPresentation.matches(blockedUnassigned, filter: .unclaimed))
        XCTAssertFalse(MIFWorkItemPresentation.matches(openAssigned, filter: .unclaimed))
        XCTAssertEqual(MIFWorkItemPresentation.assigneeLabel(blockedUnassigned.assignee_id), "待认领")
        XCTAssertEqual(MIFWorkItemPresentation.assigneeLabel(openAssigned.assignee_id), "已分配给 u1234567")
        XCTAssertTrue(MIFWorkItemPresentation.canClaim(item: blockedUnassigned,
                                                       canClaim: true, role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canClaim(item: openAssigned,
                                                        canClaim: true, role: "member"))
    }

    func testClaimRequiresPermissionUnassignedAndNotDelivered() throws {
        let openUnassigned = try workItem(state: "open", assigneeID: nil)
        let deliveredUnassigned = try workItem(state: "delivered", assigneeID: nil)

        XCTAssertTrue(MIFWorkItemPresentation.canClaim(item: openUnassigned,
                                                       canClaim: true, role: "owner"))
        XCTAssertTrue(MIFWorkItemPresentation.canClaim(item: openUnassigned,
                                                       canClaim: true, role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canClaim(item: openUnassigned,
                                                        canClaim: true, role: "viewer"))
        XCTAssertFalse(MIFWorkItemPresentation.canClaim(item: openUnassigned,
                                                        canClaim: false, role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canClaim(item: deliveredUnassigned,
                                                        canClaim: true, role: "member"))
        XCTAssertEqual(MIFWorkItemPresentation.stateLabel("delivered"), "已交付")
    }

    func testCreationRequiresV2CapabilityAndManagerRole() {
        XCTAssertFalse(MIFWorkItemPresentation.canCreate(supportsV2: false, role: "owner"))
        XCTAssertFalse(MIFWorkItemPresentation.canCreate(supportsV2: false, role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canCreate(supportsV2: true, role: "viewer"))
        XCTAssertTrue(MIFWorkItemPresentation.canCreate(supportsV2: true, role: "owner"))
        XCTAssertTrue(MIFWorkItemPresentation.canCreate(supportsV2: true, role: "member"))
    }

    func testEditingRequiresOwnerOrMemberAndMatchesCurrentState() throws {
        let open = try workItem(state: "open")
        let blocked = try workItem(state: "blocked", blockedReason: "等待确认")

        XCTAssertTrue(MIFWorkItemPresentation.canManage(role: "owner"))
        XCTAssertTrue(MIFWorkItemPresentation.canManage(role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canManage(role: "viewer"))
        XCTAssertTrue(MIFWorkItemPresentation.canBlock(item: open, canManage: true))
        XCTAssertFalse(MIFWorkItemPresentation.canBlock(item: open, canManage: false))
        XCTAssertTrue(MIFWorkItemPresentation.canResume(item: blocked, canManage: true))
        XCTAssertFalse(MIFWorkItemPresentation.canResume(item: blocked, canManage: false))
    }

    func testTaskLinkUsesExistingMembershipAndItemPolicy() throws {
        let open = try workItem(state: "open")
        let linked = try workItem(state: "open", taskIDs: ["q1234567"])
        let delivered = try workItem(state: "delivered", taskIDs: [])

        XCTAssertTrue(MIFWorkItemPresentation.canLinkTask(item: open, taskID: "q1234567", role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canLinkTask(item: open, taskID: "q1234567", role: "viewer"))
        XCTAssertFalse(MIFWorkItemPresentation.canLinkTask(item: linked, taskID: "q1234567", role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canLinkTask(item: delivered, taskID: "q1234567", role: "member"))
    }

    func testDeliveryRequiresLinkedTaskReadyWorkspaceAndManagerRole() throws {
        let item = try workItem(state: "open", taskIDs: ["q1234567"])
        let target = MIFWorkItemDeliveryTarget(hostID: "h1234567", hostName: "Host",
                                               workspaceID: "w1234567", workspaceName: "Workspace", revision: 2)

        XCTAssertTrue(MIFWorkItemPresentation.canRecordDelivery(
            item: item, taskID: "q1234567", target: target, role: "owner"))
        XCTAssertFalse(MIFWorkItemPresentation.canRecordDelivery(
            item: item, taskID: "q1234567", target: target, role: "viewer"))
        XCTAssertFalse(MIFWorkItemPresentation.canRecordDelivery(
            item: try workItem(state: "open"), taskID: "q1234567", target: target, role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canRecordDelivery(
            item: item, taskID: "q1234567", target: MIFWorkItemDeliveryTarget(
                hostID: "h1234567", hostName: "Host", workspaceID: "w1234567",
                workspaceName: "Workspace", revision: 0), role: "member"))
    }

    private func workItem(state: String, blockedReason: String = "", taskIDs: [String] = [],
                          category: String? = nil, assigneeID: String? = "u1234567") throws -> MordeProjectWorkItem {
        var value: [String: Any] = [
            "id": "j7654321",
            "project_id": "j1234567",
            "title": "Example",
            "description": "",
            "acceptance_criteria": [String](),
            "state": state,
            "blocked_reason": blockedReason,
            "task_ids": taskIDs,
            "version": 1
        ]
        if let category { value["category"] = category }
        value["assignee_id"] = assigneeID.map { $0 as Any } ?? NSNull()
        return try JSONDecoder().decode(MordeProjectWorkItem.self,
                                        from: JSONSerialization.data(withJSONObject: value))
    }
}
