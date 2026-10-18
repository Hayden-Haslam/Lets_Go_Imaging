package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lpar/gzipped" // File Server
	"github.com/pin/tftp"     // TFTP server
)

// tftpReadHandler is called when a PXE client requests a file over TFTP.
func tftpReadHandler(filename string, rf io.ReaderFrom) error {
	cfg := GetConfig()
	// Securely join the path and prevent directory traversal
	filePath := filepath.Join(cfg.TFTPDir, filepath.Clean("/"+filename))

	clientIP := ""
	if raddr, ok := rf.(tftp.OutgoingTransfer); ok {
		rnet := raddr.RemoteAddr()
		clientIP = rnet.IP.String()
	}

	file, err := os.Open(filePath)
	if err != nil {
		RecordLog("ERROR", "TFTP", "", clientIP, fmt.Sprintf("Failed to open TFTP file %s: %v", filePath, err))
		return err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		RecordLog("ERROR", "TFTP", "", clientIP, fmt.Sprintf("Failed to stat TFTP file %s: %v", filePath, err))
		return err
	}

	RecordLog("INFO", "TFTP", "", clientIP, fmt.Sprintf("Serving %s (%d bytes)", filepath.Base(filePath), stat.Size()))

	n, err := rf.ReadFrom(file)
	if err != nil {
		RecordLog("ERROR", "TFTP", "", clientIP, fmt.Sprintf("Error sending %s: %v", filepath.Base(filePath), err))
		return err
	}

	RecordLog("INFO", "TFTP", "", clientIP, fmt.Sprintf("Completed sending %s (%d bytes)", filepath.Base(filePath), n))
	return nil
}

// startTFTPServer launches the TFTP server on the configured UDP port.
func startTFTPServer(port int) {
	addr := fmt.Sprintf(":%d", port)
	RecordLog("INFO", "SYSTEM", "", "", fmt.Sprintf("Starting TFTP server on UDP %s", addr))
	s := tftp.NewServer(tftpReadHandler, nil)
	s.SetTimeout(5 * time.Second)

	if err := s.ListenAndServe(addr); err != nil {
		RecordLog("ERROR", "SYSTEM", "", "", fmt.Sprintf("TFTP server failed on %s: %v", addr, err))
	}
}

// startHTTPServer launches the HTTP server serving dynamic iPXE menus, REST APIs, and tracked OS images.
func startHTTPServer(port int) {
	cfg := GetConfig()
	addr := fmt.Sprintf(":%d", port)
	RecordLog("INFO", "SYSTEM", "", "", fmt.Sprintf("Starting HTTP Boot & API server on TCP %s (Server IP: %s)", addr, cfg.ServerIP))

	mux := http.NewServeMux()

	// Dynamic HTTP iPXE Boot Endpoints (collects SMBIOS hardware inventory)
	mux.HandleFunc("/boot/menu.ipxe", handleBootMenu)
	mux.HandleFunc("/boot/winpe.ipxe", handleBootWinPE)
	mux.HandleFunc("/boot/linux.ipxe", handleBootLinux)
	mux.HandleFunc("/boot/winpe/install.bat", handleWinPEInstallBat)

	// Management REST API Endpoints (consumed by Ruby Sinatra Web UI)
	RegisterAPIRoutes(mux)

	// Static & Image File Server with Real-Time Download Progress Tracking
	fileHandler := serveHTTPFilesWithProgress(gzipped.FileServer(http.Dir(cfg.HTTPDir)))
	mux.Handle("/", fileHandler)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("HTTP server failed to start on %s: %v", addr, err)
	}
}

func isInteractiveTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func main() {
	forceSetup := flag.Bool("setup", false, "Run the interactive first-time setup wizard")
	nonInteractive := flag.Bool("non-interactive", false, "Skip interactive CLI prompts and use default/existing config")
	flag.Parse()

	log.Println("--- Starting Lets Go Imaging (Go iPXE & API Server) ---")

	cfg, exists, err := LoadConfig()
	if err != nil {
		log.Printf("[WARN] Error reading config.json, loading defaults: %v", err)
	}

	if *forceSetup || (!exists && !*nonInteractive && isInteractiveTerminal()) {
		cfg, err = RunSetupWizard(cfg)
		if err != nil {
			log.Fatalf("Setup wizard failed: %v", err)
		}
	} else if !exists {
		cfg.SetupCompleted = true
		_ = EnsureDirectories(cfg)
		_ = SaveConfig(cfg)
		_, _ = WriteAlmaLinuxSetupScript(cfg)
		log.Printf("[INIT] Created default config.json (Server IP: %s, HTTP: :%d, TFTP: :%d)", cfg.ServerIP, cfg.HTTPPort, cfg.TFTPPort)
	} else {
		_ = EnsureDirectories(cfg)
		_ = syncTFTPBootScript(cfg)
	}

	if err := InitDB(cfg.DBPath); err != nil {
		log.Fatalf("Failed to initialize SQLite database (%s): %v", cfg.DBPath, err)
	}
	RecordLog("INFO", "SYSTEM", "", "", fmt.Sprintf("Lets Go Imaging initialized (Server IP: %s, Default WIM: %s)", cfg.ServerIP, cfg.DefaultWim))

	go startTFTPServer(cfg.TFTPPort)
	go startHTTPServer(cfg.HTTPPort)
	go StartSyncScheduler()

	select {}
}
