package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var (
	initialAdminUsername = "admin"
	initialAdminPassword = "admin123"
)

// EnsureDirectories creates the required directory structure for TFTP, WinPE boot, OS WIMs, and Linux images.
func EnsureDirectories(cfg ServerConfig) error {
	dirs := []string{
		cfg.TFTPDir,
		filepath.Join(cfg.HTTPDir, "images", "winpe"),
		filepath.Join(cfg.HTTPDir, "images", "os"),
		filepath.Join(cfg.HTTPDir, "images", "linux"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Ensure a README exists in the OS WIMs directory
	osReadme := filepath.Join(cfg.HTTPDir, "images", "os", "README.txt")
	if _, err := os.Stat(osReadme); os.IsNotExist(err) {
		content := "Drop your configured Windows deployment WIM images (e.g., Dell-Win11.wim, Lenovo-Win11.wim) in this folder,\n" +
			"or upload and manage them from the Web Management UI -> Image Management -> OS Deployment WIMs.\n"
		_ = os.WriteFile(osReadme, []byte(content), 0644)
	}

	// Ensure a README placeholder exists in the Linux images directory
	linuxReadme := filepath.Join(cfg.HTTPDir, "images", "linux", "README.txt")
	if _, err := os.Stat(linuxReadme); os.IsNotExist(err) {
		content := "Place Linux network boot files (vmlinuz and initrd.img) in this directory,\n" +
			"or upload them directly via the Ruby Web Management UI.\n"
		_ = os.WriteFile(linuxReadme, []byte(content), 0644)
	}
	return nil
}

// RunSetupWizard runs an interactive CLI setup walkthrough when running for the first time
// or when launched with the --setup flag.
func RunSetupWizard(existing ServerConfig) (ServerConfig, error) {
	reader := bufio.NewReader(os.Stdin)
	cfg := existing

	fmt.Println("=============================================================")
	fmt.Println("         LETS GO IMAGING - FIRST-TIME SETUP WIZARD           ")
	fmt.Println("=============================================================")
	fmt.Println("This wizard will configure your iPXE Server IP, ports, initial")
	fmt.Println("Web UI admin credentials, and AlmaLinux 10 startup services.")
	fmt.Println()

	// Step 0: Node Name (useful for Office vs Warehouse multi-node sync)
	fmt.Printf("0) Node / Site Name (e.g. Base-Office, Warehouse-1) [%s]: ", cfg.NodeName)
	if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
		cfg.NodeName = strings.TrimSpace(input)
	}

	// Step 1: Network Interface & IP Address Selection
	ifaces := ListNetworkInterfaces()
	fmt.Println("\n1) Select the Server IPv4 Address (used by iPXE clients):")
	if len(ifaces) > 0 {
		for idx, iface := range ifaces {
			fmt.Printf("   [%d] %s (%s - MAC: %s)\n", idx+1, iface.IP, iface.Name, iface.MAC)
		}
		fmt.Printf("   [C] Enter a custom IPv4 address (default: %s)\n", cfg.ServerIP)
		fmt.Print("Choose an option [1]: ")
		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)
		if choice == "" {
			cfg.ServerIP = ifaces[0].IP
		} else if strings.EqualFold(choice, "c") {
			fmt.Printf("Enter Server IPv4 address [%s]: ", cfg.ServerIP)
			customIP, _ := reader.ReadString('\n')
			customIP = strings.TrimSpace(customIP)
			if customIP != "" {
				cfg.ServerIP = customIP
			}
		} else if idx, err := strconv.Atoi(choice); err == nil && idx >= 1 && idx <= len(ifaces) {
			cfg.ServerIP = ifaces[idx-1].IP
		}
	} else {
		fmt.Printf("Enter Server IPv4 address [%s]: ", cfg.ServerIP)
		customIP, _ := reader.ReadString('\n')
		customIP = strings.TrimSpace(customIP)
		if customIP != "" {
			cfg.ServerIP = customIP
		}
	}
	fmt.Printf("   -> Selected Server IP: %s\n\n", cfg.ServerIP)

	// Step 2: Ports
	fmt.Printf("2) TFTP Server Port [%d]: ", cfg.TFTPPort)
	if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
		if p, err := strconv.Atoi(strings.TrimSpace(input)); err == nil && p > 0 && p <= 65535 {
			cfg.TFTPPort = p
		}
	}

	fmt.Printf("3) HTTP Boot & REST API Port [%d]: ", cfg.HTTPPort)
	if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
		if p, err := strconv.Atoi(strings.TrimSpace(input)); err == nil && p > 0 && p <= 65535 {
			cfg.HTTPPort = p
		}
	}

	fmt.Printf("4) Ruby Web Management UI Port [%d]: ", cfg.RubyWebPort)
	if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
		if p, err := strconv.Atoi(strings.TrimSpace(input)); err == nil && p > 0 && p <= 65535 {
			cfg.RubyWebPort = p
		}
	}

	// Step 3: Boot Menu Timeout & Default Windows WIM
	defaultSec := cfg.MenuTimeoutMs / 1000
	if defaultSec <= 0 {
		defaultSec = 10
	}
	fmt.Printf("5) iPXE Boot Menu Timeout in seconds [%d]: ", defaultSec)
	if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
		if sec, err := strconv.Atoi(strings.TrimSpace(input)); err == nil && sec > 0 {
			cfg.MenuTimeoutMs = sec * 1000
		}
	}

	fmt.Printf("6) Default WinPE Boot WIM filename in http_files/images/winpe/ [%s]: ", cfg.DefaultWim)
	if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
		cfg.DefaultWim = strings.TrimSpace(input)
	}

	// Step 4: Initial Web UI Admin Account
	fmt.Println("\n--- Web UI Initial Admin Credentials (Prompts for reset on 1st login) ---")
	fmt.Printf("7) Initial Admin Username [%s]: ", initialAdminUsername)
	if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
		initialAdminUsername = strings.TrimSpace(input)
	}
	fmt.Printf("8) Initial Admin Password [%s]: ", initialAdminPassword)
	if input, _ := reader.ReadString('\n'); strings.TrimSpace(input) != "" {
		initialAdminPassword = strings.TrimSpace(input)
	}

	cfg.SetupCompleted = true

	if err := EnsureDirectories(cfg); err != nil {
		return cfg, err
	}
	if err := SaveConfig(cfg); err != nil {
		return cfg, err
	}

	scriptPath, _ := WriteAlmaLinuxSetupScript(cfg)
	fmt.Printf("\n[OK] Saved configuration to %s and updated %s/boot.ipxe\n", configFilePath, cfg.TFTPDir)
	fmt.Printf("[OK] Node Sync Key (for linking warehouse nodes): %s\n", cfg.SyncSecret)
	if scriptPath != "" {
		fmt.Printf("[OK] Generated AlmaLinux 10 startup & firewall script: %s\n", scriptPath)
	}

	// Step 5: Optional Linux / AlmaLinux 10 startup service & firewall installation
	if runtime.GOOS == "linux" {
		fmt.Print("\n9) Would you like to install & enable the systemd startup services and firewalld rules now? [y/N]: ")
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(strings.ToLower(ans))
		if ans == "y" || ans == "yes" {
			if err := ApplyLinuxStartupAndFirewall(cfg); err != nil {
				fmt.Printf("   [WARN] Could not automatically apply systemd/firewalld (try running as root or run ./%s): %v\n", scriptPath, err)
			} else {
				fmt.Println("   [OK] Installed and enabled systemd services and firewalld ports!")
			}
		}
	}

	fmt.Println("=============================================================")
	fmt.Printf(" Setup Complete! Go Boot/API Server: http://%s:%d\n", cfg.ServerIP, cfg.HTTPPort)
	fmt.Printf(" Ruby Web Management UI:             http://%s:%d\n", cfg.ServerIP, cfg.RubyWebPort)
	fmt.Printf(" Initial Web Login:                  %s / %s\n", initialAdminUsername, initialAdminPassword)
	fmt.Println("=============================================================")
	fmt.Println()

	return cfg, nil
}

