# Lets_Go_Imaging

An iPXE Network Boot & OS Imaging Platform built with a **Go** backend (TFTP + Dynamic HTTP iPXE + Real-Time Progress API + SQLite) and a **Ruby (Sinatra)** Web Management Portal.

## Features
1. **Go Server Backend (`ipxe-server/`)**
   - **TFTP Server (`:69`)**: Serves `ipxe.efi` and `boot.ipxe`, which automatically chains into the dynamic HTTP boot server.
   - **Dynamic HTTP iPXE Menus (`:80`)**:
     - `/boot/menu.ipxe`: Captures client hardware inventory (**MAC Address, IP, Manufacturer, Model/Product, Serial Number, System UUID, CPU Architecture**) directly from iPXE SMBIOS variables and logs them to SQLite.
     - `/boot/winpe.ipxe`: Loads `wimboot`, `bcd`, `boot.sdi`, and your active `.wim` image over HTTP (plus optional auto-launch `winpeshl.ini` & `install.bat` with status webhook reporting).
     - `/boot/linux.ipxe`: Boots `vmlinuz` + `initrd.img` over HTTP, or displays a helpful placeholder screen if Linux boot files have not been uploaded yet.
   - **First-Time Interactive Setup Wizard**:
     - Runs automatically on first terminal launch (or via `./ipxe-server --setup`) to select your network interface IPv4 address, ports, default WIM, and generates `setup-almalinux.sh` (`systemd` startup services + `firewalld` rules for AlmaLinux 10).
   - **Real-Time Image Download Progress Tracking**:
     - Tracks byte-by-byte progress, percentage, and transfer speed (MB/s) whenever a PXE client downloads a `.wim`, `boot.sdi`, `vmlinuz`, or `initrd.img`.

2. **Ruby Sinatra Web Management Portal (`web-ui/`)**
   - **Dashboard & Live Transfers**: Real-time progress bars for active device downloads and quick server readiness indicators.
   - **Image Management**: Upload and delete Windows `.wim` images (`http_files/images/winpe/`) and Linux boot images (`http_files/images/linux/`), and switch the active default `.wim`.
   - **Hardware Inventory & Devices**: View all discovered PXE clients with MAC, IP, Manufacturer, Model, Serial Number, Architecture, boot count, and live imaging status.
   - **Server & PXE Logs**: Filterable SQLite event log viewer (`PXE`, `IMAGING`, `TFTP`, `HTTP`, `IMAGE`, `SETUP`).
   - **Setup & AlmaLinux 10 Startup**: Edit network/boot settings in the browser and view/run the generated `systemd` + `firewalld` configuration.

---

## Quick Start on AlmaLinux 10

```bash
# 1. Install Go and Ruby on AlmaLinux 10
sudo dnf install -y golang ruby ruby-devel gcc make redhat-rpm-config

# 2. Build and run the Go Server (triggers interactive setup wizard on first run)
cd ipxe-server
go build -o ipxe-server .
sudo ./ipxe-server --setup

# 3. Install Ruby Sinatra dependencies and start the Web Management UI
cd ../web-ui
bundle install
bundle exec ruby app.rb
```

Then open **`http://<SERVER-IP>:4567`** in your browser.
