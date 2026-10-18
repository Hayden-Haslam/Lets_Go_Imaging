package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ImageFileInfo describes a file in http_files/images/{os,winpe,linux}.
type ImageFileInfo struct {
	Name         string `json:"name"`
	Category     string `json:"category"` // "os", "windows", or "linux"
	SizeBytes    int64  `json:"size_bytes"`
	SizeHuman    string `json:"size_human"`
	ModifiedAt   string `json:"modified_at"`
	IsWim        bool   `json:"is_wim"`
	IsDefaultWim bool   `json:"is_default_wim"`
	Protected    bool   `json:"protected"` // core boot files like wimboot, bcd, boot.sdi
	SyncEnabled  bool   `json:"sync_enabled"`
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

// RegisterAPIRoutes mounts all REST API endpoints consumed by the Ruby Sinatra Web UI and WinPE clients.
func RegisterAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/overview", handleAPIOverview)
	mux.HandleFunc("/api/config", handleAPIConfig)

	// Authentication & User Management (RBAC)
	mux.HandleFunc("/api/auth/login", handleAPIAuthLogin)
	mux.HandleFunc("/api/auth/change-password", handleAPIAuthChangePassword)
	mux.HandleFunc("/api/users", handleAPIUsers)
	mux.HandleFunc("/api/users/delete", handleAPIUserDelete)

	// Image Management (OS WIMs, WinPE Boot WIMs, Linux Images)
	mux.HandleFunc("/api/images", handleAPIImages)
	mux.HandleFunc("/api/images/upload", handleAPIImageUpload)
	mux.HandleFunc("/api/images/delete", handleAPIImageDelete)
	mux.HandleFunc("/api/images/default-wim", handleAPISetDefaultWim)
	mux.HandleFunc("/api/images/sync-flag", handleAPIImageSyncFlag)

	// BIOS Manufacturer/Model -> Auto OS WIM Rules & WinPE Query Endpoint
	mux.HandleFunc("/api/rules", handleAPIRules)
	mux.HandleFunc("/api/rules/delete", handleAPIRuleDelete)
	mux.HandleFunc("/api/winpe/assignment", handleAPIWinPEAssignment)

	// Devices, Boot Queue, Transfers, Logs
	mux.HandleFunc("/api/devices", handleAPIDevices)
	mux.HandleFunc("/api/devices/status", handleAPIDeviceStatus)
	mux.HandleFunc("/api/devices/queue", handleAPIDeviceQueue)
	mux.HandleFunc("/api/transfers", handleAPITransfers)
	mux.HandleFunc("/api/logs", handleAPILogs)
	mux.HandleFunc("/api/simulate-boot", handleAPISimulateBoot)

	// Multi-Site / Warehouse Node Synchronization & System Updates
	mux.HandleFunc("/api/nodes", handleAPINodes)
	mux.HandleFunc("/api/nodes/delete", handleAPINodeDelete)
	mux.HandleFunc("/api/nodes/sync-now", handleAPINodesSyncNow)
	mux.HandleFunc("/api/nodes/pull-db", handleAPINodesPullDB)
	mux.HandleFunc("/api/sync/manifest", handleAPISyncManifest)
	mux.HandleFunc("/api/sync/rules", handleAPISyncRules)
	mux.HandleFunc("/api/sync/receive", handleAPISyncReceive)
	mux.HandleFunc("/api/sync/telemetry", handleAPISyncTelemetry)
	mux.HandleFunc("/api/system/update", handleAPISystemUpdate)
}

func handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	osFiles, winFiles, linuxFiles, winReady, linuxReady := scanImageFiles(cfg)
	devices, _ := ListDevices()
	transfers := GetRecentTransfers()
	logs, _ := ListLogs(50, r.URL.Query().Get("category"))
	rules, _ := ListImageRules()
	users, _ := ListUsers()
	nodes, _ := ListRemoteNodes()
	goUnit, rubyUnit := GenerateSystemdUnits(cfg)

	// Calculate Manufacturer breakdown analytics for the public view-only dashboard
	mfgBreakdown := make(map[string]int)
	for _, d := range devices {
		m := d.Manufacturer
		if m == "" {
			m = "Unknown"
		}
		mfgBreakdown[m]++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"config":        cfg,
		"interfaces":    ListNetworkInterfaces(),
		"os_files":      osFiles,
		"windows_files": winFiles,
		"linux_files":   linuxFiles,
		"winpe_ready":   winReady,
		"linux_ready":   linuxReady,
		"devices":       devices,
		"mfg_breakdown": mfgBreakdown,
		"transfers":     transfers,
		"rules":         rules,
		"users":         users,
		"nodes":         nodes,
		"logs":          logs,
		"systemd_go":    goUnit,
		"systemd_ruby":  rubyUnit,
	})
}

func handleAPIAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	user, err := AuthenticateUser(req.Username, req.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	RecordLog("INFO", "AUTH", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("User '%s' (%s) signed into Web UI", user.Username, user.Role))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "user": user})
}

func handleAPIAuthChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		Username    string `json:"username"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(strings.TrimSpace(req.NewPassword)) < 4 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Password must be at least 4 characters"})
		return
	}
	if err := UpdateUserPassword(req.Username, req.NewPassword); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	RecordLog("INFO", "AUTH", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("User '%s' updated their password", req.Username))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleAPIUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		users, _ := ListUsers()
		writeJSON(w, http.StatusOK, map[string]any{"users": users})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		Username           string `json:"username"`
		Password           string `json:"password"`
		Role               string `json:"role"`
		CanManageImages    bool   `json:"can_manage_images"`
		CanManageSettings  bool   `json:"can_manage_settings"`
		MustChangePassword bool   `json:"must_change_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := CreateOrUpdateUser(req.Username, req.Password, req.Role, req.CanManageImages, req.CanManageSettings, req.MustChangePassword); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	RecordLog("INFO", "AUTH", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Saved Web UI user '%s' (Role: %s)", req.Username, req.Role))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleAPIUserDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := DeleteUser(req.ID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func handleAPIRules(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		rules, _ := ListImageRules()
		writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		Name              string `json:"name"`
		ManufacturerMatch string `json:"manufacturer_match"`
		ModelMatch        string `json:"model_match"`
		TargetWim         string `json:"target_wim"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := CreateImageRule(req.Name, req.ManufacturerMatch, req.ModelMatch, req.TargetWim); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	RecordLog("INFO", "IMAGE", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Created BIOS Auto-Image Rule '%s' (%s / %s -> %s)", req.Name, req.ManufacturerMatch, req.ModelMatch, req.TargetWim))
	cfg := GetConfig()
	if strings.EqualFold(cfg.SyncSchedule, "realtime") && cfg.SyncRulesWithImages {
		TriggerWarehouseSyncIfNodesExist("Real-time rule update")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleAPIRuleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	_ = DeleteImageRule(req.ID)
	cfg := GetConfig()
	if strings.EqualFold(cfg.SyncSchedule, "realtime") && cfg.SyncRulesWithImages {
		TriggerWarehouseSyncIfNodesExist("Real-time rule deletion")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleAPIWinPEAssignment allows a booted WinPE environment to query which OS WIM (in /images/os/)
// should be automatically applied based on its MAC address or BIOS Manufacturer/Model rule!
func handleAPIWinPEAssignment(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	mac := parseIPXEMAC(r.URL.Query().Get("mac"))
	mfg := r.URL.Query().Get("mfg")
	model := r.URL.Query().Get("model")
	if mac == "" {
		mac, _ = LookupDeviceByIP(cleanClientIP(r.RemoteAddr))
	}

	targetWim, matchSource := ResolveTargetOSWim(mac, mfg, model)
	baseURL := serverBaseURL(cfg, r)
	targetURL := ""
	if targetWim != "" {
		targetURL = fmt.Sprintf("%s/images/os/%s", baseURL, targetWim)
	}

	osWims := listFilesWithExt(filepath.Join(cfg.HTTPDir, "images", "os"), ".wim")
	writeJSON(w, http.StatusOK, map[string]any{
		"mac":               mac,
		"target_os_wim":     targetWim,
		"target_os_wim_url": targetURL,
		"match_source":      matchSource,
		"auto_apply":        targetWim != "",
		"available_os_wims": osWims,
	})
}

func handleAPIDeviceQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		MAC            string `json:"mac"`
		NextBootAction string `json:"next_boot_action"`
		AssignedOSWim  string `json:"assigned_os_wim"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := SetDeviceQueue(req.MAC, req.NextBootAction, req.AssignedOSWim); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	RecordLog("INFO", "IMAGING", parseIPXEMAC(req.MAC), cleanClientIP(r.RemoteAddr), fmt.Sprintf("Queued next boot action '%s' (OS WIM: %s)", req.NextBootAction, req.AssignedOSWim))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	if strings.TrimSpace(incoming.NodeName) != "" {
		cfg.NodeName = strings.TrimSpace(incoming.NodeName)
	}
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
	if strings.TrimSpace(incoming.SyncSecret) != "" {
		cfg.SyncSecret = strings.TrimSpace(incoming.SyncSecret)
	}
	if incoming.SyncMaxMBps >= 0 {
		cfg.SyncMaxMBps = incoming.SyncMaxMBps
	}
	if strings.TrimSpace(incoming.SyncSchedule) != "" {
		cfg.SyncSchedule = strings.TrimSpace(incoming.SyncSchedule)
	}
	cfg.SyncRulesWithImages = incoming.SyncRulesWithImages
	cfg.AutoLaunchWinPEInstaller = incoming.AutoLaunchWinPEInstaller
	cfg.SetupCompleted = true

	if err := SaveConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_, _ = WriteAlmaLinuxSetupScript(cfg)
	RecordLog("INFO", "SETUP", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Updated server configuration (Node: %s, IP: %s, SyncSchedule: %s, SyncMaxMBps: %d)", cfg.NodeName, cfg.ServerIP, cfg.SyncSchedule, cfg.SyncMaxMBps))

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"config": cfg,
	})
}

