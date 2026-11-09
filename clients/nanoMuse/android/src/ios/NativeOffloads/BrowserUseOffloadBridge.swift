//
//  BrowserUseOffloadBridge.swift
//  MinisApp
//

import Foundation

@objc public final class BrowserUseOffloadBridge: NSObject {
    // nanoMuse: Keep the lifecycle selector used by app cleanup after removing the iSH CLI pool.
    @objc public static func releasePool(forSession _: String) {}
}
