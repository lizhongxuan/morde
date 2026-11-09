//
//  ModelUseOffloadBridge.swift
//  MinisApp
//
// nanoMuse: Cloud owns model execution; retain the transient retry helper for its existing tests.

import Foundation

private let logger = AppLogger(category: "ModelUseOffload")

@objc public final class ModelUseOffloadBridge: NSObject {
    /// Retry provider transient failures; callers can veto retries after emitting output.
    static func withTransientRetry<T>(
        label: String,
        maxRetries: Int = 5,
        initialDelaySeconds: Double = 2,
        canRetry: @escaping () -> Bool = { true },
        sleeper: (UInt64) async -> Void = { try? await Task.sleep(nanoseconds: $0) },
        operation: () async throws -> T
    ) async rethrows -> T {
        var attempt = 0
        while true {
            do {
                return try await operation()
            } catch let error as LLMError {
                guard case .transientError(let message) = error,
                      attempt < maxRetries,
                      canRetry() else { throw error }
                attempt += 1
                let delaySeconds = initialDelaySeconds * pow(2.0, Double(attempt - 1))
                logger.warning("[ModelUseRetry] \(label): transient provider error — retry \(attempt)/\(maxRetries) in \(Int(delaySeconds))s: \(message.prefix(200))")
                await sleeper(UInt64(delaySeconds * 1_000_000_000))
            }
        }
    }
}
