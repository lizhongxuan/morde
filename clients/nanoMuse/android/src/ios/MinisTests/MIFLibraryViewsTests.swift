import XCTest
@testable import Minis

final class MIFLibraryViewsTests: XCTestCase {
    private let previewHandle = MIFLibraryPreviewHandle(
        id: UUID(uuidString: "00000000-0000-0000-0000-000000000001")!)

    func testLibrarySourcesMatchDesignAndUploadsRequireAnAttachment() {
        XCTAssertEqual(MIFLibrarySource.allCases, [.all, .aiOutputs, .myUploads])

        let uploaded = accountItem(attachmentID: "pa_0123456789abcdef0123456789abcdef")
        let metadataOnly = accountItem(attachmentID: nil)
        let invalid = accountItem(attachmentID: "not-an-attachment")

        XCTAssertTrue(MIFLibrarySource.all.includes(uploaded))
        XCTAssertTrue(MIFLibrarySource.all.includes(metadataOnly))
        XCTAssertFalse(MIFLibrarySource.aiOutputs.includes(uploaded))
        XCTAssertTrue(MIFLibrarySource.myUploads.includes(uploaded))
        XCTAssertFalse(MIFLibrarySource.myUploads.includes(metadataOnly))
        XCTAssertFalse(MIFLibrarySource.myUploads.includes(invalid))
    }

    func testPersonalAttachmentPreviewRequiresSignedInAvailableSupportedAttachment() {
        let pdf = accountItem(attachmentID: "pa_0123456789abcdef0123456789abcdef",
                             mediaType: "application/pdf; charset=binary", previewHandle: previewHandle)
        let image = accountItem(attachmentID: "pa_0123456789abcdef0123456789abcdef",
                               mediaType: "image/png", previewHandle: previewHandle)

        XCTAssertEqual(MIFLibraryPreviewPolicy.personalAttachmentHandle(for: pdf, access: .available), previewHandle)
        XCTAssertEqual(MIFLibraryPreviewPolicy.personalAttachmentHandle(for: image, access: .available), previewHandle)
        XCTAssertNil(MIFLibraryPreviewPolicy.personalAttachmentHandle(for: pdf, access: .loading))
        XCTAssertNil(MIFLibraryPreviewPolicy.personalAttachmentHandle(for: pdf, access: .unavailable))
        XCTAssertNil(MIFLibraryPreviewPolicy.personalAttachmentHandle(for: pdf, access: .denied))
        XCTAssertNil(MIFLibraryPreviewPolicy.personalAttachmentHandle(
            for: accountItem(attachmentID: nil, previewHandle: previewHandle), access: .available))
        XCTAssertNil(MIFLibraryPreviewPolicy.personalAttachmentHandle(
            for: accountItem(attachmentID: "bad", previewHandle: previewHandle), access: .available))
        XCTAssertNil(MIFLibraryPreviewPolicy.personalAttachmentHandle(
            for: accountItem(attachmentID: "pa_0123456789abcdef0123456789abcdef",
                             mediaType: "application/octet-stream", previewHandle: previewHandle), access: .available))
        XCTAssertNil(MIFLibraryPreviewPolicy.personalAttachmentHandle(
            for: accountItem(attachmentID: "pa_0123456789abcdef0123456789abcdef", previewHandle: nil),
            access: .available))
    }

    func testPersonalAttachmentPreviewNormalizesOnlyImageAndPDFMediaTypes() {
        XCTAssertEqual(MIFLibraryPreviewPolicy.normalizedPersonalAttachmentMediaType("IMAGE/JPEG"), "image/jpeg")
        XCTAssertEqual(MIFLibraryPreviewPolicy.normalizedPersonalAttachmentMediaType("application/pdf; charset=binary"),
                       "application/pdf")
        XCTAssertNil(MIFLibraryPreviewPolicy.normalizedPersonalAttachmentMediaType("text/html"))
        XCTAssertNil(MIFLibraryPreviewPolicy.normalizedPersonalAttachmentMediaType(nil))
    }

