package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	envSmallBid       = "BID_SMALL_AMOUNT"
	envLargeBid       = "BID_LARGE_AMOUNT"
	envMaxConcurrency = "BIDDER_MAX_CONCURRENT_PROOFS"
)

// ConfigPayload matches the JSON from the Succinct dashboard.
type ConfigPayload struct {
	SmallBid       *float64 `json:"small_bid"`
	LargeBid       *float64 `json:"large_bid"`
	MaxConcurrency *int     `json:"max_concurrency"`
}

// LastSeen keeps the last values we've applied.
type LastSeen struct {
	SmallBid          float64
	HasSmallBid       bool
	LargeBid          float64
	HasLargeBid       bool
	MaxConcurrency    int
	HasMaxConcurrency bool
	Initialized       bool
}

func main() {
	endpoint := flag.String("endpoint", "http://localhost:8080/config", "HTTP endpoint to poll")
	interval := flag.Duration("interval", 30*time.Second, "Polling interval (e.g. 30s, 1m)")
	//envPathFlag := flag.String("env", "~/sp1-cluster/infra/.env", "Path to .env file")
	envPathFlag := flag.String("env", "~/Desktop/succinct_clone/infra/.env", "Path to .env file")
	dryRun := flag.Bool("dry-run", false, "If true, don't run systemctl commands (good for local testing)")
	flag.Parse()

	envPath, err := expandPath(*envPathFlag)
	if err != nil {
		log.Fatalf("failed to resolve .env path: %v", err)
	}

	log.Printf("Starting bidder config watcher")
	log.Printf("Endpoint: %s", *endpoint)
	log.Printf("Interval: %s", *interval)
	log.Printf(".env path: %s", envPath)
	if *dryRun {
		log.Printf("Dry-run mode: systemctl commands will NOT be executed")
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	last := &LastSeen{}

	// First immediate poll, then periodic.
	for {
		if err := pollAndUpdate(client, *endpoint, envPath, last, *dryRun); err != nil {
			log.Printf("poll error: %v", err)
		}
		<-ticker.C
	}
}

func expandPath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func pollAndUpdate(client *http.Client, endpoint, envPath string, last *LastSeen, dryRun bool) error {
	resp, err := client.Get(endpoint)
	if err != nil {
		return fmt.Errorf("failed to GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioReadAllLimit(resp.Body, 1024)
		return fmt.Errorf("unexpected status %d from endpoint: %s", resp.StatusCode, string(body))
	}

	var payload ConfigPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("failed to decode JSON: %w", err)
	}

	updates := make(map[string]string)

	// Detect changes vs last-seen values.
	if payload.SmallBid != nil {
		if !last.Initialized || !last.HasSmallBid || last.SmallBid != *payload.SmallBid {
			updates[envSmallBid] = formatFloat(*payload.SmallBid)
		}
	}
	if payload.LargeBid != nil {
		if !last.Initialized || !last.HasLargeBid || last.LargeBid != *payload.LargeBid {
			updates[envLargeBid] = formatFloat(*payload.LargeBid)
		}
	}
	if payload.MaxConcurrency != nil {
		if !last.Initialized || !last.HasMaxConcurrency || last.MaxConcurrency != *payload.MaxConcurrency {
			updates[envMaxConcurrency] = fmt.Sprintf("%d", *payload.MaxConcurrency)
		}
	}

	if len(updates) == 0 {
		log.Printf("No changes detected from endpoint")
		return nil
	}

	log.Printf("Detected changes from endpoint: %+v", updates)

	// Safely update .env (sed-like line replacement, atomic write).
	if err := updateEnvFile(envPath, updates); err != nil {
		return fmt.Errorf("failed to update .env: %w", err)
	}

	// Confirm changes by re-reading the file.
	if err := confirmEnvValues(envPath, updates); err != nil {
		return fmt.Errorf("failed to confirm .env changes: %w", err)
	}

	// Update last-seen cache ONLY after successful write.
	if payload.SmallBid != nil {
		last.SmallBid = *payload.SmallBid
		last.HasSmallBid = true
	}
	if payload.LargeBid != nil {
		last.LargeBid = *payload.LargeBid
		last.HasLargeBid = true
	}
	if payload.MaxConcurrency != nil {
		last.MaxConcurrency = *payload.MaxConcurrency
		last.HasMaxConcurrency = true
	}
	last.Initialized = true

	// Reload systemd and restart bidder.
	if err := reloadAndRestart(dryRun); err != nil {
		log.Printf("WARNING: failed to reload/restart bidder: %v", err)
	} else {
		log.Printf("Successfully reloaded systemd and restarted bidder")
	}

	return nil
}

func formatFloat(f float64) string {
	// Keep enough precision but avoid trailing zeros insanity.
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", f), "0"), ".")
}

// updateEnvFile parses the existing .env, modifies only changed vars (sed-like),
// and rewrites atomically.
func updateEnvFile(path string, updates map[string]string) error {
	var content string

	originalInfo, statErr := os.Stat(path)
	if statErr == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read .env: %w", err)
		}
		content = string(data)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("stat .env: %w", statErr)
	}

	changed := make(map[string]bool, len(updates))
	for key, val := range updates {
		re := regexp.MustCompile(fmt.Sprintf(`(?m)^(export\s+)?%s=.*$`, regexp.QuoteMeta(key)))
		replaced := false
		content = re.ReplaceAllStringFunc(content, func(line string) string {
			matches := re.FindStringSubmatch(line)
			exportPrefix := matches[1]
			replaced = true
			return fmt.Sprintf("%s%s=%s", exportPrefix, key, val)
		})
		changed[key] = replaced
	}

	var builder strings.Builder
	builder.Grow(len(content) + 64)
	builder.WriteString(content)
	hasTrailingNewline := strings.HasSuffix(content, "\n")

	for key, val := range updates {
		if changed[key] {
			continue
		}
		if builder.Len() > 0 && !hasTrailingNewline {
			builder.WriteByte('\n')
			hasTrailingNewline = true
		}
		builder.WriteString(fmt.Sprintf("%s=%s\n", key, val))
		hasTrailingNewline = true
	}

	content = builder.String()
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	// Atomic write: write to temp in same dir, chmod, fsync, rename.
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	tmpFile, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp .env: %w", err)
	}
	tmpName := tmpFile.Name()
	defer func() {
		tmpFile.Close()
		os.Remove(tmpName)
	}()

	// Preserve permissions if original existed.
	if statErr == nil {
		if err := os.Chmod(tmpName, originalInfo.Mode().Perm()); err != nil {
			return fmt.Errorf("chmod temp: %w", err)
		}
	}

	if _, err := tmpFile.WriteString(content); err != nil {
		return fmt.Errorf("write temp .env: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("sync temp .env: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp .env: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp -> .env: %w", err)
	}

	log.Printf("Updated .env at %s with keys: %v", path, keysOf(updates))
	return nil
}

