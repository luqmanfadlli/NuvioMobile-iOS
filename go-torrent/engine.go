package gotorrent

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

var (
	client               *torrent.Client
	server               *http.Server
	port                 int
	mu                   sync.RWMutex
	globalCustomTrackers []string
	lastInitError        string
)

type SessionStatus struct {
	SessionId          string  `json:"sessionId"`
	InfoHash           string  `json:"infoHash"`
	MagnetUri          string  `json:"magnetUri"`
	FileIndex          int     `json:"fileIndex"`
	Status             string  `json:"status"`
	StreamUrl          string  `json:"streamUrl"`
	FileName           string  `json:"fileName"`
	TotalSizeBytes     int64   `json:"totalSizeBytes"`
	DownloadedBytes    int64   `json:"downloadedBytes"`
	DownloadRate       int64   `json:"downloadRate"`
	UploadRate         int64   `json:"uploadRate"`
	PreloadedBytes     int64   `json:"preloadedBytes"`
	NumPeers           int     `json:"numPeers"`
	NumSeeds           int     `json:"numSeeds"`
	Progress           float64 `json:"progress"`
	IsMetadataResolved bool    `json:"isMetadataResolved"`
	IsStreaming        bool    `json:"isStreaming"`
	ErrorMessage       string  `json:"errorMessage,omitempty"`
}

type TorrentSpeedState struct {
	LastBytesRead int64
	LastTime      time.Time
	DownloadRate  int64
}

var speedTracker = make(map[string]*TorrentSpeedState)
var speedTrackerMu sync.Mutex

type EngineConfig struct {
	HttpPort           int    `json:"httpPort"`
	MaxCacheSizeBytes  int64  `json:"maxCacheSizeBytes"`
	MaxDownloadRate    int64  `json:"maxDownloadRate"`
	MaxUploadRate      int64  `json:"maxUploadRate"`
	EnableUpload       bool   `json:"enableUpload"`
	MaxPeerConnections int    `json:"maxPeerConnections"`
	EnableUpnp         bool   `json:"enableUpnp"`
	EnableDHT          bool   `json:"enableDHT"`
	ForceTcp           bool   `json:"forceTcp"`
	CustomTrackers     string `json:"customTrackers"`
	BatterySaver       bool   `json:"batterySaver"`
}

func StartEngine(dataDir string, configJson string) (res string) {
	defer func() {
		if r := recover(); r != nil {
			res = fmt.Sprintf("CRASH: panic in StartEngine: %v", r)
		}
	}()
	mu.Lock()
	defer mu.Unlock()

	if client != nil {
		return ""
	}

	var parsedCfg EngineConfig
	if configJson != "" {
		_ = json.Unmarshal([]byte(configJson), &parsedCfg)
	}

	cacheStats := configureCacheLocked(dataDir, parsedCfg.MaxCacheSizeBytes)
	if cacheStats.ErrorMessage != "" {
		lastInitError = "Cache initialization failed: " + cacheStats.ErrorMessage
		return lastInitError
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	// Partition payloads by info-hash so inactive torrents can be reclaimed
	// without touching the stream currently served to MPV.
	cfg.DefaultStorage = storage.NewFileByInfoHash(dataDir)

	// Apply dynamic settings
	cfg.NoDefaultPortForwarding = !parsedCfg.EnableUpnp
	cfg.DisableUTP = parsedCfg.ForceTcp

	globalCustomTrackers = nil
	if parsedCfg.CustomTrackers != "" {
		parts := strings.FieldsFunc(parsedCfg.CustomTrackers, func(c rune) bool {
			return c == ',' || c == '\n' || c == '\r'
		})
		for _, p := range parts {
			if p != "" {
				globalCustomTrackers = append(globalCustomTrackers, strings.TrimSpace(p))
			}
		}
	}

	// DHT Bootstrapping is already natively handled by anacrolix/dht DefaultStartingNodes

	if parsedCfg.MaxPeerConnections > 0 {
		cfg.EstablishedConnsPerTorrent = parsedCfg.MaxPeerConnections
		cfg.HalfOpenConnsPerTorrent = 100
	} else {
		cfg.EstablishedConnsPerTorrent = 250 // Restored to previously working aggressive limit
		cfg.HalfOpenConnsPerTorrent = 100
	}

	if parsedCfg.BatterySaver {
		cfg.NoDHT = true
		cfg.DisablePEX = true
		if parsedCfg.MaxPeerConnections <= 0 || parsedCfg.MaxPeerConnections > 20 {
			cfg.EstablishedConnsPerTorrent = 20
			cfg.HalfOpenConnsPerTorrent = 10
		}
	} else {
		cfg.NoDHT = !parsedCfg.EnableDHT
	}

	cfg.TorrentPeersHighWater = 250
	cfg.TorrentPeersLowWater = 150

	// Ruthless Timeouts removed because anacrolix/torrent handles peer read timeouts internally

	// Rate limits. A zero max rate means unlimited only when the direction is enabled.
	if parsedCfg.MaxDownloadRate > 0 {
		cfg.DownloadRateLimiter = rate.NewLimiter(rate.Limit(parsedCfg.MaxDownloadRate), int(parsedCfg.MaxDownloadRate))
	}

	if !parsedCfg.EnableUpload {
		// anacrolix/torrent does not expose a portable "no upload" switch here,
		// so use a near-zero limiter instead of accidentally making upload unlimited.
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(1), 1)
	} else if parsedCfg.MaxUploadRate > 0 {
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(parsedCfg.MaxUploadRate), int(parsedCfg.MaxUploadRate))
	}

	c, err := torrent.NewClient(cfg)
	if err != nil {
		lastInitError = "NewClient failed: " + err.Error()
		return lastInitError
	}
	client = c

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		client.Close()
		client = nil
		lastInitError = "Listen failed: " + err.Error()
		return lastInitError
	}
	port = listener.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/stream/", handleStream)

	server = &http.Server{Handler: mux}
	go server.Serve(listener)
	lastInitError = ""

	return ""
}

