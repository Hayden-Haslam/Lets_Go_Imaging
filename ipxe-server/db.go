package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
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

// DeviceRecord stores hardware inventory, next-boot queue, and current imaging status for a PXE client.
type DeviceRecord struct {
	MAC            string `json:"mac"`
	IP             string `json:"ip"`
	NodeName       string `json:"node_name"`
	Manufacturer   string `json:"manufacturer"`
	Model          string `json:"model"`
	Serial         string `json:"serial"`
	UUID           string `json:"uuid"`
	Arch           string `json:"arch"`
	AssetTag       string `json:"asset_tag"`
	BIOSVersion    string `json:"bios_version"`
	NICChip        string `json:"nic_chip"`
	FirstSeen      string `json:"first_seen"`
	LastSeen       string `json:"last_seen"`
	BootCount      int    `json:"boot_count"`
	CurrentStatus  string `json:"current_status"`
	CurrentImage   string `json:"current_image"`
	ProgressPct    int    `json:"progress_pct"`
	NextBootAction string `json:"next_boot_action"` // "menu", "exit", "winpe", "linux", or "os:<wim>"
	AssignedOSWim  string `json:"assigned_os_wim"`
	MatchedOSWim   string `json:"matched_os_wim"`
	MatchedRule    string `json:"matched_rule"`
}

// ImageRule maps BIOS Manufacturer / Model to an OS WIM in http_files/images/os/ for zero-prompt imaging.
type ImageRule struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	ManufacturerMatch string `json:"manufacturer_match"`
	ModelMatch        string `json:"model_match"`
	TargetWim         string `json:"target_wim"`
	CreatedAt         string `json:"created_at"`
}

// UserAccount represents a Web UI user with role-based permissions.
type UserAccount struct {
	ID                 int64  `json:"id"`
	Username           string `json:"username"`
	Role               string `json:"role"` // "admin", "operator", "viewer"
	CanManageImages    bool   `json:"can_manage_images"`
	CanManageSettings  bool   `json:"can_manage_settings"`
	MustChangePassword bool   `json:"must_change_password"`
	CreatedAt          string `json:"created_at"`
}

// RemoteNode represents a warehouse or branch imaging server that receives synced images.
type RemoteNode struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	APIKey         string `json:"api_key"`
	LastSyncAt     string `json:"last_sync_at"`
	LastSyncStatus string `json:"last_sync_status"`
	SyncProgress   int    `json:"sync_progress_pct"`
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

// TransferProgress tracks real-time and recent file downloads (e.g. boot.wim, OS WIMs, vmlinuz).
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

// InitDB opens the SQLite database, creates/migrates tables, and seeds the initial admin user.
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
		node_name TEXT NOT NULL DEFAULT '',
		manufacturer TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		serial TEXT NOT NULL DEFAULT '',
		uuid TEXT NOT NULL DEFAULT '',
		arch TEXT NOT NULL DEFAULT '',
		asset_tag TEXT NOT NULL DEFAULT 'Unknown',
		bios_version TEXT NOT NULL DEFAULT 'Unknown',
		nic_chip TEXT NOT NULL DEFAULT 'Unknown',
		first_seen TEXT NOT NULL,
		last_seen TEXT NOT NULL,
		boot_count INTEGER NOT NULL DEFAULT 1,
		current_status TEXT NOT NULL DEFAULT 'Discovered',
		current_image TEXT NOT NULL DEFAULT '',
		progress_pct INTEGER NOT NULL DEFAULT 0,
		next_boot_action TEXT NOT NULL DEFAULT 'menu',
		assigned_os_wim TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS image_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		manufacturer_match TEXT NOT NULL,
		model_match TEXT NOT NULL DEFAULT '*',
		target_wim TEXT NOT NULL,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		role TEXT NOT NULL DEFAULT 'viewer',
		can_manage_images INTEGER NOT NULL DEFAULT 0,
		can_manage_settings INTEGER NOT NULL DEFAULT 0,
		must_change_password INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS image_sync_flags (
		category TEXT NOT NULL,
		filename TEXT NOT NULL,
		sync_enabled INTEGER NOT NULL DEFAULT 0,
		sha256 TEXT NOT NULL DEFAULT '',
		size_bytes INTEGER NOT NULL DEFAULT 0,
		mod_unix INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (category, filename)
	);

	CREATE TABLE IF NOT EXISTS remote_nodes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		url TEXT NOT NULL,
		api_key TEXT NOT NULL,
		last_sync_at TEXT NOT NULL DEFAULT 'Never',
		last_sync_status TEXT NOT NULL DEFAULT 'Idle',
		sync_progress_pct INTEGER NOT NULL DEFAULT 0
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

	// Safe column migrations for existing databases
	migrations := []string{
		`ALTER TABLE devices ADD COLUMN node_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN asset_tag TEXT NOT NULL DEFAULT 'Unknown'`,
		`ALTER TABLE devices ADD COLUMN bios_version TEXT NOT NULL DEFAULT 'Unknown'`,
		`ALTER TABLE devices ADD COLUMN nic_chip TEXT NOT NULL DEFAULT 'Unknown'`,
		`ALTER TABLE devices ADD COLUMN next_boot_action TEXT NOT NULL DEFAULT 'menu'`,
		`ALTER TABLE devices ADD COLUMN assigned_os_wim TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE image_sync_flags ADD COLUMN sha256 TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE image_sync_flags ADD COLUMN size_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE image_sync_flags ADD COLUMN mod_unix INTEGER NOT NULL DEFAULT 0`,
	}
	for _, m := range migrations {
		_, _ = db.Exec(m)
	}

	// Clean up any legacy rows that had binary-encoded MAC/IP or ip- fallback keys
	_, _ = db.Exec(`DELETE FROM devices WHERE mac LIKE 'ip-%' OR length(mac) < 12`)

	dbConn = db
	ensureDefaultAdminUser()
	return nil
}