func handleAPIImages(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	osFiles, winFiles, linuxFiles, winReady, linuxReady := scanImageFiles(cfg)
	writeJSON(w, http.StatusOK, map[string]any{
		"os_files":      osFiles,
		"windows_files": winFiles,
		"linux_files":   linuxFiles,
		"winpe_ready":   winReady,
		"linux_ready":   linuxReady,
		"default_wim":   cfg.DefaultWim,
	})
}

func categoryToSubdir(category string) string {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "os":
		return "os"
	case "linux":
		return "linux"
	default:
		return "winpe"
	}
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

	category := "os"
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
			category = strings.ToLower(strings.TrimSpace(string(buf)))
			part.Close()
			continue
		}

		filename := filepath.Base(strings.TrimSpace(part.FileName()))
		if filename == "" || filename == "." || filename == "/" {
			part.Close()
			continue
		}

		targetSubdir := categoryToSubdir(category)
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

	lower := strings.ToLower(filename)
	if lower == "wimboot" || lower == "bcd" || lower == "boot.sdi" || lower == "winpeshl.ini" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("'%s' is a core WinPE bootloader file and is protected from deletion", filename)})
		return
	}

	cfg := GetConfig()
	subdir := categoryToSubdir(req.Category)
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

	RecordLog("INFO", "IMAGE", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Set default WinPE boot WIM to '%s'", filename))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "default_wim": filename})
}

