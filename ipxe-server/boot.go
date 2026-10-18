package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handleBootMenu serves /boot/menu.ipxe dynamically over HTTP and records hardware info.
func handleBootMenu(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	q := r.URL.Query()

	mac := normalizeMAC(q.Get("mac"))
	ip := q.Get("ip")
	if ip == "" {
		ip = cleanClientIP(r.RemoteAddr)
	}
	mfg := cleanField(q.Get("mfg"))
	model := cleanField(q.Get("product"))
	serial := cleanField(q.Get("serial"))
	uuid := cleanField(q.Get("uuid"))
	arch := cleanField(q.Get("arch"))

	UpsertDevice(mac, ip, mfg, model, serial, uuid, arch, "At iPXE Boot Menu", "")
	RecordLog("INFO", "PXE", mac, ip, fmt.Sprintf("Device booted to iPXE menu: %s %s (Serial: %s, Arch: %s)", mfg, model, serial, arch))

	baseURL := serverBaseURL(cfg, r)
	wimFiles := listFilesWithExt(filepath.Join(cfg.HTTPDir, "images", "winpe"), ".wim")

	var extraWimMenuItems strings.Builder
	var extraWimTargets strings.Builder
	for idx, wim := range wimFiles {
		if strings.EqualFold(wim, cfg.DefaultWim) {
			continue
		}
		itemKey := fmt.Sprintf("winpe_%d", idx)
		extraWimMenuItems.WriteString(fmt.Sprintf("item %s   Boot Windows WinPE (%s)\n", itemKey, wim))
		extraWimTargets.WriteString(fmt.Sprintf(":%s\nchain %s/boot/winpe.ipxe?mac=%s&wim=%s || goto MENU\n\n", itemKey, baseURL, mac, wim))
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	script := fmt.Sprintf(`#!ipxe

:MENU
menu Lets Go Imaging - Network Boot Menu [%s %s | MAC: %s]
item --gap -- ---------------- Operating System Deployment ----------------
item winpe    Boot Windows Environment (WinPE - %s)
%sitem linux    Boot Linux Installer / Live Environment
item --gap -- ---------------- Diagnostics & Utilities --------------------
item hwinfo   View Detected Device Hardware Info
item shell    Drop to iPXE Command Shell
item exit     Exit & Continue Local Disk Boot

choose --default winpe --timeout %d target && goto ${target} || goto MENU

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
echo  System UUID  : %s
echo  Architecture : %s
echo ==========================================================
prompt Press any key to return to the boot menu...
goto MENU

:shell
shell || goto MENU
goto MENU

:exit
exit
`, mfg, model, mac, cfg.DefaultWim, extraWimMenuItems.String(), cfg.MenuTimeoutMs, cfg.DefaultWim, baseURL, mac, cfg.DefaultWim, extraWimTargets.String(), baseURL, mac, mac, ip, mfg, model, serial, uuid, arch)

	_, _ = w.Write([]byte(script))
}

// handleBootWinPE serves /boot/winpe.ipxe dynamically for loading wimboot + WinPE files over HTTP.
func handleBootWinPE(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	mac := normalizeMAC(r.URL.Query().Get("mac"))
	ip := cleanClientIP(r.RemoteAddr)
	wimName := filepath.Base(r.URL.Query().Get("wim"))
	if wimName == "" || wimName == "." || wimName == "/" {
		wimName = cfg.DefaultWim
	}

	baseURL := serverBaseURL(cfg, r)
	winpeDir := filepath.Join(cfg.HTTPDir, "images", "winpe")
	wimPath := filepath.Join(winpeDir, wimName)

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
chain %s/boot/menu.ipxe?mac=%s
`, wimName, wimName, cfg.ServerIP, cfg.RubyWebPort, cfg.HTTPDir, baseURL, mac)
		_, _ = w.Write([]byte(missingScript))
		return
	}

	RecordLog("INFO", "IMAGING", mac, ip, fmt.Sprintf("Starting WinPE boot with image '%s'", wimName))
	UpdateDeviceStatus(mac, ip, fmt.Sprintf("Loading WinPE (%s)", wimName), wimName, 5)

	autoLines := "# Auto-launch scripts disabled in settings (standard WinPE boot)\n"
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
chain %s/boot/menu.ipxe?mac=%s
`, baseURL, wimName, autoLines, wimName, baseURL, mac)

	_, _ = w.Write([]byte(script))
}

// handleBootLinux serves /boot/linux.ipxe dynamically, booting vmlinuz/initrd.img if present
// or presenting a clear placeholder screen if Linux boot artifacts have not been uploaded yet.
func handleBootLinux(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	mac := normalizeMAC(r.URL.Query().Get("mac"))
	ip := cleanClientIP(r.RemoteAddr)
	baseURL := serverBaseURL(cfg, r)

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
chain %s/boot/menu.ipxe?mac=%s
`, cfg.HTTPDir, cfg.ServerIP, cfg.RubyWebPort, baseURL, mac)
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
chain %s/boot/menu.ipxe?mac=%s
`, baseURL, cfg.LinuxKernelArgs, baseURL, mac)

	_, _ = w.Write([]byte(script))
}

// handleWinPEInstallBat generates a dynamic install.bat script injected into WinPE that reports
// imaging status back to the Go server over HTTP.
func handleWinPEInstallBat(w http.ResponseWriter, r *http.Request) {
	cfg := GetConfig()
	mac := normalizeMAC(r.URL.Query().Get("mac"))
	baseURL := serverBaseURL(cfg, r)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	bat := fmt.Sprintf(`@echo off
echo Initializing Windows Preinstallation Environment (Lets Go Imaging)...
wpeinit
curl.exe -s -X POST "%s/api/devices/status" -H "Content-Type: application/json" -d "{\"mac\":\"%s\",\"status\":\"WinPE Booted - Ready for Custom Image Deployment\",\"progress_pct\":100}"
echo.
echo ================================================================
echo  Lets Go Imaging - WinPE Environment Ready
echo  Server: %s
echo  Device MAC: %s
echo ================================================================
cmd.exe
`, baseURL, mac, baseURL, mac)
	_, _ = w.Write([]byte(bat))
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