func hashPassword(username, password string) string {
	h := sha256.Sum256([]byte("lgi-salt-v1:" + strings.ToLower(strings.TrimSpace(username)) + ":" + password))
	return hex.EncodeToString(h[:])
}

func ensureDefaultAdminUser() {
	if dbConn == nil {
		return
	}
	var count int
	_ = dbConn.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count)
	if count == 0 {
		now := time.Now().Format("2006-01-02 15:04:05")
		uname := strings.TrimSpace(initialAdminUsername)
		if uname == "" {
			uname = "admin"
		}
		pass := initialAdminPassword
		if pass == "" {
			pass = "admin123"
		}
		_, _ = dbConn.Exec(`
			INSERT INTO users (username, password_hash, role, can_manage_images, can_manage_settings, must_change_password, created_at)
			VALUES (?, ?, 'admin', 1, 1, 1, ?)
		`, uname, hashPassword(uname, pass), now)
		log.Printf("[AUTH] Seeded initial admin user '%s' (password reset required on first login)", uname)
	}
}

// AuthenticateUser verifies username & password and returns the UserAccount if valid.
func AuthenticateUser(username, password string) (*UserAccount, error) {
	if dbConn == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	uname := strings.TrimSpace(username)
	expectedHash := hashPassword(uname, password)

	var u UserAccount
	var storedHash string
	var canImg, canSet, mustChange int
	err := dbConn.QueryRow(`
		SELECT id, username, password_hash, role, can_manage_images, can_manage_settings, must_change_password, created_at
		FROM users WHERE LOWER(username) = LOWER(?)
	`, uname).Scan(&u.ID, &u.Username, &storedHash, &u.Role, &canImg, &canSet, &mustChange, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid username or password")
	}

	if storedHash != expectedHash {
		return nil, fmt.Errorf("invalid username or password")
	}
	u.CanManageImages = canImg == 1 || u.Role == "admin"
	u.CanManageSettings = canSet == 1 || u.Role == "admin"
	u.MustChangePassword = mustChange == 1
	return &u, nil
}

// UpdateUserPassword changes a user's password and clears the must_change_password flag.
func UpdateUserPassword(username, newPassword string) error {
	if dbConn == nil {
		return fmt.Errorf("database not initialized")
	}
	uname := strings.TrimSpace(username)
	newHash := hashPassword(uname, newPassword)
	_, err := dbConn.Exec(`UPDATE users SET password_hash = ?, must_change_password = 0 WHERE LOWER(username) = LOWER(?)`, newHash, uname)
	return err
}

