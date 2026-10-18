package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ImageFileInfo describes a file in http_files/images/winpe or http_files/images/linux.
type ImageFileInfo struct {
	Name         string `json:"name"`
	Category     string `json:"category"` // "windows" or "linux"
	SizeBytes    int64  `json:"size_bytes"`
	SizeHuman    string `json:"size_human"`
	ModifiedAt   string `json:"modified_at"`
	IsWim        bool   `json:"is_wim"`
	IsDefaultWim bool   `json:"is_default_wim"`
	Protected    bool   `json:"protected"` // core boot files like wimboot, bcd, boot.sdi
}

// progressResponseWriter wraps http.ResponseWriter to track real-time bytes sent during image downloads.
type progressResponseWriter struct {
	http.ResponseWriter
	transferID string
	bytesAccum int64
}

func (prw *progressResponseWriter) Write(b []byte) (int, error) {
	n, err := prw.ResponseWriter.Write(b)
	if n > 0 {
		prw.bytesAccum += int64(n)
		// Batch updates every ~64KB to keep lock contention minimal on fast gigabit links
		if prw.bytesAccum >= 65536 {
			UpdateTransfer(prw.transferID, prw.bytesAccum)
			prw.bytesAccum = 0
		}
	}
	return n, err
}

func (prw *progressResponseWriter) FlushRemaining() {
	if prw.bytesAccum > 0 {
		UpdateTransfer(prw.transferID, prw.bytesAccum)
		prw.bytesAccum = 0
	}
}

// serveHTTPFilesWithProgress serves files from http_files/ and tracks live download progress
// for any OS image or boot payload (.wim, .sdi, vmlinuz, .img, .iso).
func serveHTTPFilesWithProgress(fileServer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := GetConfig()
		cleanRel := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		fullPath := filepath.Join(cfg.HTTPDir, cleanRel)

		stat, err := os.Stat(fullPath)
		if err == nil && !stat.IsDir() && isTrackedBootPayload(fullPath, stat.Size()) {
			baseName := filepath.Base(fullPath)
			clientIP := cleanClientIP(r.RemoteAddr)
			mac, _ := LookupDeviceByIP(clientIP)
			RecordLog("INFO", "HTTP", mac, clientIP, fmt.Sprintf("Started downloading %s (%s)", baseName, formatBytes(stat.Size())))

			transferID := StartTransfer(r.RemoteAddr, baseName, stat.Size())
			prw := &progressResponseWriter{
				ResponseWriter: w,
				transferID:     transferID,
			}
			defer func() {
				prw.FlushRemaining()
				FinishTransfer(transferID)
			}()
			fileServer.ServeHTTP(prw, r)
			return
		}

		RecordLog("INFO", "HTTP", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("GET %s", r.URL.Path))
		fileServer.ServeHTTP(w, r)
	})
}

func isTrackedBootPayload(path string, size int64) bool {
	lower := strings.ToLower(filepath.Base(path))
	if strings.HasSuffix(lower, ".wim") ||
		strings.HasSuffix(lower, ".sdi") ||
		strings.HasSuffix(lower, ".img") ||
		strings.HasSuffix(lower, ".iso") ||
		lower == "vmlinuz" {
		return true
	}
	return size > 1024*1024 // Track any file larger than 1 MB
}

// RegisterAPIRoutes mounts all REST API endpoints consumed by the Ruby Sinatra Web UI.
func RegisterAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/overview", handleAPIOverview)
	mux.HandleFunc("/api/config", handleAPIConfig)
	mux.HandleFunc("/api/images", handleAPIImages)
	mux.HandleFunc("/api/images/upload", handleAPIImageUpload)
	mux.HandleFunc("/api/images/delete", handleAPIImageDelete)
	mux.HandleFunc("/api/images/default-wim", handleAPISetDefaultWim)
	mux.HandleFunc("/api/devices", handleAPIDevices)
	mux.HandleFunc("/api/devices/status", handleAPIDeviceStatus)
	mux.HandleFunc("/api/transfers", handleAPITransfers)
	mux.HandleFunc("/api/logs", handleAPILogs)
	mux.HandleFunc("/api/simulate-boot", handleAPISimulateBoot)
}

func handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	winFiles, linuxFiles, winReady, linuxReady := scanImageFiles(cfg)
	devices, _ := ListDevices()
	transfers := GetRecentTransfers()
	logs, _ := ListLogs(50, r.URL.Query().Get("category"))
	goUnit, rubyUnit := GenerateSystemdUnits(cfg)

	writeJSON(w, http.StatusOK, map[string]any{
		"config":        cfg,
		"interfaces":    ListNetworkInterfaces(),
		"windows_files": winFiles,
		"linux_files":   linuxFiles,
		"winpe_ready":   winReady,
		"linux_ready":   linuxReady,
		"devices":       devices,
		"transfers":     transfers,
		"logs":          logs,
		"systemd_go":    goUnit,
		"systemd_ruby":  rubyUnit,
	})
}

func handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg := GetConfig()
		writeJSON(w, http.StatusOK, map[string]any{
			"config":     cfg,
			"interfaces": ListNetworkInterfaces(),
		})
		return
	}

	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var incoming ServerConfig
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	cfg := GetConfig()
	if strings.TrimSpace(incoming.ServerIP) != "" {
		cfg.ServerIP = strings.TrimSpace(incoming.ServerIP)
	}
	if incoming.TFTPPort > 0 && incoming.TFTPPort <= 65535 {
		cfg.TFTPPort = incoming.TFTPPort
	}
	if incoming.HTTPPort > 0 && incoming.HTTPPort <= 65535 {
		cfg.HTTPPort = incoming.HTTPPort
	}
	if incoming.RubyWebPort > 0 && incoming.RubyWebPort <= 65535 {
		cfg.RubyWebPort = incoming.RubyWebPort
	}
	if strings.TrimSpace(incoming.DefaultWim) != "" {
		cfg.DefaultWim = filepath.Base(strings.TrimSpace(incoming.DefaultWim))
	}
	if strings.TrimSpace(incoming.LinuxKernelArgs) != "" {
		cfg.LinuxKernelArgs = strings.TrimSpace(incoming.LinuxKernelArgs)
	}
	if incoming.MenuTimeoutMs >= 1000 {
		cfg.MenuTimeoutMs = incoming.MenuTimeoutMs
	}
	cfg.AutoLaunchWinPEInstaller = incoming.AutoLaunchWinPEInstaller
	cfg.SetupCompleted = true

	if err := SaveConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_, _ = WriteAlmaLinuxSetupScript(cfg)
	RecordLog("INFO", "SETUP", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Updated server configuration (Server IP: %s, Default WIM: %s)", cfg.ServerIP, cfg.DefaultWim))

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"config": cfg,
	})
}

func handleAPIImages(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	winFiles, linuxFiles, winReady, linuxReady := scanImageFiles(cfg)
	writeJSON(w, http.StatusOK, map[string]any{
		"windows_files": winFiles,
		"linux_files":   linuxFiles,
		"winpe_ready":   winReady,
		"linux_ready":   linuxReady,
		"default_wim":   cfg.DefaultWim,
	})
}

// handleAPIImageUpload streams large .wim or Linux image uploads directly to disk without buffering in memory.
func handleAPIImageUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}

	cfg := GetConfig()
	reader, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart upload: " + err.Error()})
		return
	}

	category := "windows"
	var savedFiles []string

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "error reading upload stream: " + err.Error()})
			return
		}

		if part.FormName() == "category" {
			buf, _ := io.ReadAll(part)
			val := strings.ToLower(strings.TrimSpace(string(buf)))
			if val == "linux" || val == "windows" {
				category = val
			}
			part.Close()
			continue
		}

		filename := filepath.Base(strings.TrimSpace(part.FileName()))
		if filename == "" || filename == "." || filename == "/" {
			part.Close()
			continue
		}

		targetSubdir := "winpe"
		if category == "linux" {
			targetSubdir = "linux"
		}
		destDir := filepath.Join(cfg.HTTPDir, "images", targetSubdir)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			part.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		destPath := filepath.Join(destDir, filename)
		outFile, err := os.Create(destPath)
		if err != nil {
			part.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		written, copyErr := io.Copy(outFile, part)
		outFile.Close()
		part.Close()
		if copyErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": copyErr.Error()})
			return
		}

		savedFiles = append(savedFiles, filename)
		RecordLog("INFO", "IMAGE", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Uploaded %s image file '%s' (%s)", category, filename, formatBytes(written)))

		// If this is a .wim file and the configured DefaultWim doesn't exist yet, set this as DefaultWim
		if category == "windows" && strings.HasSuffix(strings.ToLower(filename), ".wim") {
			defaultWimPath := filepath.Join(destDir, cfg.DefaultWim)
			if _, statErr := os.Stat(defaultWimPath); os.IsNotExist(statErr) {
				cfg.DefaultWim = filename
				_ = SaveConfig(cfg)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"saved_files": savedFiles,
	})
}

func handleAPIImageDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST or DELETE required"})
		return
	}

	var req struct {
		Category string `json:"category"`
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	filename := filepath.Base(strings.TrimSpace(req.Filename))
	if filename == "" || filename == "." || filename == "/" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
		return
	}

	// Protect essential WinPE base loader files from accidental deletion
	lower := strings.ToLower(filename)
	if lower == "wimboot" || lower == "bcd" || lower == "boot.sdi" || lower == "winpeshl.ini" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("'%s' is a core WinPE bootloader file and is protected from deletion", filename)})
		return
	}

	cfg := GetConfig()
	subdir := "winpe"
	if strings.EqualFold(req.Category, "linux") {
		subdir = "linux"
	}
	targetPath := filepath.Join(cfg.HTTPDir, "images", subdir, filename)
	if err := os.Remove(targetPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	RecordLog("INFO", "IMAGE", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Deleted %s image file '%s'", subdir, filename))
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "filename": filename})
}

