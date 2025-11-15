package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/zeyugao/synapse/internal/client"
)

var (
	version = "dev" // Default value, will be overwritten at compile time
)

func main() {
	// Get defaults from environment variables
	defaultUpstream := os.Getenv("OPENAI_BASE_URL")
	if defaultUpstream == "" {
		defaultUpstream = "http://localhost:8081/v1"
	}

	defaultAPIKey := os.Getenv("OPENAI_API_KEY")

	// Command-line flags
	baseURL := flag.String("base-url", defaultUpstream, "Upstream base URL (can also be set via OPENAI_BASE_URL)")
	apiKey := flag.String("api-key", defaultAPIKey, "Upstream API key (can also be set via OPENAI_API_KEY)")
	serverURL := flag.String("server-url", "ws://localhost:8080/ws", "WebSocket server URL")
	wsAuthKey := flag.String("ws-auth-key", "", "WebSocket authentication key")
	printVersion := flag.Bool("version", false, "Print version number")
	flag.Parse()

	if *printVersion {
		fmt.Printf("Synapse Client Version: %s\n", version)
		return
	}

	client := client.NewClient(*baseURL, *serverURL, version)
	client.WSAuthKey = *wsAuthKey
	client.ApiKey = *apiKey
	defer client.Close()

	log.Printf("Connecting to server at %s", *serverURL)
	log.Printf("Using upstream API at %s", *baseURL)

	// Keep the program running
	for {
		if err := client.Connect(); err != nil {
			log.Printf("Connection failed: %v, retrying in 5 seconds...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		select {} // Block the main thread until the connection is broken
	}
}
