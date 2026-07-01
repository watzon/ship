package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	command := "server"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	switch command {
	case "server":
		runServer()
	case "worker":
		runWorker()
	case "healthcheck":
		if err := runHealthcheck(); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown command %q", command)
	}
}

func runServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "ship sample app\naccessory=%s\n", accessoryStatus())
	})
	mux.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	addr := ":" + envDefault("PORT", "3000")
	log.Printf("sample app listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func runWorker() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	log.Printf("sample worker started accessory=%s", accessoryStatus())
	for tick := range ticker.C {
		log.Printf("sample worker heartbeat at %s", tick.UTC().Format(time.RFC3339))
	}
}

func runHealthcheck() error {
	url := envDefault("SHIP_HEALTH_URL", "http://127.0.0.1:3000/up")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("healthcheck returned %s", resp.Status)
	}
	return nil
}

func accessoryStatus() string {
	if os.Getenv("DATABASE_URL") == "" {
		return "not-configured"
	}
	return "configured"
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
