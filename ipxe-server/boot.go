package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// handleBootMenu serves /boot/menu.ipxe dynamically over HTTP, records full SMBIOS hardware info,
// executes one-time queued boot tasks, and defaults timeout to 'exit' (Local Disk) to prevent boot loops.
func handleBootMenu(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	q := r.URL.Query()

	mac := parseIPXEMAC(q.Get("mac"))
	ip := parseIPXEIP(q.Get("ip"), r.RemoteAddr)
	mfg := cleanField(q.Get("mfg"))
	model := cleanField(q.Get("product"))
	serial := cleanField(q.Get("serial"))
	uuid := parseIPXEUUID(q.Get("uuid"))
	arch := cleanField(q.Get("arch"))
	asset := cleanField(q.Get("asset"))
	bios := cleanField(q.Get("bios"))
	chip := cleanField(q.Get("chip"))

	UpsertDevice(mac, ip, mfg, model, serial, uuid, arch, asset, bios, chip, "At iPXE Boot Menu", "")

	targetOSWim, matchSource := ResolveTargetOSWim(mac, mfg, model)
	matchNote := "None (Manual Prompt in WinPE)"
	if targetOSWim != "" {
		matchNote = fmt.Sprintf("%s [%s]", targetOSWim, matchSource)
	}

	RecordLog("INFO", "PXE", mac, ip, fmt.Sprintf("Device booted to iPXE: %s %s (Serial: %s, BIOS: %s, NIC: %s, Matched WIM: %s)", mfg, model, serial, bios, chip, matchNote))

	baseURL := serverBaseURL(cfg, r)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	// Check if a one-time Next Boot Action was queued for this device in the Web UI
	queuedAction, assignedWim := ConsumeDeviceNextBootAction(mac)
	if strings.HasPrefix(queuedAction, "os:") {
		osWim := strings.TrimPrefix(queuedAction, "os:")
		_ = SetDeviceQueue(mac, "menu", osWim)
		RecordLog("INFO", "IMAGING", mac, ip, fmt.Sprintf("Executing one-time queued OS deployment (%s) and resetting next boot to local disk", osWim))
		autoScript := fmt.Sprintf("#!ipxe\necho [QUEUED TASK] Launching WinPE for OS Image: %s...\nchain %s/boot/winpe.ipxe?mac=%s&wim=%s&os_wim=%s\n", osWim, baseURL, mac, cfg.DefaultWim, osWim)
		_, _ = w.Write([]byte(autoScript))
		return
	} else if queuedAction == "winpe" {
		RecordLog("INFO", "IMAGING", mac, ip, "Executing one-time queued WinPE boot and resetting next boot to local disk")
		autoScript := fmt.Sprintf("#!ipxe\necho [QUEUED TASK] Launching Windows Preinstallation Environment...\nchain %s/boot/winpe.ipxe?mac=%s&wim=%s&os_wim=%s\n", baseURL, mac, cfg.DefaultWim, assignedWim)
		_, _ = w.Write([]byte(autoScript))
		return
	} else if queuedAction == "linux" {
		RecordLog("INFO", "IMAGING", mac, ip, "Executing one-time queued Linux boot and resetting next boot to local disk")
		autoScript := fmt.Sprintf("#!ipxe\necho [QUEUED TASK] Launching Linux Installer...\nchain %s/boot/linux.ipxe?mac=%s\n", baseURL, mac)
		_, _ = w.Write([]byte(autoScript))
		return
	} else if queuedAction == "exit" {
		RecordLog("INFO", "PXE", mac, ip, "Device configured to skip PXE menu and boot to local disk")
		_, _ = w.Write([]byte("#!ipxe\necho Exiting to local disk boot...\nexit\n"))
		return
	}

	wimFiles := listFilesWithExt(filepath.Join(cfg.HTTPDir, "images", "winpe"), ".wim")
	var extraWimMenuItems strings.Builder
	var extraWimTargets strings.Builder
	for idx, wim := range wimFiles {
		if strings.EqualFold(wim, cfg.DefaultWim) {
			continue
		}
		itemKey := fmt.Sprintf("winpe_%d", idx)
		extraWimMenuItems.WriteString(fmt.Sprintf("item %-10s Boot Windows WinPE (%s)\n", itemKey, wim))
		extraWimTargets.WriteString(fmt.Sprintf(":%s\nchain %s/boot/winpe.ipxe?mac=%s&wim=%s || goto MENU\n\n", itemKey, baseURL, mac, wim))
	}

	// Keep header & gap lines well under 78 characters so 80x25 BIOS/UEFI consoles never truncate
	hostLine := fmt.Sprintf("Device: %s %s (%s)", mfg, model, mac)
	if len(hostLine) > 74 {
		hostLine = hostLine[:74]
	}
	ruleLine := fmt.Sprintf("Auto-Matched OS WIM: %s", matchNote)
	if len(ruleLine) > 74 {
		ruleLine = ruleLine[:74]
	}

	script := fmt.Sprintf(`#!ipxe

:MENU
menu Lets Go Imaging - Network Boot Menu (%s)
item --gap -- %s
item --gap -- %s
item --gap -- ---------------- Operating System Deployment ----------------
item exit       Continue Local Disk Boot (Default on Timeout)
item winpe      Boot Windows Environment (WinPE - %s)
%sitem linux      Boot Linux Installer / Live Environment
item --gap -- ---------------- Diagnostics & Utilities --------------------
item hwinfo     View Detected Device Hardware Info
item shell      Drop to iPXE Command Shell

choose --default exit --timeout %d target && goto ${target} || goto exit

:winpe
echo Booting Windows Preinstallation Environment (%s)...
chain %s/boot/winpe.ipxe?mac=%s&wim=%s || goto MENU

%s:linux
echo Booting Linux Installer Environment...
chain %s/boot/linux.ipxe?mac=%s || goto MENU

:hwinfo
echo ==========================================================
echo  Detected Hardware Inventory (Logged to Imaging Server)
echo ==========================================================
echo  MAC Address  : %s
echo  IPv4 Address : %s
echo  Manufacturer : %s
echo  Model/Product: %s
echo  Serial Number: %s
echo  Asset Tag    : %s
echo  BIOS Version : %s
echo  NIC Chipset  : %s
echo  System UUID  : %s
echo  Architecture : %s
echo  Matched WIM  : %s
echo ==========================================================
prompt Press any key to return to the boot menu...
goto MENU

:shell
shell || goto MENU
goto MENU

:exit
echo Continuing to local disk boot...
exit
`, ip, hostLine, ruleLine, cfg.DefaultWim, extraWimMenuItems.String(), cfg.MenuTimeoutMs, cfg.DefaultWim, baseURL, mac, cfg.DefaultWim, extraWimTargets.String(), baseURL, mac, mac, ip, mfg, model, serial, asset, bios, chip, uuid, arch, matchNote)

	_, _ = w.Write([]byte(script))
}

