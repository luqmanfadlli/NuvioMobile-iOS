package com.nuvio.app.features.p2p

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import platform.Foundation.NSLock

private const val IOS_P2P_METADATA_TIMEOUT_MS = 45_000L
private const val IOS_P2P_STATUS_POLL_MS = 1_000L
private const val IOS_P2P_DEFAULT_CACHE_BYTES = 512L * 1024L * 1024L
private const val IOS_P2P_DEFAULT_MAX_PEERS = 160

actual object P2pStreamingEngine {
    private val _state = MutableStateFlow<P2pStreamingState>(P2pStreamingState.Idle)
    actual val state: StateFlow<P2pStreamingState> = _state.asStateFlow()

    private val json = Json { ignoreUnknownKeys = true }
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val lifecycleLock = NSLock()

    private var bridge: IosP2pNativeBridge? = null
    private var statsJob: Job? = null
    private var activeSessionId: String? = null
    private var streamGeneration = 0L

    actual suspend fun startStream(request: P2pStreamRequest): String = withContext(Dispatchers.Default) {
        stopStreamNow(stopEngine = false)
        val generation = nextStreamGeneration()
        _state.value = P2pStreamingState.Connecting

        try {
            val nativeBridge = ensureBridge()
            if (!nativeBridge.isEngineRunning()) {
                nativeBridge.startEngine(buildEngineConfigJson())
            }
            ensureCurrentGeneration(generation)

            val magnetUri = buildMagnetUri(request.infoHash, request.trackers)
            val requestedFileIdx = request.fileIdx ?: -1
            val addStatus = parseStatus(
                nativeBridge.addTorrentSession(
                    magnetUri = magnetUri,
                    infoHash = request.infoHash,
                    fileIdx = requestedFileIdx,
                ),
            ) ?: throw P2pStreamingException("Native iOS torrent engine returned an empty response")

            if (!addStatus.errorMessage.isNullOrBlank()) {
                throw P2pStreamingException(addStatus.errorMessage)
            }

            val sessionId = addStatus.sessionId.ifBlank { addStatus.infoHash.ifBlank { request.infoHash } }
            if (!attachSessionIfCurrent(generation, sessionId)) {
                nativeBridge.removeTorrentSession(sessionId)
                throw CancellationException("P2P stream start was cancelled")
            }

            val readyStatus = waitForPlayableStatus(nativeBridge, sessionId, generation)
            val streamUrl = readyStatus.streamUrl.ifBlank {
                throw P2pStreamingException("Native iOS torrent engine did not return a stream URL")
            }

            _state.value = P2pStreamingState.Streaming(
                localUrl = streamUrl,
                downloadSpeed = readyStatus.downloadRate,
                uploadSpeed = readyStatus.uploadRate,
                peers = readyStatus.numPeers,
                seeds = readyStatus.numSeeds,
                bufferProgress = 0f,
                totalProgress = readyStatus.progress.toFloat(),
                preloadedBytes = readyStatus.preloadedBytes,
            )

            startStatsPolling(nativeBridge, sessionId, generation)
            streamUrl
        } catch (error: CancellationException) {
            throw error
        } catch (error: Exception) {
            if (isCurrentGeneration(generation)) {
                _state.value = P2pStreamingState.Error(error.message ?: "Unable to start iOS P2P stream")
            }
            throw error
        }
    }

    actual fun stopStream() {
        scope.launch {
            stopStreamNow(stopEngine = false)
        }
    }

    actual fun shutdown() {
        scope.launch {
            stopStreamNow(stopEngine = true)
        }
    }

    private suspend fun waitForPlayableStatus(
        nativeBridge: IosP2pNativeBridge,
        sessionId: String,
        generation: Long,
    ): NativeTorrentSessionStatus {
        var latest: NativeTorrentSessionStatus? = null
        val completed = withTimeoutOrNull(IOS_P2P_METADATA_TIMEOUT_MS) {
            var playableStatus: NativeTorrentSessionStatus? = null
            while (playableStatus == null) {
                ensureCurrentGeneration(generation)
                val status = parseStatus(nativeBridge.getSessionStatusJson(sessionId))
                if (status != null) {
                    latest = status
                    if (!status.errorMessage.isNullOrBlank()) {
                        throw P2pStreamingException(status.errorMessage)
                    }
                    if (status.streamUrl.isNotBlank() && status.isMetadataResolved) {
                        playableStatus = status
                    }
                }
                if (playableStatus == null) {
                    _state.value = P2pStreamingState.Connecting
                    delay(IOS_P2P_STATUS_POLL_MS)
                }
            }
            requireNotNull(playableStatus)
        }

        if (completed != null) return completed
        val message = latest?.errorMessage?.takeIf { it.isNotBlank() }
            ?: "Timed out waiting for iOS torrent metadata"
        throw P2pStreamingException(message)
    }

    private suspend fun stopStreamNow(stopEngine: Boolean) {
        val detached: Pair<String?, Job?> = withLifecycleLock {
            streamGeneration += 1
            val sessionId = activeSessionId
            val job = statsJob
            activeSessionId = null
            statsJob = null
            sessionId to job
        }

        detached.second?.cancel()
        detached.first?.let { sessionId ->
            bridge?.removeTorrentSession(sessionId)
        }
        if (stopEngine) {
            bridge?.stopEngine()
        }
        _state.value = P2pStreamingState.Idle
    }

    private fun ensureBridge(): IosP2pNativeBridge {
        bridge?.let { return it }
        val created = IosP2pNativeBridgeRegistry.createBridge()
            ?: throw P2pStreamingException(
                "Native iOS P2P bridge is not registered. Build GoTorrent.xcframework and call NuvioNativeP2PRegistration.registerIfAvailable() from iOSApp.swift.",
            )
        bridge = created
        return created
    }

    private fun nextStreamGeneration(): Long = withLifecycleLock {
        streamGeneration += 1
        streamGeneration
    }

    private fun attachSessionIfCurrent(generation: Long, sessionId: String): Boolean = withLifecycleLock {
        if (streamGeneration != generation) {
            false
        } else {
            activeSessionId = sessionId
            true
        }
    }

    private fun isCurrentGeneration(generation: Long): Boolean =
        withLifecycleLock { streamGeneration == generation }

    private fun ensureCurrentGeneration(generation: Long) {
        if (!isCurrentGeneration(generation)) {
            throw CancellationException("P2P stream start was cancelled")
        }
    }

    private inline fun <T> withLifecycleLock(block: () -> T): T {
        lifecycleLock.lock()
        return try {
            block()
        } finally {
            lifecycleLock.unlock()
        }
    }

    private fun startStatsPolling(
        nativeBridge: IosP2pNativeBridge,
        sessionId: String,
        generation: Long,
    ) {
        statsJob?.cancel()
        statsJob = scope.launch {
            while (isActive) {
                if (!isCurrentGeneration(generation)) return@launch
                try {
                    val status = parseStatus(nativeBridge.getSessionStatusJson(sessionId))
                    val current = _state.value
                    if (status != null && current is P2pStreamingState.Streaming && isCurrentGeneration(generation)) {
                        if (!status.errorMessage.isNullOrBlank()) {
                            _state.value = P2pStreamingState.Error(status.errorMessage)
                            return@launch
                        }
                        _state.value = current.copy(
                            downloadSpeed = status.downloadRate,
                            uploadSpeed = status.uploadRate,
                            peers = status.numPeers,
                            seeds = status.numSeeds,
                            totalProgress = status.progress.toFloat(),
                            preloadedBytes = status.preloadedBytes,
                        )
                    }
                } catch (cancellation: CancellationException) {
                    throw cancellation
                } catch (_: Exception) {
                    // Keep playback alive if a transient stats poll fails.
                }
                delay(IOS_P2P_STATUS_POLL_MS)
            }
        }
    }

    private fun buildEngineConfigJson(): String {
        P2pSettingsRepository.ensureLoaded()
        val settings = P2pSettingsRepository.uiState.value
        return buildString {
            append('{')
            append("\"maxCacheSizeBytes\":$IOS_P2P_DEFAULT_CACHE_BYTES,")
            append("\"maxDownloadRate\":0,")
            append("\"maxUploadRate\":0,")
            append("\"enableUpload\":${settings.enableUpload},")
            append("\"maxPeerConnections\":$IOS_P2P_DEFAULT_MAX_PEERS,")
            append("\"enableUpnp\":false,")
            append("\"enableDHT\":true,")
            append("\"forceTcp\":false,")
            append("\"batterySaver\":false")
            append('}')
        }
    }

    private fun buildMagnetUri(infoHash: String, extraTrackers: List<String>): String {
        val trackers = (DEFAULT_TRACKERS + extraTrackers)
            .map { it.trim() }
            .filter { it.isNotEmpty() }
            .distinct()
        val trackerParams = trackers.joinToString(separator = "") { tracker -> "&tr=$tracker" }
        return "magnet:?xt=urn:btih:$infoHash$trackerParams"
    }

    private fun parseStatus(jsonString: String): NativeTorrentSessionStatus? {
        if (jsonString.isBlank() || jsonString == "{}") return null
        return try {
            json.decodeFromString<NativeTorrentSessionStatus>(jsonString)
        } catch (_: Exception) {
            NativeTorrentSessionStatus(errorMessage = "Failed to parse native torrent status")
        }
    }

    private val DEFAULT_TRACKERS = listOf(
        "udp://tracker.opentrackr.org:1337/announce",
        "udp://open.stealth.si:80/announce",
        "udp://tracker.openbittorrent.com:6969/announce",
        "udp://exodus.desync.com:6969/announce",
        "udp://tracker.torrent.eu.org:451/announce",
    )
}

@Serializable
private data class NativeTorrentSessionStatus(
    val sessionId: String = "",
    val infoHash: String = "",
    val magnetUri: String = "",
    val fileIndex: Int = -1,
    val status: String = "initializing",
    val streamUrl: String = "",
    val fileName: String = "",
    val totalSizeBytes: Long = 0L,
    val downloadedBytes: Long = 0L,
    val downloadRate: Long = 0L,
    val uploadRate: Long = 0L,
    val preloadedBytes: Long = 0L,
    val numPeers: Int = 0,
    val numSeeds: Int = 0,
    val progress: Double = 0.0,
    val isMetadataResolved: Boolean = false,
    val isStreaming: Boolean = false,
    val errorMessage: String? = null,
)
