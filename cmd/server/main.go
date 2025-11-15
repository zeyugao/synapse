package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/zeyugao/synapse/internal/server"
)

var (
	version = "dev" // Default value, will be overwritten at compile time
)

func main() {
	// Get the directory where the server binary is located
	execPath, err := os.Executable()
	if err != nil {
		log.Fatalf("Failed to get executable path: %v", err)
	}
	execDir := filepath.Dir(execPath)
	defaultClientPath := filepath.Join(execDir, "client")

	host := flag.String("host", "localhost", "Server host")
	port := flag.String("port", "8080", "Server port")
	apiAuthKey := flag.String("api-auth-key", "", "API authentication key")
	wsAuthKey := flag.String("ws-auth-key", "", "WebSocket registration authentication key")
	printVersion := flag.Bool("version", false, "Print version number")
	clientBinary := flag.String("client-binary", defaultClientPath, "Client binary file path")
	flag.Parse()

	if _, err := os.Stat(*clientBinary); os.IsNotExist(err) {
		log.Printf("Client binary not found at %s, /getclient and /run will not be available.", *clientBinary)
	}

	if *printVersion {
		fmt.Printf("Synapse Server Version: %s\n", version)
		return
	}

	log.Printf("Synapse Server Version: %s", version)
	server := server.NewServer(*apiAuthKey, *wsAuthKey, version, *clientBinary)
	log.Printf("Starting server on %s:%s", *host, *port)
	if err := server.Start(*host, *port); err != nil {
		log.Fatal(err)
	}
}