// handleBootWinPE serves /boot/winpe.ipxe dynamically for loading wimboot + WinPE files over HTTP.
func handleBootWinPE(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	mac := parseIPXEMAC(r.URL.Query().Get("mac"))
	ip := cleanClientIP(r.RemoteAddr)
	wimName := filepath.Base(r.URL.Query().Get("wim"))
	if wimName == "" || wimName == "." || wimName == "/" {
		wimName = cfg.DefaultWim
	}

	baseURL := serverBaseURL(cfg, r)
	winpeDir := filepath.Join(cfg.HTTPDir, "images", "winpe")
	wimPath := filepath.Join(winpeDir, wimName)
	returnMenuURL := fmt.Sprintf("%s/boot/menu.ipxe?mac=${net0/mac}&ip=${ip}&mfg=${manufacturer:uristring}&product=${product:uristring}&serial=${serial:uristring}&uuid=${uuid}&arch=${buildarch:uristring}&asset=${asset:uristring}&bios=${smbios/0.2.0:uristring}&chip=${chip:uristring}", baseURL)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	if _, err := os.Stat(wimPath); os.IsNotExist(err) {
		RecordLog("WARN", "PXE", mac, ip, fmt.Sprintf("Requested Windows WIM file '%s' is missing in %s", wimName, winpeDir))
		UpdateDeviceStatus(mac, ip, fmt.Sprintf("Missing WIM: %s", wimName), wimName, 0)
		missingScript := fmt.Sprintf(`#!ipxe
echo =====================================================================
echo  [WARNING] Windows WIM Image Not Found: %s
echo =====================================================================
echo  Upload '%s' via the Lets Go Imaging Web UI (http://%s:%d)
echo  or place it inside %s/images/winpe/ on the server.
echo =====================================================================
prompt Press any key to return to the boot menu...
chain %s
`, wimName, wimName, cfg.ServerIP, cfg.RubyWebPort, cfg.HTTPDir, returnMenuURL)
		_, _ = w.Write([]byte(missingScript))
		return
	}

	targetOSWim, matchSource := ResolveTargetOSWim(mac, "", "")
	statusLabel := fmt.Sprintf("Loading WinPE (%s)", wimName)
	if targetOSWim != "" {
		statusLabel = fmt.Sprintf("Loading WinPE -> Target OS: %s (%s)", targetOSWim, matchSource)
	}
	RecordLog("INFO", "IMAGING", mac, ip, statusLabel)
	UpdateDeviceStatus(mac, ip, statusLabel, wimName, 5)

	autoLines := fmt.Sprintf("# Always inject lgi-env.bat so WinPE scripts can inspect auto-matched OS WIM variables\ninitrd %s/boot/winpe/install.bat?mac=%s install.bat\n", baseURL, mac)
	if cfg.AutoLaunchWinPEInstaller {
		autoLines = fmt.Sprintf("initrd %s/images/winpe/winpeshl.ini winpeshl.ini\ninitrd %s/boot/winpe/install.bat?mac=%s install.bat\n", baseURL, baseURL, mac)
	}

	script := fmt.Sprintf(`#!ipxe
set win-url %s/images/winpe

echo
echo Loading Windows wimboot kernel...
kernel ${win-url}/wimboot gui

echo Loading WinPE boot configuration and WIM image (%s)...
%sinitrd ${win-url}/bcd      BCD
initrd ${win-url}/boot.sdi boot.sdi
initrd ${win-url}/%s       boot.wim

echo Starting Windows Preinstallation Environment...
boot || goto failed

:failed
echo [ERROR] WinPE boot failed! Returning to menu in 5 seconds...
sleep 5
chain %s
`, baseURL, wimName, autoLines, wimName, returnMenuURL)

	_, _ = w.Write([]byte(script))
}

