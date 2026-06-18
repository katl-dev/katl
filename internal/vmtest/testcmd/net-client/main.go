package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	target := strings.TrimSpace(os.Getenv("KATL_VMTEST_TARGET_URL"))
	if target == "" {
		target = "http://net-server.katl-vmtest.svc.cluster.local:8080/hostname"
	}
	deadline := 90 * time.Second
	if value := strings.TrimSpace(os.Getenv("KATL_VMTEST_DEADLINE")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			log.Fatal(err)
		}
		deadline = parsed
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	client := http.Client{Timeout: time.Second}
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			log.Fatal(err)
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				log.Fatal(readErr)
			}
			fmt.Print(string(body))
			return
		}
		if err == nil {
			lastErr = fmt.Errorf("status %s", resp.Status)
			_ = resp.Body.Close()
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			log.Fatalf("request %s did not succeed within %s: %v", target, deadline, lastErr)
		case <-time.After(time.Second):
		}
	}
}
