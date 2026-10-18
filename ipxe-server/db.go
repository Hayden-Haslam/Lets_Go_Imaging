package main

import (
	"database/sql"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	dbConn      *sql.DB
	transfersMu sync.RWMutex
	// activeTransfers maps transfer ID -> live TransferProgress
	activeTransfers = make(map[string]*TransferProgress)
)

// DeviceRecord stores hardware inventory and current imaging status for a PXE client.
type DeviceRecord struct {
	MAC           string `json:"mac"`
	IP            string `json:"ip"`
	Manufacturer  string `json:"manufacturer"`
	Model         string `json:"model"`
	Serial        string `json:"serial"`
	UUID          string `json:"uuid"`
	Arch          string `json:"arch"`
	FirstSeen     string `json:"first_seen"`
	LastSeen      string `json:"last_seen"`
	BootCount     int    `json:"boot_count"`
	CurrentStatus string `json:"current_status"`
	CurrentImage  string `json:"current_image"`
	ProgressPct   int    `json:"progress_pct"`
}

// LogEntry represents a structured event log row in SQLite.
type LogEntry struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
	Level     string `json:"level"`
	Category  string `json:"category"`
	DeviceMAC string `json:"device_mac"`
	ClientIP  string `json:"client_ip"`
	Message   string `json:"message"`
}

// TransferProgress tracks real-time and recent file downloads (e.g. boot.wim, vmlinuz, initrd.img).
type TransferProgress struct {
	ID          string  `json:"id"`
	ClientIP    string  `json:"client_ip"`
	DeviceMAC   string  `json:"device_mac"`
	DeviceModel string  `json:"device_model"`
	Filename    string  `json:"filename"`
	TotalBytes  int64   `json:"total_bytes"`
	BytesSent   int64   `json:"bytes_sent"`
	ProgressPct float64 `json:"progress_pct"`
	SpeedMBps   float64 `json:"speed_mbps"`
	Status      string  `json:"status"` // "downloading", "completed", "interrupted"
	StartedAt   string  `json:"started_at"`
	UpdatedAt   string  `json:"updated_at"`
	startTime   time.Time
}

