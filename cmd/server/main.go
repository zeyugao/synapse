package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
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
	configPath := flag.String("config", "", "Path to grouped authentication YAML or JSON config file")
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
	if *configPath != "" && (*apiAuthKey != "" || *wsAuthKey != "") {
		log.Fatal("--config cannot be combined with --api-auth-key or --ws-auth-key")
	}

	var srv *server.Server
	if *configPath != "" {
		resolvedConfigPath, err := filepath.Abs(*configPath)
		if err != nil {
			log.Fatalf("Failed to resolve config path %q: %v", *configPath, err)
		}
		cfg, err := server.LoadConfig(resolvedConfigPath)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
		srv, err = server.NewServerWithConfig(cfg, version, *clientBinary)
		if err != nil {
			log.Fatalf("Failed to initialize server with config: %v", err)
		}
		log.Printf("Loaded config %q successfully: %d group(s)", resolvedConfigPath, len(cfg.Groups))
		go watchConfigFile(resolvedConfigPath, srv)
	} else {
		srv = server.NewServer(*apiAuthKey, *wsAuthKey, version, *clientBinary)
	}

	log.Printf("Starting server on %s:%s", *host, *port)
	if err := srv.Start(*host, *port); err != nil {
		log.Fatal(err)
	}
}

func watchConfigFile(path string, srv *server.Server) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Failed to start config watcher for %q: %v", path, err)
		return
	}
	defer watcher.Close()

	configDir := filepath.Dir(path)
	configName := filepath.Base(path)
	if err := watcher.Add(configDir); err != nil {
		log.Printf("Failed to watch config directory %q: %v", configDir, err)
		return
	}
	log.Printf("Watching config %q for changes", path)

	var reloadTimer *time.Timer
	var reloadC <-chan time.Time
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != configName {
				continue
			}
			if !isConfigChangeEvent(event) {
				continue
			}
			if reloadTimer == nil {
				reloadTimer = time.NewTimer(200 * time.Millisecond)
				reloadC = reloadTimer.C
				continue
			}
			if !reloadTimer.Stop() {
				select {
				case <-reloadTimer.C:
				default:
				}
			}
			reloadTimer.Reset(200 * time.Millisecond)

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Config watcher error for %q: %v", path, err)

		case <-reloadC:
			reloadTimer = nil
			reloadC = nil
			reloadConfig(path, srv)
		}
	}
}

func isConfigChangeEvent(event fsnotify.Event) bool {
	return event.Has(fsnotify.Write) ||
		event.Has(fsnotify.Create) ||
		event.Has(fsnotify.Rename) ||
		event.Has(fsnotify.Remove) ||
		event.Has(fsnotify.Chmod)
}

func reloadConfig(path string, srv *server.Server) {
	cfg, err := server.LoadConfig(path)
	if err != nil {
		log.Printf("Config reload failed for %q: %v; keeping previous config", path, err)
		return
	}
	if err := srv.ReloadConfig(cfg); err != nil {
		log.Printf("Config reload failed for %q: %v; keeping previous config", path, err)
		return
	}

	log.Printf("Config reload succeeded for %q: %d group(s)", path, len(cfg.Groups))
}
