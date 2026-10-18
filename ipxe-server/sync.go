package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	syncJobMu      sync.Mutex
	syncJobRunning bool
	lastScheduled  string
)

// RemoteManifestItem describes a file (and any partial .sync.part file) on a remote warehouse node.
type RemoteManifestItem struct {
	Category  string `json:"category"`
	Filename  string `json:"filename"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	PartBytes int64  `json:"part_bytes"`
}

// syncStreamReader wraps an io.Reader to enforce WAN bandwidth throttling (SyncMaxMBps)
// and report real-time byte-by-byte progress to the remote_nodes table.
type syncStreamReader struct {
	reader       io.Reader
	nodeID       int64
	filename     string
	baseOffset   int64
	totalBytes   int64
	bytesRead    int64
	accumUpdate  int64
	maxBytesPerS float64
	startTime    time.Time
}

func (sr *syncStreamReader) Read(p []byte) (int, error) {
	n, err := sr.reader.Read(p)
	if n > 0 {
		sr.bytesRead += int64(n)
		sr.accumUpdate += int64(n)

		// Enforce WAN bandwidth limit if configured (SyncMaxMBps > 0)
		if sr.maxBytesPerS > 0 {
			expectedElapsed := float64(sr.bytesRead) / sr.maxBytesPerS
			actualElapsed := time.Since(sr.startTime).Seconds()
			if expectedElapsed > actualElapsed {
				sleepDur := time.Duration((expectedElapsed - actualElapsed) * float64(time.Second))
				if sleepDur > 5*time.Millisecond {
					time.Sleep(sleepDur)
				}
			}
		}

		// Update live progress every ~1 MB
		if sr.accumUpdate >= 1024*1024 || (sr.baseOffset+sr.bytesRead) >= sr.totalBytes {
			sr.accumUpdate = 0
			doneBytes := sr.baseOffset + sr.bytesRead
			pct := 0
			if sr.totalBytes > 0 {
				pct = int(float64(doneBytes) * 100.0 / float64(sr.totalBytes))
				if pct > 100 {
					pct = 100
				}
			}
			elapsed := time.Since(sr.startTime).Seconds()
			speedMBps := 0.0
			if elapsed > 0.05 {
				speedMBps = (float64(sr.bytesRead) / (1024 * 1024)) / elapsed
			}
			status := fmt.Sprintf("Syncing %s: %d%% (%.1f MB/s)", sr.filename, pct, speedMBps)
			UpdateRemoteNodeStatus(sr.nodeID, status, pct)
		}
	}
	return n, err
}

// StartSyncScheduler runs in a background goroutine and triggers daily scheduled syncs
// when SyncSchedule is set to a 24-hour time string (e.g. "02:00").
func StartSyncScheduler() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for now := range ticker.C {
		cfg := GetConfig()
		sched := strings.TrimSpace(cfg.SyncSchedule)
		if sched == "" || strings.EqualFold(sched, "disabled") || strings.EqualFold(sched, "realtime") {
			continue
		}
		currentHM := now.Format("15:04")
		currentDayKey := now.Format("2006-01-02") + " " + currentHM
		if currentHM == sched && lastScheduled != currentDayKey {
			lastScheduled = currentDayKey
			TriggerWarehouseSyncIfNodesExist("Scheduled daily sync (" + sched + ")")
		}
	}
}

// TriggerWarehouseSyncIfNodesExist starts a background sync job if at least one remote node is registered
// and no sync job is currently active.
func TriggerWarehouseSyncIfNodesExist(reason string) {
	nodes, err := ListRemoteNodes()
	if err != nil || len(nodes) == 0 {
		return
	}
	RecordLog("INFO", "SYNC", "", "", fmt.Sprintf("Triggering warehouse node sync (%s) to %d node(s)", reason, len(nodes)))
	go runWarehouseSyncJob(nodes)
}

func runWarehouseSyncJob(nodes []RemoteNode) {
	syncJobMu.Lock()
	if syncJobRunning {
		syncJobMu.Unlock()
		return
	}
	syncJobRunning = true
	syncJobMu.Unlock()
	defer func() {
		syncJobMu.Lock()
		syncJobRunning = false
		syncJobMu.Unlock()
	}()

	cfg := GetConfig()
	osFiles, winFiles, linuxFiles, _, _ := scanImageFiles(cfg)
	var toSync []ImageFileInfo
	for _, f := range osFiles {
		if f.SyncEnabled {
			toSync = append(toSync, f)
		}
	}
	for _, f := range winFiles {
		if f.SyncEnabled {
			toSync = append(toSync, f)
		}
	}
	for _, f := range linuxFiles {
		if f.SyncEnabled {
			toSync = append(toSync, f)
		}
	}

	rules, _ := ListImageRules()

	for _, node := range nodes {
		UpdateRemoteNodeStatus(node.ID, "Checking remote manifest...", 2)
		manifest := fetchRemoteNodeManifest(node)

		skippedCount := 0
		syncedCount := 0
		allOK := true

		for _, img := range toSync {
			subdir := categoryToSubdir(img.Category)
			fullPath := filepath.Join(cfg.HTTPDir, "images", subdir, img.Name)
			stat, err := os.Stat(fullPath)
			if err != nil {
				UpdateRemoteNodeStatus(node.ID, fmt.Sprintf("Local file missing: %s", img.Name), 0)
				allOK = false
				break
			}

			UpdateRemoteNodeStatus(node.ID, fmt.Sprintf("Verifying SHA-256 for %s...", img.Name), 5)
			localSHA := GetOrComputeFileSHA256(img.Category, img.Name, fullPath, stat.Size(), stat.ModTime().Unix())

			key := strings.ToLower(img.Category) + "/" + img.Name
			remoteItem, existsOnRemote := manifest[key]

			// 1. Deduplication check: if remote node already has identical size + SHA-256, skip uploading!
			if existsOnRemote && remoteItem.SizeBytes == stat.Size() && remoteItem.SHA256 != "" && remoteItem.SHA256 == localSHA {
				skippedCount++
				RecordLog("INFO", "SYNC", "", node.URL, fmt.Sprintf("Skipped %s on '%s' (SHA-256 matches, already up to date)", img.Name, node.Name))
				continue
			}

			// 2. Resumable transfer check: if a partial .sync.part exists on remote node, resume from PartBytes!
			var resumeOffset int64
			if existsOnRemote && remoteItem.PartBytes > 0 && remoteItem.PartBytes < stat.Size() {
				resumeOffset = remoteItem.PartBytes
				RecordLog("INFO", "SYNC", "", node.URL, fmt.Sprintf("Resuming interrupted transfer of %s to '%s' from %s", img.Name, node.Name, formatBytes(resumeOffset)))
			}

			file, err := os.Open(fullPath)
			if err != nil {
				UpdateRemoteNodeStatus(node.ID, fmt.Sprintf("Failed opening %s: %v", img.Name, err), 0)
				allOK = false
				break
			}
			if resumeOffset > 0 {
				if _, seekErr := file.Seek(resumeOffset, io.SeekStart); seekErr != nil {
					resumeOffset = 0
					_, _ = file.Seek(0, io.SeekStart)
				}
			}

			var maxBytesPerS float64
			if cfg.SyncMaxMBps > 0 {
				maxBytesPerS = float64(cfg.SyncMaxMBps) * 1024 * 1024
			}

			streamReader := &syncStreamReader{
				reader:       file,
				nodeID:       node.ID,
				filename:     img.Name,
				baseOffset:   resumeOffset,
				totalBytes:   stat.Size(),
				maxBytesPerS: maxBytesPerS,
				startTime:    time.Now(),
			}

			targetURL := fmt.Sprintf("%s/api/sync/receive?category=%s&filename=%s", strings.TrimRight(node.URL, "/"), img.Category, img.Name)
			req, err := http.NewRequest(http.MethodPost, targetURL, streamReader)
			if err != nil {
				file.Close()
				UpdateRemoteNodeStatus(node.ID, fmt.Sprintf("Invalid node URL: %v", err), 0)
				allOK = false
				break
			}
			req.Header.Set("X-Sync-Key", node.APIKey)
			req.Header.Set("X-Resume-Offset", strconv.FormatInt(resumeOffset, 10))
			req.Header.Set("X-Expected-SHA256", localSHA)
			req.ContentLength = stat.Size() - resumeOffset

			client := &http.Client{Timeout: 6 * time.Hour}
			resp, err := client.Do(req)
			file.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				errMsg := "remote error"
				if err != nil {
					errMsg = err.Error()
				} else if resp != nil {
					bodyBytes, _ := io.ReadAll(resp.Body)
					errMsg = fmt.Sprintf("HTTP %d (%s)", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
					resp.Body.Close()
				}
				UpdateRemoteNodeStatus(node.ID, fmt.Sprintf("Error syncing %s: %s", img.Name, errMsg), 0)
				RecordLog("ERROR", "SYNC", "", node.URL, fmt.Sprintf("Failed syncing %s to %s: %s", img.Name, node.Name, errMsg))
				allOK = false
				break
			}
			resp.Body.Close()
			syncedCount++
			RecordLog("INFO", "SYNC", "", node.URL, fmt.Sprintf("Successfully synced %s (%s) to node '%s'", img.Name, img.SizeHuman, node.Name))
		}

		// 3. Replicate BIOS Manufacturer/Model -> OS WIM rules if enabled
		if allOK && cfg.SyncRulesWithImages {
			if err := pushRulesToRemoteNode(node, rules); err != nil {
				RecordLog("WARN", "SYNC", "", node.URL, fmt.Sprintf("Synced images to '%s' but rule replication failed: %v", node.Name, err))
			}
		}

		// 4. Pull remote warehouse device inventory and imaging logs back to Master DB
		mergedDevs, mergedLogs, telErr := pullTelemetryFromRemoteNode(node)
		if telErr != nil {
			RecordLog("WARN", "SYNC", "", node.URL, fmt.Sprintf("Telemetry DB pull from '%s' failed: %v", node.Name, telErr))
		} else if mergedDevs > 0 || mergedLogs > 0 {
			RecordLog("INFO", "SYNC", "", node.URL, fmt.Sprintf("Pulled %d device(s) and %d new log event(s) from warehouse node '%s'", mergedDevs, mergedLogs, node.Name))
		}

		if allOK {
			summary := fmt.Sprintf("Up to date (%d synced, %d unchanged, %d devs pulled)", syncedCount, skippedCount, mergedDevs)
			if cfg.SyncRulesWithImages {
				summary = fmt.Sprintf("Up to date (%d synced, %d unchanged, %d rules, %d devs)", syncedCount, skippedCount, len(rules), mergedDevs)
			}
			UpdateRemoteNodeStatus(node.ID, summary, 100)
		}
	}
}

// PullAllRemoteNodesTelemetry immediately pulls device records and imaging logs from all registered warehouse nodes
// into the master database. Can be triggered in real time from the Web UI.
func PullAllRemoteNodesTelemetry(nodes []RemoteNode) (totalDevs int, totalLogs int, lastErr error) {
	for _, node := range nodes {
		devs, logs, err := pullTelemetryFromRemoteNode(node)
		if err != nil {
			lastErr = fmt.Errorf("%s: %v", node.Name, err)
			RecordLog("WARN", "SYNC", "", node.URL, fmt.Sprintf("Real-time DB pull from '%s' failed: %v", node.Name, err))
			continue
		}
		totalDevs += devs
		totalLogs += logs
		UpdateRemoteNodeStatus(node.ID, fmt.Sprintf("DB synced (%d devices, +%d logs)", devs, logs), 100)
		RecordLog("INFO", "SYNC", "", node.URL, fmt.Sprintf("Real-time DB sync from '%s': merged %d device(s), %d new log(s)", node.Name, devs, logs))
	}
	return totalDevs, totalLogs, lastErr
}

func pullTelemetryFromRemoteNode(node RemoteNode) (int, int, error) {
	targetURL := fmt.Sprintf("%s/api/sync/telemetry", strings.TrimRight(node.URL, "/"))
	req, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("X-Sync-Key", node.APIKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("HTTP %d (%s)", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var payload struct {
		NodeName string         `json:"node_name"`
		Devices  []DeviceRecord `json:"devices"`
		Logs     []LogEntry     `json:"logs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, 0, err
	}

	remoteLabel := strings.TrimSpace(node.Name)
	if remoteLabel == "" {
		remoteLabel = strings.TrimSpace(payload.NodeName)
	}
	if remoteLabel == "" {
		remoteLabel = node.URL
	}

	mergedDevs, mergedLogs := MergeRemoteNodeTelemetry(remoteLabel, payload.Devices, payload.Logs)
	return mergedDevs, mergedLogs, nil
}

