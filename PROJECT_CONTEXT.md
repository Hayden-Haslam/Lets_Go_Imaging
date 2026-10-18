# Let's Go Imaging — Project Context & Session Handoff

**Last Updated:** 2026-10-18  
**Target OS:** AlmaLinux 10 / RHEL 10 (cross-platform compatible with Windows)  
**Architecture Philosophy:**
- **100% Server-Side Backend in Go (`ipxe-server/`)**: Handles UDP TFTP (`:69`), HTTP Boot & REST API (`:80`), dynamic iPXE script generation, SMBIOS hardware telemetry ingestion, pure-Go SQLite (`modernc.org/sqlite`), RBAC authentication, and multi-site warehouse image + database synchronization.
- **100% Web Management UI in Ruby (`web-ui/`)**: Pure-Ruby Sinatra + WEBrick (`:4567`) web console with public View-Only Wallboard mode and authenticated management tabs.

---

## 1. Repository File Map

### Root
- [`update.sh`](file:///c:/WORKING/Lets_Go_Imaging/update.sh) — Root update script (`./update.sh` via SSH or triggered from the Web UI). Runs `git pull --ff-only`, compiles Go if `go` is installed (or uses the pre-built `ipxe-server/lets-go-imaging` static Linux binary), verifies Ruby gems, and restarts `lets-go-imaging` + `lets-go-imaging-web` systemd services.
- [`PROJECT_CONTEXT.md`](file:///c:/WORKING/Lets_Go_Imaging/PROJECT_CONTEXT.md) — This persistent context document.

### Go Backend (`ipxe-server/`)
- [`main.go`](file:///c:/WORKING/Lets_Go_Imaging/ipxe-server/main.go) — Entrypoint; supports `--setup` interactive wizard, initializes SQLite (`imaging.db`), starts background sync scheduler (`StartSyncScheduler`), and launches TFTP + HTTP servers.
- [`config.go`](file:///c:/WORKING/Lets_Go_Imaging/ipxe-server/config.go) — Manages `config.json`, network interface auto-detection, and auto-syncs `tftp_files/boot.ipxe` whenever `ServerIP` or `HTTPPort` changes.
- [`setup.go`](file:///c:/WORKING/Lets_Go_Imaging/ipxe-server/setup.go) — Interactive first-run CLI wizard (`--setup`) for configuring Node Name, Server IP, ports, default admin credentials, and generating `setup-almalinux.sh` (`systemd` units + `firewalld` rules).
- [`db.go`](file:///c:/WORKING/Lets_Go_Imaging/ipxe-server/db.go) — Pure-Go SQLite (`imaging.db`) managing:
  - `devices`: PXE client SMBIOS inventory (`mac`, `ip`, `node_name`, `manufacturer`, `model`, `serial`, `uuid`, `arch`, `asset_tag`, `bios_version`, `nic_chip`, `next_boot_action`, `assigned_os_wim`, `current_status`, `last_image`, `progress_pct`).
  - `logs`: Categorized event logs (`PXE`, `IMAGING`, `SYNC`, `AUTH`, `TFTP`, `HTTP`, `IMAGE`, `SETUP`, `SYSTEM`).
  - `image_rules`: BIOS Manufacturer/Model $\rightarrow$ Target OS WIM auto-assignment rules.
  - `users`: Bcrypt-hashed RBAC accounts (`admin`, `operator`, `viewer`, `can_manage_images`, `can_manage_settings`, `must_change_password`).
  - `remote_nodes`, `image_sync_flags`, `file_hashes`: Multi-site warehouse node registry, per-image sync flags, and cached SHA-256 file manifests.
- [`boot.go`](file:///c:/WORKING/Lets_Go_Imaging/ipxe-server/boot.go) — Dynamic HTTP iPXE endpoints:
  - `/boot/menu.ipxe`: Collects SMBIOS hardware telemetry, checks one-time next-boot queue (`menu`, `winpe`, `os:<wim>`, `linux`, `exit`), defaults menu timeout to `exit` (Boot to Local Disk) to prevent accidental boot loops, and formats cleanly for 80x25 console width.
  - `/boot/winpe.ipxe`: Boots WinPE via `wimboot` and optionally injects `/boot/winpe/winpeshl.ini` + `/boot/winpe/install.bat`.
  - `/boot/winpe/install.bat`: Dynamic batch script that queries `/api/winpe/assignment?mac=...` to auto-apply the matched OS WIM from `http_files/images/os/`.
  - `/boot/linux.ipxe`: Boots Linux `vmlinuz`/`initrd.img` or displays a diagnostic placeholder screen.
- [`api.go`](file:///c:/WORKING/Lets_Go_Imaging/ipxe-server/api.go) — REST API endpoints for the Ruby Web UI and WinPE clients, including live byte-level HTTP transfer progress tracking (`progressResponseWriter`) and `/api/system/update`.
- [`sync.go`](file:///c:/WORKING/Lets_Go_Imaging/ipxe-server/sync.go) — Multi-site synchronization engine:
  - **Master $\rightarrow$ Warehouse Push**: SHA-256 manifest deduplication (`/api/sync/manifest`), resumable `.sync.part` file transfers (`/api/sync/receive`), WAN bandwidth throttling (`sync_max_mbps`), and BIOS rule replication (`/api/sync/rules`).
  - **Warehouse $\rightarrow$ Master Telemetry Pull**: Pulls remote `devices` and `logs` (`/api/sync/telemetry`) during the daily `02:00` AM scheduled sync (`StartSyncScheduler`) and on-demand via `POST /api/nodes/pull-db`.
- [`lets-go-imaging`](file:///c:/WORKING/Lets_Go_Imaging/ipxe-server/lets-go-imaging) — Pre-built static Linux `amd64` binary (`CGO_ENABLED=0`).

### Ruby Sinatra Web UI (`web-ui/`)
- [`Gemfile`](file:///c:/WORKING/Lets_Go_Imaging/web-ui/Gemfile) — Pure-Ruby dependencies (`sinatra`, `rackup`, `webrick`) — no C compiler (`gcc`/`make`/`ruby-devel`) required on target servers.
- [`app.rb`](file:///c:/WORKING/Lets_Go_Imaging/web-ui/app.rb) — Sinatra web application with session auth, RBAC enforcement, View-Only public mode redaction, multipart image uploads (`images/os/`, `images/winpe/`, `images/linux/`), real-time warehouse DB pull (`/nodes/pull-db`), and one-click app update (`/system/update`).
- [`views/index.erb`](file:///c:/WORKING/Lets_Go_Imaging/web-ui/views/index.erb), [`public/app.js`](file:///c:/WORKING/Lets_Go_Imaging/web-ui/public/app.js), [`public/style.css`](file:///c:/WORKING/Lets_Go_Imaging/web-ui/public/style.css) — Frontend dashboard, wallboard mode, modals, and management tabs.

---

## 2. Future / Parked Discussion Topics
1. **Active Directory / LDAP Authentication for Web UI**:
   - Can be added either in Ruby (`net-ldap`, pure-Ruby gem) or in Go (`go-ldap/ldap/v3`) with AD group $\rightarrow$ role mapping (`Admin Group`, `Operator Group`, `Viewer Group`) while keeping local SQLite admin login as fallback.
2. **ProxyDHCP (`:4011` / `:67`)**:
   - Currently skipped because routers are configured with DHCP Options 66 & 67 (`ipxe.efi`).
3. **Windows Service Port**:
   - Because the Go backend uses pure-Go SQLite (`modernc.org/sqlite`), it already compiles natively on Windows (`lets-go-imaging.exe`) and can be wrapped with `golang.org/x/sys/windows/svc` later if needed.
