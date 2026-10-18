package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

const configFilePath = "config.json"

// ServerConfig stores persistent server settings configured via the CLI wizard or Web UI.
type ServerConfig struct {
	ServerIP                 string `json:"server_ip"`
	NodeName                 string `json:"node_name"`
	TFTPPort                 int    `json:"tftp_port"`
	HTTPPort                 int    `json:"http_port"`
	RubyWebPort              int    `json:"ruby_web_port"`
	DefaultWim               string `json:"default_wim"`
	LinuxKernelArgs          string `json:"linux_kernel_args"`
	MenuTimeoutMs            int    `json:"menu_timeout_ms"`
	AutoLaunchWinPEInstaller bool   `json:"auto_launch_winpe_installer"`
	SyncSecret               string `json:"sync_secret"`
	SyncMaxMBps              int    `json:"sync_max_mbps"`           // 0 = unlimited, >0 limits WAN sync speed in MB/s
	SyncSchedule             string `json:"sync_schedule"`          // ""/"disabled", "realtime", or "HH:MM" (e.g. "02:00")
	SyncRulesWithImages      bool   `json:"sync_rules_with_images"` // Replicate BIOS Auto-WIM rules to warehouse nodes
	TFTPDir                  string `json:"tftp_dir"`
	HTTPDir                  string `json:"http_dir"`
	DBPath                   string `json:"db_path"`
	SetupCompleted           bool   `json:"setup_completed"`
}

var (
	configMu  sync.RWMutex
	appConfig ServerConfig
)

func generateRandomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "lgi-default-sync-key-change-me"
	}
	return hex.EncodeToString(b)
}

// DefaultConfig returns sensible defaults for Lets_Go_Imaging.
func DefaultConfig() ServerConfig {
	detectedIP := detectPrimaryIPv4()
	if detectedIP == "" {
		detectedIP = "192.168.1.10"
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "Primary-Imaging-Node"
	}
	return ServerConfig{
		ServerIP:                 detectedIP,
		NodeName:                 hostname,
		TFTPPort:                 69,
		HTTPPort:                 80,
		RubyWebPort:              4567,
		DefaultWim:               "boot.wim",
		LinuxKernelArgs:          "initrd=initrd.img root=/dev/ram0 ip=dhcp",
		MenuTimeoutMs:            10000,
		AutoLaunchWinPEInstaller: false,
		SyncSecret:               generateRandomHex(16),
		SyncMaxMBps:              0,
		SyncSchedule:             "02:00",
		SyncRulesWithImages:      true,
		TFTPDir:                  "tftp_files",
		HTTPDir:                  "http_files",
		DBPath:                   "imaging.db",
		SetupCompleted:           false,
	}
}

// GetConfig returns a thread-safe copy of the current configuration.
func GetConfig() ServerConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return appConfig
}

// LoadConfig reads config.json from disk, returning (config, exists, error).
func LoadConfig() (ServerConfig, bool, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			configMu.Lock()
			appConfig = cfg
			configMu.Unlock()
			return cfg, false, nil
		}
		return cfg, false, err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, true, err
	}

	if cfg.NodeName == "" {
		cfg.NodeName = "Primary-Imaging-Node"
	}
	if cfg.TFTPDir == "" {
		cfg.TFTPDir = "tftp_files"
	}
	if cfg.HTTPDir == "" {
		cfg.HTTPDir = "http_files"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "imaging.db"
	}
	if cfg.DefaultWim == "" {
		cfg.DefaultWim = "boot.wim"
	}
	if cfg.MenuTimeoutMs <= 0 {
		cfg.MenuTimeoutMs = 10000
	}
	if cfg.SyncSecret == "" {
		cfg.SyncSecret = generateRandomHex(16)
	}

	configMu.Lock()
	appConfig = cfg
	configMu.Unlock()

	return cfg, true, nil
}

// SaveConfig persists the configuration to config.json and updates tftp_files/boot.ipxe.
func SaveConfig(cfg ServerConfig) error {
	configMu.Lock()
	appConfig = cfg
	configMu.Unlock()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(configFilePath, data, 0644); err != nil {
		return err
	}

	return syncTFTPBootScript(cfg)
}

// syncTFTPBootScript writes tftp_files/boot.ipxe so TFTP clients immediately chainload
// the dynamic HTTP menu while passing SMBIOS hardware identifiers (MAC, IP, Manufacturer, Model, Serial, UUID, Arch, Asset Tag, BIOS Version, NIC Chip).
func syncTFTPBootScript(cfg ServerConfig) error {
	if err := os.MkdirAll(cfg.TFTPDir, 0755); err != nil {
		return err
	}

	portPart := ""
	if cfg.HTTPPort != 80 && cfg.HTTPPort > 0 {
		portPart = fmt.Sprintf(":%d", cfg.HTTPPort)
	}

	script := fmt.Sprintf(`#!ipxe

# Resolve boot server address (prefers DHCP next-server, falls back to configured IP %s)
isset ${next-server} || set next-server %s
set boot-url http://${next-server}%s

echo ==========================================================
echo  Lets Go Imaging - Collecting Hardware Info & Connecting...
echo  MAC: ${net0/mac} | IP: ${ip}
echo  Model: ${manufacturer} ${product}
echo ==========================================================

chain ${boot-url}/boot/menu.ipxe?mac=${net0/mac}&ip=${ip}&mfg=${manufacturer:uristring}&product=${product:uristring}&serial=${serial:uristring}&uuid=${uuid}&arch=${buildarch:uristring}&asset=${asset:uristring}&bios=${smbios/0.2.0:uristring}&chip=${chip:uristring} || goto fallback
`, cfg.ServerIP, cfg.ServerIP, portPart)

	script += fmt.Sprintf(`
:fallback
echo Dynamic HTTP menu unreachable on ${boot-url}, falling back to local static menu...
menu Lets Go Imaging (Fallback Mode)
item exit  Exit to Local Disk Boot (Default)
item winpe Boot Windows Installer (WinPE)
item linux Boot Linux Installer
item shell Drop to iPXE Shell
choose --default exit --timeout %d target && goto ${target}

:winpe
chain tftp://${next-server}/winpe.ipxe || goto fallback

:linux
chain tftp://${next-server}/linux.ipxe || goto fallback

:shell
shell

:exit
exit
`, cfg.MenuTimeoutMs)

	bootIpxePath := filepath.Join(cfg.TFTPDir, "boot.ipxe")
	return os.WriteFile(bootIpxePath, []byte(script), 0644)
}

// NetworkInterfaceOption represents a detected local network interface and IPv4 address.
type NetworkInterfaceOption struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
	MAC  string `json:"mac"`
}

// ListNetworkInterfaces returns all non-loopback IPv4 interfaces on the host.
func ListNetworkInterfaces() []NetworkInterfaceOption {
	var options []NetworkInterfaceOption
	ifaces, err := net.Interfaces()
	if err != nil {
		return options
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ipv4 := ip.To4()
			if ipv4 == nil {
				continue
			}
			options = append(options, NetworkInterfaceOption{
				Name: iface.Name,
				IP:   ipv4.String(),
				MAC:  iface.HardwareAddr.String(),
			})
		}
	}
	return options
}

func detectPrimaryIPv4() string {
	opts := ListNetworkInterfaces()
	if len(opts) > 0 {
		return opts[0].IP
	}
	return ""
}