    func testPersonalAttachmentPreviewCanRepresentLoadingAndReadyStates() {
        let loading = MIFPersonalAttachmentPreviewItem(
            fileURL: nil, name: "report.pdf", mediaType: "application/pdf")
        let ready = MIFPersonalAttachmentPreviewItem(
            fileURL: URL(fileURLWithPath: "/tmp/report.pdf"), name: "report.pdf", mediaType: "application/pdf")

        XCTAssertTrue(loading.isLoading)
        XCTAssertFalse(ready.isLoading)
    }

    @MainActor
    func testTemporaryAttachmentFileIsRetainedAndRemovedOnRetry() throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("morde-cleanup-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let file = directory.appendingPathComponent("preview.pdf")
        try Data("fixture".utf8).write(to: file)
        defer {
            try? FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
            try? FileManager.default.removeItem(at: directory)
        }

        try FileManager.default.setAttributes([.posixPermissions: 0o500], ofItemAtPath: directory.path)
        MIFLibraryTemporaryFileCleanup.discard(file)
        XCTAssertTrue(FileManager.default.fileExists(atPath: file.path))
        XCTAssertTrue(MIFLibraryTemporaryFileCleanup.pendingFiles.contains(file))

        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
        MIFLibraryTemporaryFileCleanup.retryPending()
        XCTAssertFalse(FileManager.default.fileExists(atPath: file.path))
        XCTAssertFalse(MIFLibraryTemporaryFileCleanup.pendingFiles.contains(file))
    }

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

    func testTaskArtifactDecodesOptionalWorkspaceProofFields() throws {
        let data = Data(#"{"id":"a1234567","label":"Output","media_type":"text/plain","verified":true,"created_at":"2026-11-05T00:00:00Z","workspace_id":"wabc1234","workspace_revision":3,"path":"reports/output.txt","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}"#.utf8)
        let artifact = try JSONDecoder().decode(MordeTask.Artifact.self, from: data)

        XCTAssertEqual(artifact.workspace_id, "wabc1234")
        XCTAssertEqual(artifact.workspace_revision, 3)
        XCTAssertEqual(artifact.path, "reports/output.txt")
        XCTAssertEqual(artifact.size_bytes, 12)
        XCTAssertEqual(artifact.sha256, String(repeating: "a", count: 64))
    }

    func testLegacyTaskArtifactWithoutProofDecodesButCannotPreview() throws {
        let data = Data(#"{"id":"a1234567","label":"Legacy output","media_type":"text/plain","verified":true,"created_at":"2026-11-05T00:00:00Z"}"#.utf8)
        let decoded = try JSONDecoder().decode(MordeTask.Artifact.self, from: data)
        let artifact = MIFLibraryTaskArtifact(
            id: decoded.id, label: decoded.label, mediaType: decoded.media_type,
            workspaceID: decoded.workspace_id, workspaceRevision: decoded.workspace_revision,
            sizeBytes: decoded.size_bytes, sha256: decoded.sha256, verified: decoded.verified,
            previewHandle: previewHandle)

        XCTAssertNil(preview(for: artifact))
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

    @MainActor
    func testPreviewAccessibilityLabelsIncludeFileAndArtifactNames() {
        let fileLabel = MIFLibraryPage.localPreviewAccessibilityLabel(for: "report.pdf")
        XCTAssertTrue(fileLabel.contains("report.pdf"))
        XCTAssertNotEqual(fileLabel, AppLocalized("Preview"))

        let uploadLabel = MIFLibraryPage.accountAttachmentPreviewAccessibilityLabel(for: "upload.pdf")
        XCTAssertTrue(uploadLabel.contains("upload.pdf"))
        XCTAssertNotEqual(uploadLabel, AppLocalized("Preview"))

        let artifactLabel = MIFLibraryPage.taskArtifactPreviewAccessibilityLabel(for: "draft.png")
        XCTAssertTrue(artifactLabel.contains("draft.png"))
        XCTAssertNotEqual(artifactLabel, AppLocalized("Preview"))
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

    private func accountItem(attachmentID: String?, mediaType: String? = "application/pdf",
                             previewHandle: MIFLibraryPreviewHandle? = nil) -> MIFLibraryAccountItem {
        MIFLibraryAccountItem(id: "library-item", name: "report.pdf", kind: .artifact,
                              mediaType: mediaType, sizeBytes: 10,
                              attachmentID: attachmentID, previewHandle: previewHandle)
    }
}
