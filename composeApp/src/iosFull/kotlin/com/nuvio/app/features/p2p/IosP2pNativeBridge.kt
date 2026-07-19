package com.nuvio.app.features.p2p

/**
 * iOS-only bridge boundary for the native P2P engine.
 *
 * Swift implements this protocol and registers a provider during app startup.
 * Keeping this file in iosFull ensures App Store builds do not export or link
 * the native torrent bridge.
 */
interface IosP2pNativeBridge {
    fun startEngine(configJson: String)
    fun stopEngine()
    fun isEngineRunning(): Boolean
    fun addTorrentSession(magnetUri: String, infoHash: String, fileIdx: Int): String
    fun removeTorrentSession(sessionId: String)
    fun getSessionStatusJson(sessionId: String): String
    fun getEngineStatsJson(): String
    fun destroyEngine()
}

interface IosP2pNativeBridgeProvider {
    fun createBridge(): IosP2pNativeBridge
}

object IosP2pNativeBridgeRegistry {
    private var provider: IosP2pNativeBridgeProvider? = null

    fun registerProvider(provider: IosP2pNativeBridgeProvider) {
        this.provider = provider
    }

    internal fun createBridge(): IosP2pNativeBridge? = provider?.createBridge()

    val isRegistered: Boolean
        get() = provider != null
}
