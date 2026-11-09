import XCTest
@testable import Minis

final class NanoMuseMordeContentPagesTests: XCTestCase {
    func testFeedContentUsesOnlyAllowedFieldsAndValidValues() throws {
        let post = NanoMuseMordeFeedPostBody(title: "Today", type: "brief", emoji: "📰",
                                            created_at: "2026-11-02T10:30:00Z", liked: false, body: "A short update.")
        XCTAssertTrue(post.isValid)
        var invalid = post
        invalid.type = "private_note"
        XCTAssertFalse(invalid.isValid)

        let preference = NanoMuseMordeFeedPreferences(content: "Keep posts short.")
        XCTAssertTrue(preference.isValid)
        XCTAssertFalse(NanoMuseMordeFeedPreferences(content: "").isValid)
    }

    func testIdeaMarksDoNotContainTheStaticIdeaOrConversation() throws {
        let body = NanoMuseMordeIdeaMarkBody(state: "saved")
        XCTAssertTrue(body.isValid)
        let value = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(body)) as? [String: String])
        XCTAssertEqual(Set(value.keys), ["state"])
        XCTAssertTrue(MordePersonalDataContract.isValidObjectIdentifier(type: "idea_mark", id: "train-watch"))
        XCTAssertFalse(MordePersonalDataContract.isValidObjectIdentifier(type: "idea_mark", id: "train/watch"))
        XCTAssertFalse(NanoMuseMordeIdeaMarkBody(state: "open").isValid)
    }

    func testLibraryMetadataDoesNotStoreLocalPaths() throws {
        let body = NanoMuseMordeLibraryItemBody(name: "notes.pdf", kind: "artifact", mime: "application/pdf",
                                                size_bytes: 10, attachment_id: nil)
        XCTAssertTrue(body.isValid)
        let value = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(body)) as? [String: Any])
        XCTAssertEqual(Set(value.keys), ["name", "kind", "mime", "size_bytes", "attachment_id"])
        XCTAssertEqual(value["attachment_id"] as? String, "")
        XCTAssertFalse(body.hasAttachment)
        XCTAssertNil(value["path"])
        XCTAssertFalse(NanoMuseMordeLibraryItemBody(name: "../notes.pdf", kind: "artifact", mime: nil,
                                                    size_bytes: 10, attachment_id: nil).isValid)
        XCTAssertFalse(NanoMuseMordeLibraryItemBody(name: "notes.pdf", kind: "artifact", mime: nil,
                                                    size_bytes: -1, attachment_id: nil).isValid)
    }

    func testLibraryAttachmentIDRoundTripsEmptyAndPresentValues() throws {
        let metadataOnly = NanoMuseMordeLibraryItemBody(name: "notes.pdf", kind: "artifact", mime: "application/pdf",
                                                        size_bytes: 10, attachment_id: nil)
        let encodedMetadata = try JSONEncoder().encode(metadataOnly)
        let metadataJSON = try XCTUnwrap(JSONSerialization.jsonObject(with: encodedMetadata) as? [String: Any])
        XCTAssertEqual(metadataJSON["attachment_id"] as? String, "")
        let decodedMetadata = try JSONDecoder().decode(NanoMuseMordeLibraryItemBody.self, from: encodedMetadata)
        XCTAssertNil(decodedMetadata.attachment_id)
        XCTAssertFalse(decodedMetadata.hasAttachment)

        let attached = NanoMuseMordeLibraryItemBody(name: "notes.pdf", kind: "artifact", mime: "application/pdf",
                                                    size_bytes: 10, attachment_id: "pa_0123456789abcdef0123456789abcdef")
        let decodedAttached = try JSONDecoder().decode(NanoMuseMordeLibraryItemBody.self,
                                                       from: JSONEncoder().encode(attached))
        XCTAssertEqual(decodedAttached.attachment_id, attached.attachment_id)
        XCTAssertTrue(decodedAttached.hasAttachment)
    }

    func testDiarySyncValidatesCanonicalDatesAndBodyContract() throws {
        XCTAssertTrue(NanoMuseDiarySyncPolicy.isValidDate("2024-02-29"))
        XCTAssertFalse(NanoMuseDiarySyncPolicy.isValidDate("2023-02-29"))
        XCTAssertFalse(NanoMuseDiarySyncPolicy.isValidDate("2026-2-03"))
        XCTAssertFalse(NanoMuseDiarySyncPolicy.isValidDate("0000-01-01"))

        let body = try XCTUnwrap(NanoMuseDiarySyncPolicy.body(date: "2026-11-03", content: "A diary entry."))
        XCTAssertEqual(NanoMuseDiarySyncPolicy.cloudCopy(objectID: "2026-11-03", revision: 2, body: body),
                       NanoMuseDiarySyncPolicy.CloudCopy(date: "2026-11-03", content: "A diary entry.", revision: 2))
        XCTAssertNil(NanoMuseDiarySyncPolicy.body(date: "2026-11-03", content: String(repeating: "x", count: 65_537)))
        XCTAssertNil(NanoMuseDiarySyncPolicy.cloudCopy(objectID: "2026-11-04", revision: 2, body: body))
        XCTAssertNil(NanoMuseDiarySyncPolicy.cloudCopy(objectID: "2026-11-03", revision: 0, body: body))
        XCTAssertNil(NanoMuseDiarySyncPolicy.cloudCopy(
            objectID: "2026-11-03", revision: 2,
            body: .object(["date": .string("2026-11-03"), "content": .string("entry"), "extra": .bool(true)])))
    }

    func testDiaryReconciliationRequiresExplicitChoiceForDifferentCopies() {
        XCTAssertEqual(NanoMuseDiarySyncPolicy.reconciliation(local: nil, cloud: nil), .noCopies)
        XCTAssertEqual(NanoMuseDiarySyncPolicy.reconciliation(local: "local", cloud: nil), .localOnly)
        XCTAssertEqual(NanoMuseDiarySyncPolicy.reconciliation(local: nil, cloud: "cloud"), .cloudOnly)
        XCTAssertEqual(NanoMuseDiarySyncPolicy.reconciliation(local: "same", cloud: "same"), .matching)
        XCTAssertEqual(NanoMuseDiarySyncPolicy.reconciliation(local: "local", cloud: "cloud"), .different)
    }

    func testScheduleRunHistoryRejectsCrossScheduleAndDuplicateRows() {
        let run = MordeScheduleRunDTO(id: "r1234567", schedule_id: "j1234567", schedule_version: 1,
                                      occurrence_key: "daily:2026-11-03", coalesced_count: 0,
                                      status: "completed", notification_events: [], observation_outcome: "none",
                                      created_at: "2026-11-03T00:00:00Z", updated_at: "2026-11-03T00:00:00Z")
        XCTAssertTrue(NanoMuseMordeScheduleRunHistoryPolicy.canAppend(
            existingIDs: [], page: [run], scheduleID: "j1234567"))
        XCTAssertFalse(NanoMuseMordeScheduleRunHistoryPolicy.canAppend(
            existingIDs: [run.id], page: [run], scheduleID: "j1234567"))
        XCTAssertFalse(NanoMuseMordeScheduleRunHistoryPolicy.canAppend(
            existingIDs: [], page: [run], scheduleID: "j7654321"))
    }
}