func StopEngine() {
	mu.Lock()
	defer mu.Unlock()
	if server != nil {
		server.Close()
		server = nil
	}
	if client != nil {
		client.Close()
		client = nil
	}
	_ = reclaimCacheLocked(cacheRootDir, cacheLimitBytes)
}

func AddMagnet(uri string, fileIdx int) (res string) {
	defer func() {
		if r := recover(); r != nil {
			res = fmt.Sprintf(`{"errorMessage": "CRASH: panic in AddMagnet: %v"}`, r)
		}
	}()
	mu.Lock()
	defer mu.Unlock()

	if client == nil {
		if lastInitError != "" {
			errMsg := strings.ReplaceAll(lastInitError, "\"", "'")
			return fmt.Sprintf(`{"errorMessage": "Engine init failed: %s"}`, errMsg)
		}
		return `{"errorMessage": "Engine not started"}`
	}

	// Inject a small default tracker set. Kotlin also passes trackers from the stream.
	godTrackers := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://open.stealth.si:80/announce",
		"udp://tracker.openbittorrent.com:6969/announce",
		"udp://exodus.desync.com:6969/announce",
		"udp://tracker.torrent.eu.org:451/announce",
	}

	if mag, err := metainfo.ParseMagnetUri(uri); err == nil {
		mag.Trackers = append(godTrackers, mag.Trackers...)

		if len(globalCustomTrackers) > 0 {
			for _, tr := range globalCustomTrackers {
				mag.Trackers = append(mag.Trackers, tr)
			}
		}

		// Remove duplicates
		seen := make(map[string]bool)
		var uniqueTrackers []string
		for _, tr := range mag.Trackers {
			if !seen[tr] && tr != "" {
				seen[tr] = true
				uniqueTrackers = append(uniqueTrackers, tr)
			}
		}
		mag.Trackers = uniqueTrackers
		uri = mag.String()
	}

	t, err := client.AddMagnet(uri)
	if err != nil {
		return fmt.Sprintf(`{"errorMessage": "%s"}`, err.Error())
	}

	hash := t.InfoHash().HexString()

	// Instant Warmup & Preloading
	go func(torrentObj *torrent.Torrent, uriStr string, fIdx int) {
		<-torrentObj.GotInfo()
		if torrentObj.Info() != nil {
			files := torrentObj.Files()
			if fIdx >= 0 && fIdx < len(files) {
				targetFile := files[fIdx]
				// Prioritize first 2 pieces for warmup so HTTP probe doesn't hang
				pieceLen := torrentObj.Info().PieceLength
				if pieceLen > 0 {
					firstPiece := int(targetFile.Offset() / int64(pieceLen))
					torrentObj.Piece(firstPiece).SetPriority(torrent.PiecePriorityNow)
					if firstPiece+1 < torrentObj.Info().NumPieces() {
						torrentObj.Piece(firstPiece + 1).SetPriority(torrent.PiecePriorityNow)
					}
				}
			}
		}
	}(t, uri, fileIdx)

	return getSessionStatusJson(hash, uri, fileIdx, t)
}

