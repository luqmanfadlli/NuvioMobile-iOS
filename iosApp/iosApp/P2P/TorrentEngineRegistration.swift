import Foundation
import ComposeApp

#if NUVIO_FULL && canImport(GoTorrent)

final class IosP2pNativeBridgeImpl: NSObject, IosP2pNativeBridge {
    private let engine = TorrentEngineSwiftBridge.shared

    func startEngine(configJson: String) {
        engine.startEngine(configJson)
    }

    func stopEngine() {
        engine.stopEngine()
    }

    func isEngineRunning() -> Bool {
        engine.isEngineRunning()
    }

    func addTorrentSession(magnetUri: String, infoHash: String, fileIdx: Int32) -> String {
        engine.addTorrentSession(magnetUri: magnetUri, infoHash: infoHash, fileIdx: fileIdx)
    }

    func removeTorrentSession(sessionId: String) {
        engine.removeTorrentSession(sessionId: sessionId)
    }

    func getSessionStatusJson(sessionId: String) -> String {
        engine.getSessionStatusJson(sessionId: sessionId)
    }

    func getEngineStatsJson() -> String {
        engine.getEngineStatsJson()
    }

    func destroyEngine() {
        engine.destroyEngine()
    }
}

final class IosP2pNativeBridgeProviderImpl: NSObject, IosP2pNativeBridgeProvider {
    func createBridge() -> any IosP2pNativeBridge {
        IosP2pNativeBridgeImpl()
    }
}

#endif

enum NuvioNativeP2PRegistration {
    static func registerIfAvailable() {
        #if NUVIO_FULL && canImport(GoTorrent)
        IosP2pNativeBridgeRegistry.shared.registerProvider(provider: IosP2pNativeBridgeProviderImpl())
        print("[P2P] Native iOS GoTorrent bridge registered")
        #endif
    }
}