// GenerateSystemdUnits returns the systemd unit contents for the Go server and Ruby Web UI.
func GenerateSystemdUnits(cfg ServerConfig) (goService string, rubyService string) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "/opt/Lets_Go_Imaging/ipxe-server"
	}
	webDir := filepath.Clean(filepath.Join(cwd, "..", "web-ui"))
	binPath := filepath.Join(cwd, "ipxe-server")

	goService = fmt.Sprintf(`[Unit]
Description=Lets Go Imaging - Go iPXE TFTP/HTTP Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s --non-interactive
Restart=on-failure
RestartSec=3
User=root
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
`, cwd, binPath)

	rubyService = fmt.Sprintf(`[Unit]
Description=Lets Go Imaging - Ruby Sinatra Web Management Portal
After=network-online.target lets-go-imaging.service
Wants=lets-go-imaging.service

[Service]
Type=simple
WorkingDirectory=%s
Environment="GO_API_URL=http://127.0.0.1:%d"
Environment="PORT=%d"
ExecStart=/usr/bin/env bundle exec ruby app.rb -o 0.0.0.0 -p %d
Restart=on-failure
RestartSec=3
User=root

[Install]
WantedBy=multi-user.target
`, webDir, cfg.HTTPPort, cfg.RubyWebPort, cfg.RubyWebPort)

	return goService, rubyService
}

// WriteAlmaLinuxSetupScript generates an executable shell script for AlmaLinux 10 to install
// systemd services and configure firewalld.
func WriteAlmaLinuxSetupScript(cfg ServerConfig) (string, error) {
	goUnit, rubyUnit := GenerateSystemdUnits(cfg)

	script := fmt.Sprintf(`#!/usr/bin/env bash
# Auto-generated AlmaLinux 10 / RHEL Setup Script for Lets Go Imaging
set -e

echo "=== Configuring Lets Go Imaging on AlmaLinux 10 ==="

cat << 'EOF' > /etc/systemd/system/lets-go-imaging.service
%sEOF

cat << 'EOF' > /etc/systemd/system/lets-go-imaging-web.service
%sEOF

systemctl daemon-reload
systemctl enable lets-go-imaging.service
systemctl enable lets-go-imaging-web.service

if command -v firewall-cmd >/dev/null 2>&1; then
    echo "Configuring firewalld ports (UDP %d for TFTP, TCP %d for HTTP Boot, TCP %d for Web UI)..."
    firewall-cmd --permanent --add-port=%d/udp
    firewall-cmd --permanent --add-port=%d/tcp
    firewall-cmd --permanent --add-port=%d/tcp
    firewall-cmd --reload
fi

echo "Done! Start services with:"
echo "  sudo systemctl start lets-go-imaging lets-go-imaging-web"
`, goUnit, rubyUnit, cfg.TFTPPort, cfg.HTTPPort, cfg.RubyWebPort, cfg.TFTPPort, cfg.HTTPPort, cfg.RubyWebPort)

	outPath := "setup-almalinux.sh"
	if err := os.WriteFile(outPath, []byte(script), 0755); err != nil {
		return "", err
	}
	return outPath, nil
}

// ApplyLinuxStartupAndFirewall executes the generated setup script on Linux.
func ApplyLinuxStartupAndFirewall(cfg ServerConfig) error {
	scriptPath, err := WriteAlmaLinuxSetupScript(cfg)
	if err != nil {
		return err
	}
	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