func handleAPISetDefaultWim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	filename := filepath.Base(strings.TrimSpace(req.Filename))
	cfg := GetConfig()
	cfg.DefaultWim = filename
	if err := SaveConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	RecordLog("INFO", "IMAGE", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Set default Windows WIM to '%s'", filename))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "default_wim": filename})
}

func handleAPIDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := ListDevices()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func handleAPIDeviceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		MAC         string `json:"mac"`
		Status      string `json:"status"`
		Image       string `json:"image"`
		ProgressPct int    `json:"progress_pct"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	ip := cleanClientIP(r.RemoteAddr)
	UpdateDeviceStatus(req.MAC, ip, req.Status, req.Image, req.ProgressPct)
	RecordLog("INFO", "IMAGING", normalizeMAC(req.MAC), ip, fmt.Sprintf("Device status update: %s (%d%%)", req.Status, req.ProgressPct))
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func handleAPITransfers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"transfers": GetRecentTransfers(),
	})
}

func handleAPILogs(w http.ResponseWriter, r *http.Request) {
	limit := 150
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	category := r.URL.Query().Get("category")
	logs, err := ListLogs(limit, category)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

// handleAPISimulateBoot simulates a PXE hardware check-in and live image download so the user
// can test hardware logging and real-time download progress bars on AlmaLinux 10 without a 2nd machine.
func handleAPISimulateBoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	cfg := GetConfig()
	mac := "00:15:5D:8A:4F:21"
	ip := "192.168.1.142"
	mfg := "Dell Inc."
	model := "OptiPlex 7010"
	serial := "DL7010X99"
	uuid := "4c4c4544-004c-3710-8031-c4c04f583939"
	arch := "x86_64"

	UpsertDevice(mac, ip, mfg, model, serial, uuid, arch, "At iPXE Boot Menu", cfg.DefaultWim)
	RecordLog("INFO", "PXE", mac, ip, fmt.Sprintf("Simulated PXE boot check-in: %s %s (Serial: %s)", mfg, model, serial))

	// Simulate an active 8-second WIM download in the background so the Web UI progress bar animates live
	go func() {
		totalBytes := int64(450 * 1024 * 1024) // 450 MB simulated WIM
		tid := StartTransfer(ip, cfg.DefaultWim, totalBytes)
		chunk := totalBytes / 16
		for i := 0; i < 16; i++ {
			time.Sleep(500 * time.Millisecond)
			UpdateTransfer(tid, chunk)
		}
		FinishTransfer(tid)
	}()

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "simulation_started",
		"message": "Simulated PXE client check-in (Dell OptiPlex 7010) and live WIM download started!",
	})
}

func scanImageFiles(cfg ServerConfig) (winFiles []ImageFileInfo, linuxFiles []ImageFileInfo, winpeReady bool, linuxReady bool) {
	winDir := filepath.Join(cfg.HTTPDir, "images", "winpe")
	linuxDir := filepath.Join(cfg.HTTPDir, "images", "linux")

	hasWimboot := false
	hasBCD := false
	hasSDI := false
	hasDefaultWim := false

	if entries, err := os.ReadDir(winDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			name := e.Name()
			lower := strings.ToLower(name)
			isWim := strings.HasSuffix(lower, ".wim")
			isDef := strings.EqualFold(name, cfg.DefaultWim)
			protected := lower == "wimboot" || lower == "bcd" || lower == "boot.sdi" || lower == "winpeshl.ini"

			if lower == "wimboot" {
				hasWimboot = true
			}
			if lower == "bcd" {
				hasBCD = true
			}
			if lower == "boot.sdi" {
				hasSDI = true
			}
			if isDef {
				hasDefaultWim = true
			}

			winFiles = append(winFiles, ImageFileInfo{
				Name:         name,
				Category:     "windows",
				SizeBytes:    info.Size(),
				SizeHuman:    formatBytes(info.Size()),
				ModifiedAt:   info.ModTime().Format("2006-01-02 15:04"),
				IsWim:        isWim,
				IsDefaultWim: isDef,
				Protected:    protected,
			})
		}
	}
	winpeReady = hasWimboot && hasBCD && hasSDI && hasDefaultWim

	hasKernel := false
	hasInitrd := false
	if entries, err := os.ReadDir(linuxDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.EqualFold(e.Name(), "README.txt") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			name := e.Name()
			lower := strings.ToLower(name)
			if lower == "vmlinuz" {
				hasKernel = true
			}
			if lower == "initrd.img" {
				hasInitrd = true
			}
			linuxFiles = append(linuxFiles, ImageFileInfo{
				Name:       name,
				Category:   "linux",
				SizeBytes:  info.Size(),
				SizeHuman:  formatBytes(info.Size()),
				ModifiedAt: info.ModTime().Format("2006-01-02 15:04"),
			})
		}
	}
	linuxReady = hasKernel && hasInitrd

	if winFiles == nil {
		winFiles = []ImageFileInfo{}
	}
	if linuxFiles == nil {
		linuxFiles = []ImageFileInfo{}
	}
	return
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