// handleBootLinux serves /boot/linux.ipxe dynamically, booting vmlinuz/initrd.img if present
// or presenting a clear placeholder screen if Linux boot artifacts have not been uploaded yet.
func handleBootLinux(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	mac := parseIPXEMAC(r.URL.Query().Get("mac"))
	ip := cleanClientIP(r.RemoteAddr)
	baseURL := serverBaseURL(cfg, r)
	returnMenuURL := fmt.Sprintf("%s/boot/menu.ipxe?mac=${net0/mac}&ip=${ip}&mfg=${manufacturer:uristring}&product=${product:uristring}&serial=${serial:uristring}&uuid=${uuid}&arch=${buildarch:uristring}&asset=${asset:uristring}&bios=${smbios/0.2.0:uristring}&chip=${chip:uristring}", baseURL)

	linuxDir := filepath.Join(cfg.HTTPDir, "images", "linux")
	kernelPath := filepath.Join(linuxDir, "vmlinuz")
	initrdPath := filepath.Join(linuxDir, "initrd.img")

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	_, errKernel := os.Stat(kernelPath)
	_, errInitrd := os.Stat(initrdPath)
	if os.IsNotExist(errKernel) || os.IsNotExist(errInitrd) {
		RecordLog("INFO", "PXE", mac, ip, "Selected Linux Boot (Placeholder mode - vmlinuz/initrd.img not uploaded yet)")
		UpdateDeviceStatus(mac, ip, "Selected Linux Placeholder", "Linux (Placeholder)", 0)

		placeholderScript := fmt.Sprintf(`#!ipxe
echo =====================================================================
echo  Linux Network Installer (Placeholder Mode)
echo =====================================================================
echo  No vmlinuz / initrd.img found in %s/images/linux/ yet.
echo  To enable full Linux network booting, upload 'vmlinuz' and
echo  'initrd.img' in the Web Management Portal:
echo    http://%s:%d -> Images -> Linux Images
echo =====================================================================
prompt Press any key to return to the main boot menu...
chain %s
`, cfg.HTTPDir, cfg.ServerIP, cfg.RubyWebPort, returnMenuURL)
		_, _ = w.Write([]byte(placeholderScript))
		return
	}

	RecordLog("INFO", "IMAGING", mac, ip, "Starting Linux network boot (vmlinuz + initrd.img)")
	UpdateDeviceStatus(mac, ip, "Loading Linux Kernel & Initrd", "Linux (vmlinuz)", 5)

	script := fmt.Sprintf(`#!ipxe
set linux-url %s/images/linux

echo Loading Linux kernel (vmlinuz)...
kernel ${linux-url}/vmlinuz %s

echo Loading Linux initial ramdisk (initrd.img)...
initrd ${linux-url}/initrd.img

echo Booting Linux...
boot || goto failed

:failed
echo [ERROR] Booting Linux failed, returning to menu...
sleep 4
chain %s
`, baseURL, cfg.LinuxKernelArgs, returnMenuURL)

	_, _ = w.Write([]byte(script))
}