func handleAPIImageSyncFlag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		Category    string `json:"category"`
		Filename    string `json:"filename"`
		SyncEnabled bool   `json:"sync_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := SetImageSyncFlag(req.Category, req.Filename, req.SyncEnabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	RecordLog("INFO", "SYNC", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Set sync flag=%v for %s/%s", req.SyncEnabled, req.Category, req.Filename))
	cfg := GetConfig()
	if req.SyncEnabled && strings.EqualFold(cfg.SyncSchedule, "realtime") {
		TriggerWarehouseSyncIfNodesExist(fmt.Sprintf("Real-time flag enabled for %s", req.Filename))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleAPINodes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		nodes, _ := ListRemoteNodes()
		writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		Name   string `json:"name"`
		URL    string `json:"url"`
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := CreateRemoteNode(req.Name, req.URL, req.APIKey); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	RecordLog("INFO", "SYNC", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Added remote warehouse node '%s' (%s)", req.Name, req.URL))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleAPINodeDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	_ = DeleteRemoteNode(req.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleAPINodesSyncNow pushes all "Sync-Enabled" images to registered remote warehouse nodes in the background.
func handleAPINodesSyncNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	nodes, _ := ListRemoteNodes()
	if len(nodes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No remote warehouse nodes registered yet"})
		return
	}

	go runWarehouseSyncJob(nodes)
	writeJSON(w, http.StatusOK, map[string]string{"status": "started", "message": "Started background image & DB sync with remote warehouse nodes"})
}

// handleAPINodesPullDB forces a real-time pull of device records and logs from all registered warehouse nodes.
func handleAPINodesPullDB(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	nodes, _ := ListRemoteNodes()
	if len(nodes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No remote warehouse nodes registered yet"})
		return
	}

	devs, logs, err := PullAllRemoteNodesTelemetry(nodes)
	msg := fmt.Sprintf("Real-time DB sync complete: pulled %d device(s) and %d new log event(s) from %d warehouse node(s).", devs, logs, len(nodes))
	if err != nil {
		msg = fmt.Sprintf("Pulled %d device(s) and %d log(s), with warning: %v", devs, logs, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"merged_devices": devs,
		"merged_logs":    logs,
		"message":        msg,
	})
}

// handleAPISystemUpdate executes the root update.sh script to pull the latest repo code, rebuild/update, and restart services.
func handleAPISystemUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}

	// Locate update.sh in repo root (either ../update.sh from ipxe-server/ or ./update.sh)
	scriptPath := filepath.Clean("../update.sh")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		if _, err2 := os.Stat("update.sh"); err2 == nil {
			scriptPath = "update.sh"
		} else {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "update.sh not found in repository root"})
			return
		}
	}

	absScript, _ := filepath.Abs(scriptPath)
	repoRoot := filepath.Dir(absScript)

	if runtime.GOOS == "windows" {
		RecordLog("INFO", "SYSTEM", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("Update script requested on Windows host (%s); update.sh targets AlmaLinux/Linux", absScript))
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "simulated_on_windows",
			"message": fmt.Sprintf("Verified %s exists. On AlmaLinux 10, this executes 'bash %s --bg-restart' to git pull, build/update, and restart systemd services.", absScript, absScript),
		})
		return
	}

	RecordLog("INFO", "SYSTEM", "", cleanClientIP(r.RemoteAddr), "Running root update.sh from Web UI...")

	// Run update.sh with --no-restart first so we can capture git pull + build output and return it to the Web UI,
	// then trigger the service restart asynchronously 1 second later so the HTTP response finishes cleanly.
	cmd := exec.Command("bash", absScript, "--no-restart")
	cmd.Dir = repoRoot
	outBytes, err := cmd.CombinedOutput()
	outputStr := strings.TrimSpace(string(outBytes))
	if err != nil {
		RecordLog("ERROR", "SYSTEM", "", cleanClientIP(r.RemoteAddr), fmt.Sprintf("update.sh failed: %v — %s", err, outputStr))
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":  fmt.Sprintf("update.sh failed (%v): %s", err, outputStr),
			"output": outputStr,
		})
		return
	}

	RecordLog("INFO", "SYSTEM", "", cleanClientIP(r.RemoteAddr), "update.sh succeeded; restarting services in 1s")

	go func() {
		time.Sleep(1200 * time.Millisecond)
		restartCmd := exec.Command("bash", absScript, "--restart-only")
		restartCmd.Dir = repoRoot
		_ = restartCmd.Run()
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": "Application updated via update.sh! Services are restarting now...",
		"output":  outputStr,
	})
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
	RecordLog("INFO", "IMAGING", parseIPXEMAC(req.MAC), ip, fmt.Sprintf("Device status update: %s (%d%%)", req.Status, req.ProgressPct))
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

// handleAPISimulateBoot simulates a PXE hardware check-in and live image download.
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
	asset := "ASSET-7010-HQ"
	bios := "1.18.0"
	chip := "Intel I219-LM"

	UpsertDevice(mac, ip, mfg, model, serial, uuid, arch, asset, bios, chip, "At iPXE Boot Menu", cfg.DefaultWim)
	RecordLog("INFO", "PXE", mac, ip, fmt.Sprintf("Simulated PXE boot check-in: %s %s (Serial: %s, BIOS: %s)", mfg, model, serial, bios))

	go func() {
		totalBytes := int64(450 * 1024 * 1024)
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

func scanImageFiles(cfg ServerConfig) (osFiles []ImageFileInfo, winFiles []ImageFileInfo, linuxFiles []ImageFileInfo, winpeReady bool, linuxReady bool) {
	osDir := filepath.Join(cfg.HTTPDir, "images", "os")
	winDir := filepath.Join(cfg.HTTPDir, "images", "winpe")
	linuxDir := filepath.Join(cfg.HTTPDir, "images", "linux")
	syncFlags := GetImageSyncFlagsMap()

	if entries, err := os.ReadDir(osDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.EqualFold(e.Name(), "README.txt") || strings.HasSuffix(e.Name(), ".sync.part") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			name := e.Name()
			osFiles = append(osFiles, ImageFileInfo{
				Name:        name,
				Category:    "os",
				SizeBytes:   info.Size(),
				SizeHuman:   formatBytes(info.Size()),
				ModifiedAt:  info.ModTime().Format("2006-01-02 15:04"),
				IsWim:       strings.HasSuffix(strings.ToLower(name), ".wim"),
				SyncEnabled: syncFlags["os/"+name],
			})
		}
	}

	hasWimboot := false
	hasBCD := false
	hasSDI := false
	hasDefaultWim := false

	if entries, err := os.ReadDir(winDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.HasSuffix(e.Name(), ".sync.part") {
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
				SyncEnabled:  syncFlags["windows/"+name],
			})
		}
	}
	winpeReady = hasWimboot && hasBCD && hasSDI && hasDefaultWim

	hasKernel := false
	hasInitrd := false
	if entries, err := os.ReadDir(linuxDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.EqualFold(e.Name(), "README.txt") || strings.HasSuffix(e.Name(), ".sync.part") {
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
				Name:        name,
				Category:    "linux",
				SizeBytes:   info.Size(),
				SizeHuman:   formatBytes(info.Size()),
				ModifiedAt:  info.ModTime().Format("2006-01-02 15:04"),
				SyncEnabled: syncFlags["linux/"+name],
			})
		}
	}
	linuxReady = hasKernel && hasInitrd

	if osFiles == nil {
		osFiles = []ImageFileInfo{}
	}
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