func confirmEnvValues(path string, expected map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("confirm read: %w", err)
	}

	found := parseEnvToMap(data)
	for k, expectedVal := range expected {
		actual, ok := found[k]
		if !ok {
			return fmt.Errorf("confirm: key %s not found in .env", k)
		}
		if actual != expectedVal {
			return fmt.Errorf("confirm: key %s value mismatch (got %q, expected %q)", k, actual, expectedVal)
		}
	}
	log.Printf("Confirmed .env values after update: %+v", expected)
	return nil
}

func parseEnvToMap(data []byte) map[string]string {
	result := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "export ") {
			trimmed = strings.TrimSpace(trimmed[len("export "):])
		}
		idx := strings.Index(trimmed, "=")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		val := trimmed[idx+1:]
		result[key] = val
	}
	return result
}

func reloadAndRestart(dryRun bool) error {
	if dryRun {
		log.Printf("[dry-run] Would run: sudo systemctl daemon-reload")
		log.Printf("[dry-run] Would run: sudo systemctl restart bidder")
		return nil
	}

	commands := [][]string{
		{"sudo", "systemctl", "daemon-reload"},
		{"sudo", "systemctl", "restart", "bidder"},
	}

	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...)
		out, err := cmd.CombinedOutput()
		log.Printf("Ran %v, output:\n%s", args, string(out))
		if err != nil {
			return fmt.Errorf("command %v failed: %w", args, err)
		}
	}

	return nil
}

func keysOf(m map[string]string) []string {
	r := make([]string, 0, len(m))
	for k := range m {
		r = append(r, k)
	}
	return r
}

// ioReadAllLimit reads at most n bytes.
func ioReadAllLimit(r io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, n))
}
