// Command healthcheck is the container liveness probe.
//
// The runtime image is distroless: no shell, no curl, no wget. A Docker
// HEALTHCHECK still has to run something, so it runs this — a few kilobytes of
// static binary that makes one HTTP request and turns the answer into an exit
// code. It deliberately has no dependencies beyond the standard library.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	url := os.Getenv("HEALTHCHECK_URL")
	if url == "" {
		url = "http://127.0.0.1:8080/healthz"
	}

	// Shorter than the healthcheck's own timeout, so a hung server is reported
	// as unhealthy by this process rather than killed by Docker mid-request.
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %s\n", url, resp.Status)
		os.Exit(1)
	}
}