// InitDB opens the SQLite database and creates tables if they do not exist.
func InitDB(dbPath string) error {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)

	schema := `
	CREATE TABLE IF NOT EXISTS devices (
		mac TEXT PRIMARY KEY,
		ip TEXT NOT NULL DEFAULT '',
		manufacturer TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		serial TEXT NOT NULL DEFAULT '',
		uuid TEXT NOT NULL DEFAULT '',
		arch TEXT NOT NULL DEFAULT '',
		first_seen TEXT NOT NULL,
		last_seen TEXT NOT NULL,
		boot_count INTEGER NOT NULL DEFAULT 1,
		current_status TEXT NOT NULL DEFAULT 'Discovered',
		current_image TEXT NOT NULL DEFAULT '',
		progress_pct INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at TEXT NOT NULL,
		level TEXT NOT NULL,
		category TEXT NOT NULL,
		device_mac TEXT NOT NULL DEFAULT '',
		client_ip TEXT NOT NULL DEFAULT '',
		message TEXT NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_logs_created_at ON logs(id DESC);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	dbConn = db
	return nil
}

// RecordLog writes a log entry both to standard output and to the SQLite logs table.
func RecordLog(level, category, deviceMAC, clientIP, message string) {
	now := time.Now().Format("2006-01-02 15:04:05")
	log.Printf("[%s][%s] (IP:%s MAC:%s) %s", level, category, clientIP, deviceMAC, message)

	if dbConn == nil {
		return
	}
	_, err := dbConn.Exec(
		`INSERT INTO logs (created_at, level, category, device_mac, client_ip, message) VALUES (?, ?, ?, ?, ?, ?)`,
		now, level, category, deviceMAC, clientIP, message,
	)
	if err != nil {
		log.Printf("[DB] Error writing log: %v", err)
	}
}

// UpsertDevice registers or updates a PXE client's hardware inventory in SQLite.
func UpsertDevice(mac, ip, manufacturer, model, serial, uuid, arch, status, image string) {
	mac = normalizeMAC(mac)
	if mac == "" {
		mac = "ip-" + cleanClientIP(ip)
	}
	now := time.Now().Format("2006-01-02 15:04:05")

	if dbConn == nil {
		return
	}

	var existingCount int
	err := dbConn.QueryRow(`SELECT boot_count FROM devices WHERE mac = ?`, mac).Scan(&existingCount)
	if err == sql.ErrNoRows {
		_, err = dbConn.Exec(`
			INSERT INTO devices (mac, ip, manufacturer, model, serial, uuid, arch, first_seen, last_seen, boot_count, current_status, current_image, progress_pct)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, 0)
		`, mac, cleanClientIP(ip), cleanField(manufacturer), cleanField(model), cleanField(serial), cleanField(uuid), cleanField(arch), now, now, status, image)
		if err != nil {
			log.Printf("[DB] Insert device error: %v", err)
		}
		return
	}

	_, err = dbConn.Exec(`
		UPDATE devices SET
			ip = CASE WHEN ? != '' THEN ? ELSE ip END,
			manufacturer = CASE WHEN ? != '' THEN ? ELSE manufacturer END,
			model = CASE WHEN ? != '' THEN ? ELSE model END,
			serial = CASE WHEN ? != '' THEN ? ELSE serial END,
			uuid = CASE WHEN ? != '' THEN ? ELSE uuid END,
			arch = CASE WHEN ? != '' THEN ? ELSE arch END,
			last_seen = ?,
			boot_count = boot_count + 1,
			current_status = CASE WHEN ? != '' THEN ? ELSE current_status END,
			current_image = CASE WHEN ? != '' THEN ? ELSE current_image END
		WHERE mac = ?
	`,
		cleanClientIP(ip), cleanClientIP(ip),
		cleanField(manufacturer), cleanField(manufacturer),
		cleanField(model), cleanField(model),
		cleanField(serial), cleanField(serial),
		cleanField(uuid), cleanField(uuid),
		cleanField(arch), cleanField(arch),
		now,
		status, status,
		image, image,
		mac,
	)
	if err != nil {
		log.Printf("[DB] Update device error: %v", err)
	}
}

// UpdateDeviceStatus updates the status, image, and progress percentage of a device by MAC or IP.
func UpdateDeviceStatus(mac, ip, status, image string, progressPct int) {
	if dbConn == nil {
		return
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	mac = normalizeMAC(mac)
	cleanIP := cleanClientIP(ip)

	if mac != "" {
		_, _ = dbConn.Exec(`
			UPDATE devices
			SET current_status = ?,
			    current_image = CASE WHEN ? != '' THEN ? ELSE current_image END,
			    progress_pct = ?,
			    last_seen = ?
			WHERE mac = ?
		`, status, image, image, progressPct, now, mac)
		return
	}

	if cleanIP != "" {
		_, _ = dbConn.Exec(`
			UPDATE devices
			SET current_status = ?,
			    current_image = CASE WHEN ? != '' THEN ? ELSE current_image END,
			    progress_pct = ?,
			    last_seen = ?
			WHERE ip = ?
		`, status, image, image, progressPct, now, cleanIP)
	}
}

// LookupDeviceByIP finds the most recently active device for a given client IP.
func LookupDeviceByIP(ip string) (mac string, model string) {
	if dbConn == nil {
		return "", ""
	}
	cleanIP := cleanClientIP(ip)
	var mfr, mdl string
	err := dbConn.QueryRow(`SELECT mac, manufacturer, model FROM devices WHERE ip = ? ORDER BY last_seen DESC LIMIT 1`, cleanIP).Scan(&mac, &mfr, &mdl)
	if err != nil {
		return "", ""
	}
	return mac, strings.TrimSpace(mfr + " " + mdl)
}

// ListDevices returns all discovered PXE devices ordered by last_seen descending.
func ListDevices() ([]DeviceRecord, error) {
	if dbConn == nil {
		return []DeviceRecord{}, nil
	}
	rows, err := dbConn.Query(`
		SELECT mac, ip, manufacturer, model, serial, uuid, arch, first_seen, last_seen, boot_count, current_status, current_image, progress_pct
		FROM devices
		ORDER BY last_seen DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []DeviceRecord
	for rows.Next() {
		var d DeviceRecord
		if err := rows.Scan(&d.MAC, &d.IP, &d.Manufacturer, &d.Model, &d.Serial, &d.UUID, &d.Arch, &d.FirstSeen, &d.LastSeen, &d.BootCount, &d.CurrentStatus, &d.CurrentImage, &d.ProgressPct); err != nil {
			continue
		}
		list = append(list, d)
	}
	if list == nil {
		list = []DeviceRecord{}
	}
	return list, nil
}