// handleWinPEInstallBat generates a dynamic install.bat script injected into WinPE (X:\Windows\System32\install.bat).
// It exports TARGET_OS_WIM and TARGET_OS_WIM_URL based on BIOS Manufacturer/Model rules or device assignment!
func handleWinPEInstallBat(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	mac := parseIPXEMAC(r.URL.Query().Get("mac"))
	baseURL := serverBaseURL(cfg, r)
	targetOSWim, matchSource := ResolveTargetOSWim(mac, "", "")

	autoApply := "0"
	targetURL := ""
	statusMsg := "WinPE Booted - Waiting for Image Selection"
	if targetOSWim != "" {
		autoApply = "1"
		targetURL = fmt.Sprintf("%s/images/os/%s", baseURL, targetOSWim)
		statusMsg = fmt.Sprintf("WinPE Booted - Auto-Matched %s (%s)", targetOSWim, matchSource)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	bat := fmt.Sprintf(`@echo off
SET LGI_SERVER_URL=%s
SET LGI_DEVICE_MAC=%s
SET TARGET_OS_WIM=%s
SET TARGET_OS_WIM_URL=%s
SET TARGET_MATCH_RULE=%s
SET AUTO_APPLY_WIM=%s

echo Initializing Windows Preinstallation Environment (Lets Go Imaging)...
wpeinit
curl.exe -s -X POST "%s/api/devices/status" -H "Content-Type: application/json" -d "{\"mac\":\"%s\",\"status\":\"%s\",\"image\":\"%s\",\"progress_pct\":100}"
echo.
echo ================================================================
echo  Lets Go Imaging - WinPE Environment Ready
echo  Server URL        : %%LGI_SERVER_URL%%
echo  Device MAC        : %%LGI_DEVICE_MAC%%
echo  Auto-Matched WIM  : %%TARGET_OS_WIM%% (%%TARGET_MATCH_RULE%%)
echo  OS WIM HTTP URL   : %%TARGET_OS_WIM_URL%%
echo ================================================================
if "%%AUTO_APPLY_WIM%%"=="1" (
    echo [AUTO-IMAGE] BIOS matched rule "%%TARGET_MATCH_RULE%%".
    echo [AUTO-IMAGE] Your custom WinPE script can pull %%TARGET_OS_WIM_URL%% automatically!
)
cmd.exe
`, baseURL, mac, targetOSWim, targetURL, matchSource, autoApply, baseURL, mac, statusMsg, targetOSWim)
	_, _ = w.Write([]byte(bat))
}

// parseIPXEMAC handles both standard ASCII MAC strings ("00:15:5d:8a:4f:21")
// and raw 6-byte binary MACs if a client used ${net0/mac:uristring}.
func parseIPXEMAC(raw string) string {
	if len(raw) == 6 && !isPrintableASCII(raw) {
		b := []byte(raw)
		return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[0], b[1], b[2], b[3], b[4], b[5])
	}
	cleaned := normalizeMAC(raw)
	if !isPrintableASCII(cleaned) {
		return ""
	}
	return cleaned
}

// parseIPXEIP handles both standard ASCII IPv4 strings and raw 4-byte binary IPv4s,
// falling back to the HTTP request's RemoteAddr if needed.
func parseIPXEIP(raw string, remoteAddr string) string {
	if len(raw) == 4 && !isPrintableASCII(raw) {
		b := []byte(raw)
		return net.IPv4(b[0], b[1], b[2], b[3]).String()
	}
	trimmed := strings.TrimSpace(raw)
	if ip := net.ParseIP(trimmed); ip != nil {
		return ip.String()
	}
	return cleanClientIP(remoteAddr)
}

// parseIPXEUUID handles both standard ASCII UUID strings and raw 16-byte binary UUIDs.
func parseIPXEUUID(raw string) string {
	if len(raw) == 16 && !isPrintableASCII(raw) {
		b := []byte(raw)
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
			b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	cleaned := cleanField(raw)
	if !isPrintableASCII(cleaned) {
		return "Unknown"
	}
	return cleaned
}

func isPrintableASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 32 || c > 126 || !unicode.IsPrint(rune(c)) {
			return false
		}
	}
	return true
}

func serverBaseURL(cfg ServerConfig, r *http.Request) string {
	host := cfg.ServerIP
	if r != nil && r.Host != "" {
		return "http://" + r.Host
	}
	if cfg.HTTPPort != 80 && cfg.HTTPPort > 0 {
		return fmt.Sprintf("http://%s:%d", host, cfg.HTTPPort)
	}
	return fmt.Sprintf("http://%s", host)
}

func listFilesWithExt(dir, ext string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), strings.ToLower(ext)) {
			names = append(names, e.Name())
		}
	}
	return names
}
