import XCTest
@testable import Minis

final class MIFLibraryViewsTests: XCTestCase {
    private let previewHandle = MIFLibraryPreviewHandle(
        id: UUID(uuidString: "00000000-0000-0000-0000-000000000001")!)

    func testTaskPreviewRequiresCompleteVerifiedWorkspaceProof() {
        let complete = artifact()
        XCTAssertEqual(preview(for: complete), previewHandle)

        XCTAssertNil(preview(for: artifact(workspaceID: nil)))
        XCTAssertNil(preview(for: artifact(workspaceID: "workspace")))
        XCTAssertNil(preview(for: artifact(workspaceRevision: nil)))
        XCTAssertNil(preview(for: artifact(workspaceRevision: -1)))
        XCTAssertNil(preview(for: artifact(sizeBytes: nil)))
        XCTAssertNil(preview(for: artifact(sizeBytes: -1)))
        XCTAssertNil(preview(for: artifact(sha256: nil)))
        XCTAssertNil(preview(for: artifact(sha256: String(repeating: "A", count: 64))))
        XCTAssertNil(preview(for: artifact(sha256: "short")))
        XCTAssertNil(preview(for: artifact(verified: false)))
        XCTAssertNil(preview(for: artifact(verified: nil)))
        XCTAssertNil(preview(for: artifact(previewHandle: nil)))
        XCTAssertNil(preview(for: artifact(id: "")))
    }

    func testTaskPreviewFailsClosedWithoutPermissionOrCapability() {
        let complete = artifact()
        XCTAssertNil(MIFLibraryPreviewPolicy.taskHandle(
            for: complete, access: .denied, workspaceSyncAvailable: true, taskMediaGenerationAvailable: true))
        XCTAssertNil(MIFLibraryPreviewPolicy.taskHandle(
            for: complete, access: .loading, workspaceSyncAvailable: true, taskMediaGenerationAvailable: true))
        XCTAssertNil(MIFLibraryPreviewPolicy.taskHandle(
            for: complete, access: .unavailable, workspaceSyncAvailable: true, taskMediaGenerationAvailable: true))
        XCTAssertNil(MIFLibraryPreviewPolicy.taskHandle(
            for: complete, access: .available, workspaceSyncAvailable: true, taskMediaGenerationAvailable: false))
        XCTAssertNil(MIFLibraryPreviewPolicy.taskHandle(
            for: complete, access: .available, workspaceSyncAvailable: true, taskMediaGenerationAvailable: nil))
    }

    func testTaskPreviewRequiresWorkspaceSyncForEveryArtifact() {
        let document = artifact(mediaType: "application/pdf")
        XCTAssertNil(MIFLibraryPreviewPolicy.taskHandle(
            for: document, access: .available, workspaceSyncAvailable: false,
            taskMediaGenerationAvailable: true))
        XCTAssertNil(MIFLibraryPreviewPolicy.taskHandle(
            for: document, access: .available, workspaceSyncAvailable: nil,
            taskMediaGenerationAvailable: true))
    }

    func testNonMediaTaskArtifactDoesNotRequireMediaGenerationCapability() {
        let document = artifact(mediaType: "application/pdf")
        XCTAssertEqual(MIFLibraryPreviewPolicy.taskHandle(
            for: document, access: .available, workspaceSyncAvailable: true,
            taskMediaGenerationAvailable: false), previewHandle)
        XCTAssertEqual(MIFLibraryPreviewPolicy.taskHandle(
            for: document, access: .available, workspaceSyncAvailable: true,
            taskMediaGenerationAvailable: nil), previewHandle)
    }

    func testLocalPreviewNeedsAvailableSourceAndOpaqueHandle() {
        let item = MIFLibraryLocalItem(id: UUID(), name: "report.pdf", kind: .artifact,
                                       sizeBytes: 10, modifiedAt: nil,
                                       origin: .conversation(title: "Synthetic chat"),
                                       previewHandle: previewHandle)
        XCTAssertEqual(MIFLibraryPreviewPolicy.localHandle(for: item, access: .available), previewHandle)
        XCTAssertNil(MIFLibraryPreviewPolicy.localHandle(for: item, access: .denied))
        XCTAssertNil(MIFLibraryPreviewPolicy.localHandle(for: item, access: .loading))
        XCTAssertNil(MIFLibraryPreviewPolicy.localHandle(for: item, access: .unavailable))

        let metadataOnly = MIFLibraryLocalItem(id: UUID(), name: "report.pdf", kind: .artifact,
                                               sizeBytes: 10, modifiedAt: nil, origin: nil,
                                               previewHandle: nil)
        XCTAssertNil(MIFLibraryPreviewPolicy.localHandle(for: metadataOnly, access: .available))
    }

    private func preview(for artifact: MIFLibraryTaskArtifact) -> MIFLibraryPreviewHandle? {
        MIFLibraryPreviewPolicy.taskHandle(
            for: artifact, access: .available, workspaceSyncAvailable: true,
            taskMediaGenerationAvailable: true)
    }

    private func artifact(id: String = "a1234567",
                          mediaType: String = "image/png",
                          workspaceID: String? = "wabc1234",
                          workspaceRevision: Int64? = 3,
                          sizeBytes: Int64? = 64,
                          sha256: String? = String(repeating: "a", count: 64),
                          verified: Bool? = true,
                          previewHandle: MIFLibraryPreviewHandle? = MIFLibraryPreviewHandle(
                            id: UUID(uuidString: "00000000-0000-0000-0000-000000000001")!)) -> MIFLibraryTaskArtifact {
        MIFLibraryTaskArtifact(id: id, label: "Synthetic output", mediaType: mediaType,
                               workspaceID: workspaceID, workspaceRevision: workspaceRevision,
                               sizeBytes: sizeBytes, sha256: sha256, verified: verified,
                               previewHandle: previewHandle)
    }
}