// ListLogs returns the most recent log entries, optionally filtered by category.
func ListLogs(limit int, category string) ([]LogEntry, error) {
	if dbConn == nil {
		return []LogEntry{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 150
	}

	var rows *sql.Rows
	var err error
	if category != "" && !strings.EqualFold(category, "all") {
		rows, err = dbConn.Query(`
			SELECT id, created_at, level, category, device_mac, client_ip, message
			FROM logs
			WHERE LOWER(category) = LOWER(?)
			ORDER BY id DESC LIMIT ?
		`, category, limit)
	} else {
		rows, err = dbConn.Query(`
			SELECT id, created_at, level, category, device_mac, client_ip, message
			FROM logs
			ORDER BY id DESC LIMIT ?
		`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Level, &e.Category, &e.DeviceMAC, &e.ClientIP, &e.Message); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if entries == nil {
		entries = []LogEntry{}
	}
	return entries, nil
}

// StartTransfer initializes a live download session for a client pulling an image file.
func StartTransfer(clientAddr, filename string, totalBytes int64) string {
	ip := cleanClientIP(clientAddr)
	mac, model := LookupDeviceByIP(ip)
	now := time.Now()
	id := fmt.Sprintf("%s-%s-%d", ip, filename, now.UnixNano())

	transfersMu.Lock()
	activeTransfers[id] = &TransferProgress{
		ID:          id,
		ClientIP:    ip,
		DeviceMAC:   mac,
		DeviceModel: model,
		Filename:    filename,
		TotalBytes:  totalBytes,
		BytesSent:   0,
		ProgressPct: 0,
		SpeedMBps:   0,
		Status:      "downloading",
		StartedAt:   now.Format("15:04:05"),
		UpdatedAt:   now.Format("15:04:05"),
		startTime:   now,
	}
	transfersMu.Unlock()

	UpdateDeviceStatus(mac, ip, fmt.Sprintf("Downloading %s", filename), filename, 0)
	return id
}

// UpdateTransfer increments bytes sent and computes live progress % and MB/s speed.
func UpdateTransfer(id string, bytesDelta int64) {
	transfersMu.Lock()
	t, ok := activeTransfers[id]
	if !ok {
		transfersMu.Unlock()
		return
	}
	t.BytesSent += bytesDelta
	if t.TotalBytes > 0 {
		t.ProgressPct = float64(t.BytesSent) * 100.0 / float64(t.TotalBytes)
		if t.ProgressPct > 100 {
			t.ProgressPct = 100
		}
	}
	elapsed := time.Since(t.startTime).Seconds()
	if elapsed > 0.05 {
		t.SpeedMBps = (float64(t.BytesSent) / (1024 * 1024)) / elapsed
	}
	t.UpdatedAt = time.Now().Format("15:04:05")
	pctInt := int(t.ProgressPct)
	mac := t.DeviceMAC
	ip := t.ClientIP
	fname := t.Filename
	transfersMu.Unlock()

	UpdateDeviceStatus(mac, ip, fmt.Sprintf("Downloading %s (%d%%)", fname, pctInt), fname, pctInt)
}

// FinishTransfer marks a transfer as completed or interrupted.
func FinishTransfer(id string) {
	transfersMu.Lock()
	t, ok := activeTransfers[id]
	if !ok {
		transfersMu.Unlock()
		return
	}
	if t.TotalBytes > 0 && t.BytesSent >= t.TotalBytes {
		t.ProgressPct = 100.0
		t.Status = "completed"
	} else {
		t.Status = "interrupted"
	}
	t.UpdatedAt = time.Now().Format("15:04:05")
	status := t.Status
	mac := t.DeviceMAC
	ip := t.ClientIP
	fname := t.Filename
	pctInt := int(t.ProgressPct)
	transfersMu.Unlock()

	if status == "completed" {
		UpdateDeviceStatus(mac, ip, fmt.Sprintf("Downloaded %s - Booting/Applying", fname), fname, 100)
		RecordLog("INFO", "IMAGING", mac, ip, fmt.Sprintf("Completed downloading image file %s", fname))
	} else {
		UpdateDeviceStatus(mac, ip, fmt.Sprintf("Transfer interrupted (%s at %d%%)", fname, pctInt), fname, pctInt)
	}
}

// GetRecentTransfers returns active and recently completed transfers (within the last 30 minutes).
func GetRecentTransfers() []TransferProgress {
	transfersMu.RLock()
	defer transfersMu.RUnlock()

	var result []TransferProgress
	cutoff := time.Now().Add(-30 * time.Minute)
	for _, t := range activeTransfers {
		if t.Status == "downloading" || t.startTime.After(cutoff) {
			result = append(result, *t)
		}
	}
	if result == nil {
		result = []TransferProgress{}
	}
	return result
}

func cleanClientIP(addr string) string {
	addr = strings.TrimSpace(addr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func normalizeMAC(mac string) string {
	mac = strings.TrimSpace(strings.ToUpper(mac))
	mac = strings.ReplaceAll(mac, "-", ":")
	return mac
}

func cleanField(val string) string {
	val = strings.TrimSpace(val)
	if val == "" || strings.HasPrefix(val, "${") {
		return "Unknown"
	}
	return val
}
