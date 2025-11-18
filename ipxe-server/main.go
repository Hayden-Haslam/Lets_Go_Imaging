package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lpar/gzipped" // File Server
	"github.com/pin/tftp"     // TFTP server
)

// TFTP Server
// tftpReadHandler is called when a client requests a file
func tftpReadHandler(filename string, rf io.ReaderFrom) error {
	// Securely join the path and prevent directory traversal
	// e.g., prevents a request for "../../../../etc/passwd"
	filePath := filepath.Join("tftp_files", filepath.Clean("/"+filename))

	// Open the requested file
	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("[TFTP] ERROR opening file %s: %v", filePath, err)
		return err
	}
	defer file.Close()

	// Get file stats to log the size
	stat, err := file.Stat()
	if err != nil {
		log.Printf("[TFTP] ERROR getting stats for %s: %v", filePath, err)
		return err
	}

	log.Printf("[TFTP] Serving file %s (%d bytes)", filePath, stat.Size())

	// Send the file's content to the client
	n, err := rf.ReadFrom(file)
	if err != nil {
		log.Printf("[TFTP] ERROR sending file %s: %v", filePath, err)
		return err
	}

	log.Printf("[TFTP] Sent %d bytes of %s", n, filePath)
	return nil

}

// Start TFTP server in a goroutine
func startTFTPServer() {
	log.Println("Starting TFTP server on :69...")
	s := tftp.NewServer(tftpReadHandler, nil)

	// Set a timeout
	s.SetTimeout(5 * time.Second)

	// Start the server
	err := s.ListenAndServe(":69") // This will listen on all interfaces, port 69
	if err != nil {
		log.Fatalf("TFTP server failed to start: %v", err)
	}
}

// HTTP Server

// loggingMiddleware logs the incoming HTTP request
func logHTTPRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] Request from %s for %s", r.RemoteAddr, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// Start the HTTP Server
func startHTTPServer() {
	log.Println("Starting HTTP server on :80...")

	// Use the gzipped file server to serve files from the 'http_files' directory
	// This will handle iPXE scripts, kernels, WIMs, etc.
	handler := logHTTPRequest(gzipped.FileServer(http.Dir("http_files")))

	// Start the Server
	err := http.ListenAndServe(":80", handler) // Listen on all interfaces, port 80
	if err != nil {
		log.Fatalf("HTTP server failed to start: %v", err)
	}
}

func main() {
	log.Println("--- Starting iPXE Boot Server ---")

	// Start the TFTP server in its own goroutine
	go startTFTPServer()

	// Start the HTTP server in its own goroutine
	go startHTTPServer()

	// Keep the function alive forever
	// Add web ui logic here later
	select {}
}