func fetchRemoteNodeManifest(node RemoteNode) map[string]RemoteManifestItem {
	result := make(map[string]RemoteManifestItem)
	targetURL := fmt.Sprintf("%s/api/sync/manifest", strings.TrimRight(node.URL, "/"))
	req, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		return result
	}
	req.Header.Set("X-Sync-Key", node.APIKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return result
	}

	var payload struct {
		Items []RemoteManifestItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return result
	}
	for _, item := range payload.Items {
		key := strings.ToLower(item.Category) + "/" + item.Filename
		result[key] = item
	}
	return result
}

func pushRulesToRemoteNode(node RemoteNode, rules []ImageRule) error {
	targetURL := fmt.Sprintf("%s/api/sync/rules", strings.TrimRight(node.URL, "/"))
	body, err := json.Marshal(map[string]any{"rules": rules})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sync-Key", node.APIKey)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// handleAPISyncManifest returns the local warehouse node's file inventory (sizes, SHA-256, and any partial .sync.part sizes)
// so the Base Office node can skip identical files and resume interrupted transfers.
func handleAPISyncManifest(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	providedKey := strings.TrimSpace(r.Header.Get("X-Sync-Key"))
	if providedKey == "" || providedKey != cfg.SyncSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid X-Sync-Key"})
		return
	}

	categories := []struct {
		cat    string
		subdir string
	}{
		{"os", "os"},
		{"windows", "winpe"},
		{"linux", "linux"},
	}

	var items []RemoteManifestItem
	for _, c := range categories {
		dirPath := filepath.Join(cfg.HTTPDir, "images", c.subdir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || strings.EqualFold(e.Name(), "README.txt") || strings.HasSuffix(e.Name(), ".sync.part") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			fullPath := filepath.Join(dirPath, e.Name())
			sha := GetOrComputeFileSHA256(c.cat, e.Name(), fullPath, info.Size(), info.ModTime().Unix())

			var partBytes int64
			if partInfo, pErr := os.Stat(fullPath + ".sync.part"); pErr == nil {
				partBytes = partInfo.Size()
			}

			items = append(items, RemoteManifestItem{
				Category:  c.cat,
				Filename:  e.Name(),
				SizeBytes: info.Size(),
				SHA256:    sha,
				PartBytes: partBytes,
			})
		}

		// Also include any standalone .sync.part files whose final file doesn't exist yet
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".sync.part") {
				origName := strings.TrimSuffix(e.Name(), ".sync.part")
				if _, origErr := os.Stat(filepath.Join(dirPath, origName)); os.IsNotExist(origErr) {
					if pInfo, pErr := e.Info(); pErr == nil {
						items = append(items, RemoteManifestItem{
							Category:  c.cat,
							Filename:  origName,
							SizeBytes: 0,
							SHA256:    "",
							PartBytes: pInfo.Size(),
						})
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleAPISyncRules receives replicated BIOS Manufacturer/Model -> OS WIM rules from the Base Office server.
func handleAPISyncRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	cfg := GetConfig()
	providedKey := strings.TrimSpace(r.Header.Get("X-Sync-Key"))
	if providedKey == "" || providedKey != cfg.SyncSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid X-Sync-Key"})
		return
	}

	var req struct {
		Rules []ImageRule `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := ReplaceAllImageRules(req.Rules); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	RecordLog("INFO", "SYNC", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Replicated %d BIOS Auto-Image rule(s) from primary server", len(req.Rules)))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleAPISyncReceive receives an image file pushed from the primary server (supporting resumable byte offsets),
// verifies the SHA-256 checksum of the complete .sync.part file, and atomically renames it to the target filename.
func handleAPISyncReceive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	cfg := GetConfig()
	providedKey := strings.TrimSpace(r.Header.Get("X-Sync-Key"))
	if providedKey == "" || providedKey != cfg.SyncSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid X-Sync-Key"})
		return
	}

	category := r.URL.Query().Get("category")
	filename := filepath.Base(strings.TrimSpace(r.URL.Query().Get("filename")))
	if filename == "" || filename == "." || filename == "/" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
		return
	}

	resumeOffset, _ := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-Resume-Offset")), 10, 64)
	expectedSHA := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Expected-SHA256")))

	subdir := categoryToSubdir(category)
	destDir := filepath.Join(cfg.HTTPDir, "images", subdir)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	tempPath := filepath.Join(destDir, filename+".sync.part")
	finalPath := filepath.Join(destDir, filename)

	var out *os.File
	var err error
	if resumeOffset > 0 {
		out, err = os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			if _, seekErr := out.Seek(resumeOffset, io.SeekStart); seekErr != nil {
				out.Close()
				err = seekErr
			}
		}
	} else {
		out, err = os.Create(tempPath)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	written, copyErr := io.Copy(out, r.Body)
	out.Close()
	if copyErr != nil {
		// Keep tempPath on disk so the next sync attempt can resume from the last written byte!
		RecordLog("WARN", "SYNC", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Interrupted sync receive for %s/%s after %s (saved .sync.part for resume)", subdir, filename, formatBytes(resumeOffset+written)))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": copyErr.Error()})
		return
	}

	// Verify full SHA-256 checksum before replacing the production WIM/image file
	if expectedSHA != "" {
		fCheck, err := os.Open(tempPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		h := sha256.New()
		_, _ = io.Copy(h, fCheck)
		fCheck.Close()
		actualSHA := strings.ToLower(hex.EncodeToString(h.Sum(nil)))
		if actualSHA != expectedSHA {
			_ = os.Remove(tempPath)
			RecordLog("ERROR", "SYNC", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("SHA-256 mismatch on synced file %s/%s (expected %s, got %s)", subdir, filename, expectedSHA, actualSHA))
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "SHA-256 checksum verification failed"})
			return
		}
	}

	if err := os.Rename(tempPath, finalPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if stat, sErr := os.Stat(finalPath); sErr == nil && expectedSHA != "" {
		_ = GetOrComputeFileSHA256(category, filename, finalPath, stat.Size(), stat.ModTime().Unix())
	}

	RecordLog("INFO", "SYNC", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Received & verified synced image %s/%s (%s total)", subdir, filename, formatBytes(resumeOffset+written)))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "bytes": resumeOffset + written})
}

// handleAPISyncTelemetry returns this warehouse node's discovered/imaged devices and recent imaging logs
// so the primary Base Office web host can display a unified multi-site device inventory and audit log.
func handleAPISyncTelemetry(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	providedKey := strings.TrimSpace(r.Header.Get("X-Sync-Key"))
	if providedKey == "" || providedKey != cfg.SyncSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid X-Sync-Key"})
		return
	}

	devices, _ := ListDevices()
	logs, _ := ListLogs(250, "ALL")

	nodeName := strings.TrimSpace(cfg.NodeName)
	if nodeName == "" {
		nodeName = cfg.ServerIP
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"node_name": nodeName,
		"devices":   devices,
		"logs":      logs,
	})
}

