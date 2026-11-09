import XCTest
@testable import Minis

final class MordeTaskSpeechInputPolicyTests: XCTestCase {
    func testTaskVoiceIndicatorTracksIdleStartingAndRecordingStates() {
        XCTAssertEqual(MordeTaskSpeechIndicatorState.resolve(isStarting: false, isRecording: false), .idle)
        XCTAssertEqual(MordeTaskSpeechIndicatorState.resolve(isStarting: true, isRecording: false), .starting)
        XCTAssertEqual(MordeTaskSpeechIndicatorState.resolve(isStarting: false, isRecording: true), .recording)
    }

    func testRecordingStateWinsIfPermissionCompletionRacesWithRecorderStart() {
        XCTAssertEqual(MordeTaskSpeechIndicatorState.resolve(isStarting: true, isRecording: true), .recording)
    }

    func testTaskDictationFailsClosedWhenOnDeviceRecognitionIsUnsupported() {
        XCTAssertNil(MordeTaskSpeechInputPolicy.onDeviceRequirement(forced: true, supported: false))
        XCTAssertEqual(MordeTaskSpeechInputPolicy.onDeviceRequirement(forced: true, supported: true), true)
    }

    func testExistingVoiceInputStillPrefersOnDeviceWhenAvailable() {
        XCTAssertEqual(MordeTaskSpeechInputPolicy.onDeviceRequirement(forced: false, supported: true), true)
        XCTAssertEqual(MordeTaskSpeechInputPolicy.onDeviceRequirement(forced: false, supported: false), false)
    }
}
