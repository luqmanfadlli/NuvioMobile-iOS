import Foundation

#if NUVIO_FULL && canImport(GoTorrent)
import GoTorrent

@objc public final class TorrentEngineSwiftBridge: NSObject {
    @objc public static let shared = TorrentEngineSwiftBridge()

    private struct SessionParameters {
        let magnetUri: String
        let fileIdx: Int32
    }

    private var isStarted = false
    private var lastStartError = ""
    private var sessions: [String: SessionParameters] = [:]
    private let engineQueue = DispatchQueue(label: "com.nuvio.p2p.gotorrent", qos: .userInitiated)

    private override init() {
        super.init()
    }

    @objc public func startEngine(_ configJson: String) {
        engineQueue.sync {
            guard !isStarted else { return }

            let cacheRoot = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0]
            let downloadPath = cacheRoot.appendingPathComponent("TorrentCache", isDirectory: true).path

            do {
                try FileManager.default.createDirectory(atPath: downloadPath, withIntermediateDirectories: true)
            } catch {
                lastStartError = "Failed to create torrent cache: \(error.localizedDescription)"
                return
            }

            let startResult = GotorrentStartEngine(downloadPath, configJson) ?? ""
            if startResult.isEmpty {
                lastStartError = ""
                isStarted = true
            } else {
                lastStartError = startResult
            }
        }
    }

    @objc public func stopEngine() {
        engineQueue.sync {
            guard isStarted else { return }
            GotorrentStopEngine()
            sessions.removeAll()
            isStarted = false
            lastStartError = ""
        }
    }

    @objc public func isEngineRunning() -> Bool {
        engineQueue.sync { isStarted }
    }

    @objc public func addTorrentSession(magnetUri: String, infoHash: String, fileIdx: Int32) -> String {
        engineQueue.sync {
            guard isStarted else {
                return jsonError(lastStartError.isEmpty ? "Native torrent engine is not started" : lastStartError)
            }

            let resultJson = GotorrentAddMagnet(magnetUri, Int(fileIdx)) ?? "{}"
            if let sessionId = Self.extractString("sessionId", from: resultJson), sessionId.isEmpty == false {
                sessions[sessionId] = SessionParameters(magnetUri: magnetUri, fileIdx: fileIdx)
            } else if let hash = Self.extractString("infoHash", from: resultJson), hash.isEmpty == false {
                sessions[hash] = SessionParameters(magnetUri: magnetUri, fileIdx: fileIdx)
            }
            return resultJson
        }
    }

    @objc public func removeTorrentSession(sessionId: String) {
        engineQueue.async {
            guard self.isStarted else { return }
            self.sessions.removeValue(forKey: sessionId)
            GotorrentRemoveTorrent(sessionId)
        }
    }

    @objc public func getSessionStatusJson(sessionId: String) -> String {
        engineQueue.sync {
            guard isStarted else { return "{}" }
            let params = sessions[sessionId]
            return GotorrentGetSessionStatus(
                sessionId,
                params?.magnetUri ?? "",
                Int(params?.fileIdx ?? -1)
            ) ?? "{}"
        }
    }

    @objc public func getEngineStatsJson() -> String {
        engineQueue.sync {
            guard isStarted else { return "{}" }
            return GotorrentGetEngineStatsJson() ?? "{}"
        }
    }

    @objc public func destroyEngine() {
        stopEngine()
    }

    private func jsonError(_ message: String) -> String {
        let escaped = message
            .replacingOccurrences(of: "\\", with: "\\\\")
            .replacingOccurrences(of: "\"", with: "\\\"")
            .replacingOccurrences(of: "\n", with: " ")
        return "{\"errorMessage\":\"\(escaped)\"}"
    }

    private static func extractString(_ key: String, from json: String) -> String? {
        guard let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            return nil
        }
        return object[key] as? String
    }
}
#endif