func GetSessionStatus(hash string, uri string, fileIdx int) (res string) {
	defer func() {
		if r := recover(); r != nil {
			res = fmt.Sprintf(`{"errorMessage": "CRASH: panic in GetSessionStatus: %v"}`, r)
		}
	}()
	mu.RLock()
	defer mu.RUnlock()

	if client == nil {
		if lastInitError != "" {
			errMsg := strings.ReplaceAll(lastInitError, "\"", "'")
			return fmt.Sprintf(`{"errorMessage": "Engine init failed: %s"}`, errMsg)
		}
		return `{"errorMessage": "Engine not started"}`
	}

	var t *torrent.Torrent
	for _, tt := range client.Torrents() {
		if tt.InfoHash().HexString() == hash {
			t = tt
			break
		}
	}

	if t == nil {
		return `{"errorMessage": "Torrent not found"}`
	}

	return getSessionStatusJson(hash, uri, fileIdx, t)
}

func RemoveTorrent(hash string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("CRASH: panic in RemoveTorrent: %v\n", r)
		}
	}()
	mu.Lock()
	defer mu.Unlock()
	if client != nil {
		for _, t := range client.Torrents() {
			if t.InfoHash().HexString() == hash {
				t.Drop()
				break
			}
		}
	}
	speedTrackerMu.Lock()
	delete(speedTracker, hash)
	speedTrackerMu.Unlock()
	_ = reclaimCacheLocked(cacheRootDir, cacheLimitBytes)
}

func GetEngineStatsJson() string {
	return `{"activeSessions": 1, "totalDownloadRate": 0, "totalUploadRate": 0}`
}

func getSessionStatusJson(hash, uri string, fileIdx int, t *torrent.Torrent) (res string) {
	defer func() {
		if r := recover(); r != nil {
			res = fmt.Sprintf(`{"errorMessage": "CRASH: panic in getSessionStatusJson: %v"}`, r)
		}
	}()
	info := t.Info()

	status := "resolvingmetadata"
	if info != nil {
		status = "downloading"
	}

	s := SessionStatus{
		SessionId:          hash,
		InfoHash:           hash,
		MagnetUri:          uri,
		FileIndex:          fileIdx,
		Status:             status,
		NumPeers:           len(t.PeerConns()),
		IsMetadataResolved: info != nil,
	}

	if info != nil {
		var targetFile *torrent.File
		files := t.Files()
		if fileIdx >= 0 && fileIdx < len(files) {
			targetFile = files[fileIdx]
		} else {
			var largestSize int64
			for _, f := range files {
				if f.Length() > largestSize {
					largestSize = f.Length()
					targetFile = f
				}
			}
		}

		if targetFile != nil {
			s.FileName = targetFile.DisplayPath()
			s.TotalSizeBytes = targetFile.Length()
			s.DownloadedBytes = targetFile.BytesCompleted()
			s.PreloadedBytes = targetFile.BytesCompleted()
			if targetFile.Length() > 0 {
				s.Progress = float64(targetFile.BytesCompleted()) / float64(targetFile.Length())
			}
			if s.Progress >= 1.0 {
				s.Status = "completed"
			} else if s.Progress > 0 {
				s.Status = "streaming"
				s.IsStreaming = true
			}

		}

		s.StreamUrl = fmt.Sprintf("http://127.0.0.1:%d/stream/%s?fileIdx=%d", port, hash, fileIdx)
	}

	// Calculate True Download Rate
	speedTrackerMu.Lock()
	tracker, ok := speedTracker[hash]
	if !ok {
		tracker = &TorrentSpeedState{}
		speedTracker[hash] = tracker
	}
	now := time.Now()
	stats := t.Stats()
	bytesRead := stats.BytesReadData.Int64()
	if !tracker.LastTime.IsZero() {
		dur := now.Sub(tracker.LastTime).Seconds()
		if dur > 0 {
			tracker.DownloadRate = int64(float64(bytesRead-tracker.LastBytesRead) / dur)
		}
	}
	tracker.LastBytesRead = bytesRead
	tracker.LastTime = now
	s.DownloadRate = tracker.DownloadRate
	speedTrackerMu.Unlock()

	b, _ := json.Marshal(s)
	return string(b)
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	c := client
	mu.RUnlock()

	if c == nil {
		http.Error(w, "Engine not started", 500)
		return
	}

	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.Error(w, "Invalid path", 400)
		return
	}
	hash := parts[2]

	var t *torrent.Torrent
	for _, tt := range c.Torrents() {
		if tt.InfoHash().HexString() == hash {
			t = tt
			break
		}
	}

	if t == nil {
		http.Error(w, "Torrent not found", 404)
		return
	}

	select {
	case <-t.GotInfo():
	case <-r.Context().Done():
		return
	}

	fileIdx := -1
	fmt.Sscanf(r.URL.Query().Get("fileIdx"), "%d", &fileIdx)

	info := t.Info()
	if info == nil {
		http.Error(w, "Metadata not ready", 500)
		return
	}

	filenameHint := r.URL.Query().Get("filename")

	files := t.Files()
	var targetFile *torrent.File
	if fileIdx >= 0 && fileIdx < len(files) {
		targetFile = files[fileIdx]
	} else if filenameHint != "" {
		for _, f := range files {
			if strings.Contains(strings.ToLower(f.DisplayPath()), strings.ToLower(filenameHint)) {
				targetFile = f
				break
			}
		}
	}

	if targetFile == nil {
		var largestSize int64
		for _, f := range files {
			if f.Length() > largestSize {
				largestSize = f.Length()
				targetFile = f
			}
		}
	}

	if targetFile == nil {
		http.Error(w, "File not found", 404)
		return
	}

	// Prioritize first 2 pieces for warmup so HTTP probe doesn't hang
	if info != nil {
		pieceLen := info.PieceLength
		if pieceLen > 0 {
			firstPiece := int(targetFile.Offset() / int64(pieceLen))
			t.Piece(firstPiece).SetPriority(torrent.PiecePriorityNow)
			if firstPiece+1 < info.NumPieces() {
				t.Piece(firstPiece + 1).SetPriority(torrent.PiecePriorityNow)
			}
		}
	}

	reader := targetFile.NewReader()
	defer reader.Close()

	reader.SetResponsive()

	// Wait for at least some data to be downloaded before returning HTTP 200 OK.
	// If we return 200 OK with 0 bytes available, ffmpeg/mpv might throw
	// "stream ends prematurely at 0" and fail to probe the container format.
	if r.Method == "GET" {
		for {
			if targetFile.Length() > 0 && targetFile.BytesCompleted() > 0 {
				break
			}
			select {
			case <-time.After(10 * time.Millisecond):
				continue
			case <-r.Context().Done():
				return
			}
		}
	}

	// Set Content-Type based on file extension so MPV/ffmpeg recognises the
	// container format immediately instead of guessing.
	fileName := filepath.Base(targetFile.DisplayPath())
	contentType := inferContentType(fileName)
	w.Header().Set("Content-Type", contentType)

	http.ServeContent(w, r, fileName, time.Time{}, &PrioritizingReader{
		reader:     reader,
		targetFile: targetFile,
		torrentObj: t,
	})
}

func inferContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	case ".webm":
		return "video/webm"
	case ".ts":
		return "video/mp2t"
	case ".mov":
		return "video/quicktime"
	case ".flv":
		return "video/x-flv"
	case ".wmv":
		return "video/x-ms-wmv"
	default:
		return "application/octet-stream"
	}
}

type PrioritizingReader struct {
	reader     torrent.Reader
	targetFile *torrent.File
	torrentObj *torrent.Torrent
}

func (pr *PrioritizingReader) Seek(offset int64, whence int) (int64, error) {
	newOffset, err := pr.reader.Seek(offset, whence)
	if err == nil {
		info := pr.torrentObj.Info()
		if info != nil {
			pieceLen := info.PieceLength
			if pieceLen > 0 {
				globalOffset := pr.targetFile.Offset() + newOffset
				pieceIdx := int(globalOffset / int64(pieceLen))

				for i := pieceIdx; i < pieceIdx+20 && i < info.NumPieces(); i++ {
					pr.torrentObj.Piece(i).SetPriority(torrent.PiecePriorityNow)
				}
			}
		}
	}
	return newOffset, err
}

func (pr *PrioritizingReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		info := pr.torrentObj.Info()
		if info != nil {
			pieceLen := info.PieceLength
			if pieceLen > 0 {
				currentOffset, _ := pr.reader.Seek(0, 1)
				globalOffset := pr.targetFile.Offset() + currentOffset
				pieceIdx := int(globalOffset / int64(pieceLen))

				for i := pieceIdx; i < pieceIdx+10 && i < info.NumPieces(); i++ {
					pr.torrentObj.Piece(i).SetPriority(torrent.PiecePriorityNow)
				}
			}
		}
	}
	return n, err
}