// ListUsers returns all Web UI users.
func ListUsers() ([]UserAccount, error) {
	if dbConn == nil {
		return []UserAccount{}, nil
	}
	rows, err := dbConn.Query(`
		SELECT id, username, role, can_manage_images, can_manage_settings, must_change_password, created_at
		FROM users ORDER BY id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []UserAccount
	for rows.Next() {
		var u UserAccount
		var canImg, canSet, mustChange int
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &canImg, &canSet, &mustChange, &u.CreatedAt); err != nil {
			continue
		}
		u.CanManageImages = canImg == 1 || u.Role == "admin"
		u.CanManageSettings = canSet == 1 || u.Role == "admin"
		u.MustChangePassword = mustChange == 1
		list = append(list, u)
	}
	if list == nil {
		list = []UserAccount{}
	}
	return list, nil
}

// CreateOrUpdateUser creates or updates a Web UI user account.
func CreateOrUpdateUser(username, password, role string, canManageImages, canManageSettings, mustChange bool) error {
	if dbConn == nil {
		return fmt.Errorf("database not initialized")
	}
	uname := strings.TrimSpace(username)
	if uname == "" || password == "" {
		return fmt.Errorf("username and password are required")
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	canImg := 0
	if canManageImages || role == "admin" {
		canImg = 1
	}
	canSet := 0
	if canManageSettings || role == "admin" {
		canSet = 1
	}
	mc := 0
	if mustChange {
		mc = 1
	}
	_, err := dbConn.Exec(`
		INSERT INTO users (username, password_hash, role, can_manage_images, can_manage_settings, must_change_password, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(username) DO UPDATE SET
			password_hash = excluded.password_hash,
			role = excluded.role,
			can_manage_images = excluded.can_manage_images,
			can_manage_settings = excluded.can_manage_settings,
			must_change_password = excluded.must_change_password
	`, uname, hashPassword(uname, password), role, canImg, canSet, mc, now)
	return err
}

// DeleteUser removes a user account by ID (preventing deletion of the last admin).
func DeleteUser(id int64) error {
	if dbConn == nil {
		return nil
	}
	var adminCount int
	_ = dbConn.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&adminCount)
	var targetRole string
	_ = dbConn.QueryRow(`SELECT role FROM users WHERE id = ?`, id).Scan(&targetRole)
	if targetRole == "admin" && adminCount <= 1 {
		return fmt.Errorf("cannot delete the only remaining admin account")
	}
	_, err := dbConn.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
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

// UpsertDevice registers or updates a PXE client's full SMBIOS hardware inventory in SQLite.
func UpsertDevice(mac, ip, manufacturer, model, serial, uuid, arch, assetTag, biosVersion, nicChip, status, image string) {
	mac = parseIPXEMAC(mac)
	ip = cleanClientIP(ip)
	if mac == "" {
		mac = "ip-" + ip
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	nodeName := GetConfig().NodeName
	if nodeName == "" {
		nodeName = "Primary-Imaging-Node"
	}

	if dbConn == nil {
		return
	}

	mfg := cleanField(manufacturer)
	mdl := cleanField(model)
	ser := cleanField(serial)
	uid := cleanField(uuid)
	ar := cleanField(arch)
	asset := cleanField(assetTag)
	bios := cleanField(biosVersion)
	chip := cleanField(nicChip)

	var existingCount int
	err := dbConn.QueryRow(`SELECT boot_count FROM devices WHERE mac = ?`, mac).Scan(&existingCount)
	if err == sql.ErrNoRows {
		_, err = dbConn.Exec(`
			INSERT INTO devices (mac, ip, node_name, manufacturer, model, serial, uuid, arch, asset_tag, bios_version, nic_chip, first_seen, last_seen, boot_count, current_status, current_image, progress_pct, next_boot_action, assigned_os_wim)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, 0, 'menu', '')
		`, mac, ip, nodeName, mfg, mdl, ser, uid, ar, asset, bios, chip, now, now, status, image)
		if err != nil {
			log.Printf("[DB] Insert device error: %v", err)
		}
		return
	}

	_, err = dbConn.Exec(`
		UPDATE devices SET
			ip = CASE WHEN ? != '' THEN ? ELSE ip END,
			node_name = ?,
			manufacturer = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE manufacturer END,
			model = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE model END,
			serial = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE serial END,
			uuid = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE uuid END,
			arch = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE arch END,
			asset_tag = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE asset_tag END,
			bios_version = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE bios_version END,
			nic_chip = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE nic_chip END,
			last_seen = ?,
			boot_count = boot_count + 1,
			current_status = CASE WHEN ? != '' THEN ? ELSE current_status END,
			current_image = CASE WHEN ? != '' THEN ? ELSE current_image END
		WHERE mac = ?
	`,
		ip, ip,
		nodeName,
		mfg, mfg, mfg,
		mdl, mdl, mdl,
		ser, ser, ser,
		uid, uid, uid,
		ar, ar, ar,
		asset, asset, asset,
		bios, bios, bios,
		chip, chip, chip,
		now,
		status, status,
		image, image,
		mac,
	)
	if err != nil {
		log.Printf("[DB] Update device error: %v", err)
	}
}

// ConsumeDeviceNextBootAction checks if a device has a queued one-time boot action (e.g., "winpe", "linux", "os:Dell.wim")
// and immediately resets next_boot_action to "menu" (which defaults to exit) to prevent boot loops!
func ConsumeDeviceNextBootAction(mac string) (action string, assignedOSWim string) {
	if dbConn == nil {
		return "menu", ""
	}
	mac = parseIPXEMAC(mac)
	if mac == "" {
		return "menu", ""
	}
	err := dbConn.QueryRow(`SELECT next_boot_action, assigned_os_wim FROM devices WHERE mac = ?`, mac).Scan(&action, &assignedOSWim)
	if err != nil || action == "" {
		return "menu", ""
	}
	if action != "menu" && action != "exit" {
		// Reset back to menu (which defaults to exit on timeout) so the device never boot-loops after imaging
		_, _ = dbConn.Exec(`UPDATE devices SET next_boot_action = 'menu' WHERE mac = ?`, mac)
	}
	return action, assignedOSWim
}

// SetDeviceQueue updates a device's next boot action and/or assigned OS WIM.
func SetDeviceQueue(mac, nextBootAction, assignedOSWim string) error {
	if dbConn == nil {
		return nil
	}
	mac = parseIPXEMAC(mac)
	_, err := dbConn.Exec(`
		UPDATE devices SET next_boot_action = ?, assigned_os_wim = ? WHERE mac = ?
	`, nextBootAction, assignedOSWim, mac)
	return err
}

// ResolveTargetOSWim determines which OS WIM (in http_files/images/os/) should be applied to a device.
// Priority: 1) Device's explicit assigned_os_wim, 2) BIOS Manufacturer/Model Auto-Image Rules.
func ResolveTargetOSWim(mac, manufacturer, model string) (targetWim string, matchSource string) {
	if dbConn == nil {
		return "", ""
	}
	mac = parseIPXEMAC(mac)
	if mac != "" {
		var assigned, dbMfg, dbMdl string
		if err := dbConn.QueryRow(`SELECT assigned_os_wim, manufacturer, model FROM devices WHERE mac = ?`, mac).Scan(&assigned, &dbMfg, &dbMdl); err == nil {
			if manufacturer == "" || manufacturer == "Unknown" {
				manufacturer = dbMfg
			}
			if model == "" || model == "Unknown" {
				model = dbMdl
			}
			if strings.TrimSpace(assigned) != "" {
				return strings.TrimSpace(assigned), "Assigned to Device"
			}
		}
	}

	rules, err := ListImageRules()
	if err != nil {
		return "", ""
	}

	mfgLower := strings.ToLower(strings.TrimSpace(manufacturer))
	mdlLower := strings.ToLower(strings.TrimSpace(model))

	for _, r := range rules {
		rMfg := strings.ToLower(strings.TrimSpace(r.ManufacturerMatch))
		rMdl := strings.ToLower(strings.TrimSpace(r.ModelMatch))

		mfgMatches := rMfg == "*" || rMfg == "" || (mfgLower != "" && strings.Contains(mfgLower, rMfg))
		mdlMatches := rMdl == "*" || rMdl == "" || (mdlLower != "" && strings.Contains(mdlLower, rMdl))

		if mfgMatches && mdlMatches {
			return r.TargetWim, fmt.Sprintf("Auto-Rule: %s", r.Name)
		}
	}
	return "", ""
}

// ListImageRules returns all BIOS Manufacturer/Model -> OS WIM auto-selection rules.
func ListImageRules() ([]ImageRule, error) {
	if dbConn == nil {
		return []ImageRule{}, nil
	}
	rows, err := dbConn.Query(`SELECT id, name, manufacturer_match, model_match, target_wim, created_at FROM image_rules ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []ImageRule
	for rows.Next() {
		var r ImageRule
		if err := rows.Scan(&r.ID, &r.Name, &r.ManufacturerMatch, &r.ModelMatch, &r.TargetWim, &r.CreatedAt); err != nil {
			continue
		}
		list = append(list, r)
	}
	if list == nil {
		list = []ImageRule{}
	}
	return list, nil
}

// CreateImageRule creates a new BIOS Manufacturer/Model -> OS WIM auto-selection rule.
func CreateImageRule(name, manufacturerMatch, modelMatch, targetWim string) error {
	if dbConn == nil {
		return nil
	}
	if strings.TrimSpace(modelMatch) == "" {
		modelMatch = "*"
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	_, err := dbConn.Exec(`
		INSERT INTO image_rules (name, manufacturer_match, model_match, target_wim, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, strings.TrimSpace(name), strings.TrimSpace(manufacturerMatch), strings.TrimSpace(modelMatch), strings.TrimSpace(targetWim), now)
	return err
}

// DeleteImageRule deletes an auto-image rule by ID.
func DeleteImageRule(id int64) error {
	if dbConn == nil {
		return nil
	}
	_, err := dbConn.Exec(`DELETE FROM image_rules WHERE id = ?`, id)
	return err
}

// ReplaceAllImageRules replaces local BIOS Auto-Image rules with the rule set pushed from the primary office server.
func ReplaceAllImageRules(rules []ImageRule) error {
	if dbConn == nil {
		return nil
	}
	tx, err := dbConn.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM image_rules`); err != nil {
		_ = tx.Rollback()
		return err
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	for _, r := range rules {
		created := r.CreatedAt
		if created == "" {
			created = now
		}
		if _, err := tx.Exec(`
			INSERT INTO image_rules (name, manufacturer_match, model_match, target_wim, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, r.Name, r.ManufacturerMatch, r.ModelMatch, r.TargetWim, created); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// GetOrComputeFileSHA256 returns the cached SHA-256 hash of an image file if size & mtime are unchanged,
// or computes and caches it in SQLite so multi-GB WIM files are only hashed when modified.
func GetOrComputeFileSHA256(category, filename, fullPath string, sizeBytes, modUnix int64) string {
	cat := strings.ToLower(strings.TrimSpace(category))
	if dbConn != nil {
		var cachedHash string
		var cachedSize, cachedMod int64
		err := dbConn.QueryRow(`
			SELECT sha256, size_bytes, mod_unix FROM image_sync_flags WHERE category = ? AND filename = ?
		`, cat, filename).Scan(&cachedHash, &cachedSize, &cachedMod)
		if err == nil && cachedHash != "" && cachedSize == sizeBytes && cachedMod == modUnix {
			return cachedHash
		}
	}

	f, err := os.Open(fullPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	sum := hex.EncodeToString(h.Sum(nil))

	if dbConn != nil {
		_, _ = dbConn.Exec(`
			INSERT INTO image_sync_flags (category, filename, sync_enabled, sha256, size_bytes, mod_unix)
			VALUES (?, ?, 0, ?, ?, ?)
			ON CONFLICT(category, filename) DO UPDATE SET
				sha256 = excluded.sha256,
				size_bytes = excluded.size_bytes,
				mod_unix = excluded.mod_unix
		`, cat, filename, sum, sizeBytes, modUnix)
	}
	return sum
}

// SetImageSyncFlag toggles whether an image is flagged for syncing to remote warehouse nodes.
func SetImageSyncFlag(category, filename string, enabled bool) error {
	if dbConn == nil {
		return nil
	}
	val := 0
	if enabled {
		val = 1
	}
	_, err := dbConn.Exec(`
		INSERT INTO image_sync_flags (category, filename, sync_enabled)
		VALUES (?, ?, ?)
		ON CONFLICT(category, filename) DO UPDATE SET sync_enabled = excluded.sync_enabled
	`, strings.ToLower(category), filename, val)
	return err
}

// GetImageSyncFlagsMap returns a map of "category/filename" -> bool.
func GetImageSyncFlagsMap() map[string]bool {
	res := make(map[string]bool)
	if dbConn == nil {
		return res
	}
	rows, err := dbConn.Query(`SELECT category, filename, sync_enabled FROM image_sync_flags`)
	if err != nil {
		return res
	}
	defer rows.Close()
	for rows.Next() {
		var cat, fname string
		var en int
		if err := rows.Scan(&cat, &fname, &en); err == nil {
			res[strings.ToLower(cat)+"/"+fname] = en == 1
		}
	}
	return res
}

// ListRemoteNodes returns all registered warehouse/remote imaging nodes.
func ListRemoteNodes() ([]RemoteNode, error) {
	if dbConn == nil {
		return []RemoteNode{}, nil
	}
	rows, err := dbConn.Query(`SELECT id, name, url, api_key, last_sync_at, last_sync_status, sync_progress_pct FROM remote_nodes ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []RemoteNode
	for rows.Next() {
		var n RemoteNode
		if err := rows.Scan(&n.ID, &n.Name, &n.URL, &n.APIKey, &n.LastSyncAt, &n.LastSyncStatus, &n.SyncProgress); err == nil {
			list = append(list, n)
		}
	}
	if list == nil {
		list = []RemoteNode{}
	}
	return list, nil
}

// CreateRemoteNode registers a warehouse/remote node for image synchronization.
func CreateRemoteNode(name, url, apiKey string) error {
	if dbConn == nil {
		return nil
	}
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	_, err := dbConn.Exec(`INSERT INTO remote_nodes (name, url, api_key) VALUES (?, ?, ?)`, strings.TrimSpace(name), url, strings.TrimSpace(apiKey))
	return err
}

// DeleteRemoteNode removes a remote node by ID.
func DeleteRemoteNode(id int64) error {
	if dbConn == nil {
		return nil
	}
	_, err := dbConn.Exec(`DELETE FROM remote_nodes WHERE id = ?`, id)
	return err
}

// UpdateRemoteNodeStatus updates the sync status and progress percentage for a warehouse node.
func UpdateRemoteNodeStatus(id int64, status string, progressPct int) {
	if dbConn == nil {
		return
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	_, _ = dbConn.Exec(`UPDATE remote_nodes SET last_sync_at = ?, last_sync_status = ?, sync_progress_pct = ? WHERE id = ?`, now, status, progressPct, id)
}

// UpdateDeviceStatus updates the status, image, and progress percentage of a device by MAC or IP.
func UpdateDeviceStatus(mac, ip, status, image string, progressPct int) {
	if dbConn == nil {
		return
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	mac = parseIPXEMAC(mac)
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

// ListDevices returns all discovered PXE devices ordered by last_seen descending, enriched with matched OS WIM rules.
func ListDevices() ([]DeviceRecord, error) {
	if dbConn == nil {
		return []DeviceRecord{}, nil
	}
	rows, err := dbConn.Query(`
		SELECT mac, ip, node_name, manufacturer, model, serial, uuid, arch, asset_tag, bios_version, nic_chip,
		       first_seen, last_seen, boot_count, current_status, current_image, progress_pct,
		       next_boot_action, assigned_os_wim
		FROM devices
		ORDER BY last_seen DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	defaultNode := GetConfig().NodeName
	if defaultNode == "" {
		defaultNode = "Primary-Imaging-Node"
	}

	var list []DeviceRecord
	for rows.Next() {
		var d DeviceRecord
		if err := rows.Scan(&d.MAC, &d.IP, &d.NodeName, &d.Manufacturer, &d.Model, &d.Serial, &d.UUID, &d.Arch, &d.AssetTag, &d.BIOSVersion, &d.NICChip, &d.FirstSeen, &d.LastSeen, &d.BootCount, &d.CurrentStatus, &d.CurrentImage, &d.ProgressPct, &d.NextBootAction, &d.AssignedOSWim); err != nil {
			continue
		}
		if d.NodeName == "" {
			d.NodeName = defaultNode
		}
		list = append(list, d)
	}
	rows.Close()

	for i := range list {
		wim, src := ResolveTargetOSWim(list[i].MAC, list[i].Manufacturer, list[i].Model)
		list[i].MatchedOSWim = wim
		list[i].MatchedRule = src
	}

	if list == nil {
		list = []DeviceRecord{}
	}
	return list, nil
}

// MergeRemoteNodeTelemetry merges device inventory and imaging logs pulled from a remote warehouse node
// into the Master SQLite database so the Master Web UI shows devices imaged across all nodes.
func MergeRemoteNodeTelemetry(remoteNodeName string, remoteDevices []DeviceRecord, remoteLogs []LogEntry) (mergedDevices int, mergedLogs int) {
	if dbConn == nil {
		return 0, 0
	}
	if strings.TrimSpace(remoteNodeName) == "" {
		remoteNodeName = "Warehouse-Node"
	}

	for _, d := range remoteDevices {
		mac := parseIPXEMAC(d.MAC)
		if mac == "" {
			continue
		}
		nodeTag := strings.TrimSpace(d.NodeName)
		if nodeTag == "" {
			nodeTag = remoteNodeName
		}

		var existingLastSeen string
		err := dbConn.QueryRow(`SELECT last_seen FROM devices WHERE mac = ?`, mac).Scan(&existingLastSeen)
		if err == sql.ErrNoRows {
			_, iErr := dbConn.Exec(`
				INSERT INTO devices (mac, ip, node_name, manufacturer, model, serial, uuid, arch, asset_tag, bios_version, nic_chip,
				                     first_seen, last_seen, boot_count, current_status, current_image, progress_pct, next_boot_action, assigned_os_wim)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, mac, d.IP, nodeTag, d.Manufacturer, d.Model, d.Serial, d.UUID, d.Arch, d.AssetTag, d.BIOSVersion, d.NICChip,
				d.FirstSeen, d.LastSeen, d.BootCount, d.CurrentStatus, d.CurrentImage, d.ProgressPct, d.NextBootAction, d.AssignedOSWim)
			if iErr == nil {
				mergedDevices++
			}
			continue
		}

		// Update if the remote node saw or imaged the device at or after our local last_seen timestamp
		if d.LastSeen >= existingLastSeen {
			_, uErr := dbConn.Exec(`
				UPDATE devices SET
					ip = ?,
					node_name = ?,
					manufacturer = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE manufacturer END,
					model = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE model END,
					serial = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE serial END,
					uuid = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE uuid END,
					arch = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE arch END,
					asset_tag = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE asset_tag END,
					bios_version = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE bios_version END,
					nic_chip = CASE WHEN ? != '' AND ? != 'Unknown' THEN ? ELSE nic_chip END,
					last_seen = ?,
					boot_count = CASE WHEN ? > boot_count THEN ? ELSE boot_count END,
					current_status = ?,
					current_image = ?,
					progress_pct = ?
				WHERE mac = ?
			`,
				d.IP, nodeTag,
				d.Manufacturer, d.Manufacturer, d.Manufacturer,
				d.Model, d.Model, d.Model,
				d.Serial, d.Serial, d.Serial,
				d.UUID, d.UUID, d.UUID,
				d.Arch, d.Arch, d.Arch,
				d.AssetTag, d.AssetTag, d.AssetTag,
				d.BIOSVersion, d.BIOSVersion, d.BIOSVersion,
				d.NICChip, d.NICChip, d.NICChip,
				d.LastSeen,
				d.BootCount, d.BootCount,
				d.CurrentStatus, d.CurrentImage, d.ProgressPct,
				mac,
			)
			if uErr == nil {
				mergedDevices++
			}
		}
	}

	prefix := fmt.Sprintf("[%s] ", remoteNodeName)
	for _, l := range remoteLogs {
		msg := l.Message
		if !strings.HasPrefix(msg, "[") {
			msg = prefix + msg
		}
		var exists int
		_ = dbConn.QueryRow(`SELECT COUNT(*) FROM logs WHERE created_at = ? AND device_mac = ? AND message = ?`, l.CreatedAt, l.DeviceMAC, msg).Scan(&exists)
		if exists == 0 {
			_, lErr := dbConn.Exec(`
				INSERT INTO logs (created_at, level, category, device_mac, client_ip, message)
				VALUES (?, ?, ?, ?, ?, ?)
			`, l.CreatedAt, l.Level, l.Category, l.DeviceMAC, l.ClientIP, msg)
			if lErr == nil {
				mergedLogs++
			}
		}
	}

	return mergedDevices, mergedLogs
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
