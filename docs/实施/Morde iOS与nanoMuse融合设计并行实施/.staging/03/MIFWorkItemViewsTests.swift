import XCTest
@testable import Minis

final class MIFWorkItemViewsTests: XCTestCase {
    func testOpenWorkItemDoesNotReadAsUnclaimed() {
        XCTAssertEqual(MIFWorkItemPresentation.stateLabel("open"), "开放")
        XCTAssertNotEqual(MIFWorkItemPresentation.stateLabel("open"), "待认领")
        XCTAssertEqual(MIFWorkItemPresentation.stateLabel("blocked"), "已阻塞")
        XCTAssertEqual(MIFWorkItemPresentation.stateLabel("delivered"), "已交付")
    }

    func testEditingRequiresOwnerOrMemberAndMatchesCurrentState() {
        let open = workItem(state: "open")
        let blocked = workItem(state: "blocked", blockedReason: "等待确认")

        XCTAssertTrue(MIFWorkItemPresentation.canManage(role: "owner"))
        XCTAssertTrue(MIFWorkItemPresentation.canManage(role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canManage(role: "viewer"))
        XCTAssertTrue(MIFWorkItemPresentation.canBlock(item: open, canManage: true))
        XCTAssertFalse(MIFWorkItemPresentation.canBlock(item: open, canManage: false))
        XCTAssertTrue(MIFWorkItemPresentation.canResume(item: blocked, canManage: true))
        XCTAssertFalse(MIFWorkItemPresentation.canResume(item: blocked, canManage: false))
    }

    func testTaskLinkUsesExistingMembershipAndItemPolicy() {
        let open = workItem(state: "open")
        let linked = workItem(state: "open", taskIDs: ["q1234567"])
        let delivered = workItem(state: "delivered", taskIDs: [])

        XCTAssertTrue(MIFWorkItemPresentation.canLinkTask(item: open, taskID: "q1234567", role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canLinkTask(item: open, taskID: "q1234567", role: "viewer"))
        XCTAssertFalse(MIFWorkItemPresentation.canLinkTask(item: linked, taskID: "q1234567", role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canLinkTask(item: delivered, taskID: "q1234567", role: "member"))
    }

    func testDeliveryRequiresLinkedTaskReadyWorkspaceAndManagerRole() {
        let item = workItem(state: "open", taskIDs: ["q1234567"])
        let target = MIFWorkItemDeliveryTarget(hostID: "h1234567", hostName: "Host",
                                               workspaceID: "w1234567", workspaceName: "Workspace", revision: 2)

        XCTAssertTrue(MIFWorkItemPresentation.canRecordDelivery(
            item: item, taskID: "q1234567", target: target, role: "owner"))
        XCTAssertFalse(MIFWorkItemPresentation.canRecordDelivery(
            item: item, taskID: "q1234567", target: target, role: "viewer"))
        XCTAssertFalse(MIFWorkItemPresentation.canRecordDelivery(
            item: workItem(state: "open"), taskID: "q1234567", target: target, role: "member"))
        XCTAssertFalse(MIFWorkItemPresentation.canRecordDelivery(
            item: item, taskID: "q1234567", target: MIFWorkItemDeliveryTarget(
                hostID: "h1234567", hostName: "Host", workspaceID: "w1234567",
                workspaceName: "Workspace", revision: 0), role: "member"))
    }

    private func workItem(state: String, blockedReason: String = "", taskIDs: [String] = []) -> MordeProjectWorkItem {
        MordeProjectWorkItem(id: "j7654321", project_id: "j1234567", title: "Example", description: "",
                             acceptance_criteria: [], assignee_id: "u1234567", state: state,
                             blocked_reason: blockedReason, task_ids: taskIDs, delivery: nil, version: 1)
    }
}
