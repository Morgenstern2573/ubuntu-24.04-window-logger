package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// --- Configuration ---
const (
	pollInterval   = 5 * time.Second
	webAppEndpoint = "http://localhost:9999/api/v1/log_activity" // Change this to your web app's URL
	//webAppApiKey   = "YOUR_SECRET_API_KEY_HERE"                  // Change this to your secret key
	maxBackoff   = 60 * time.Minute
	localLogFile = "window-logs.json" // The local log file name
	queueSize    = 1000               // Buffer size for network uploads (approx 1.5 hours of data)
)

// LogEntry is the complete data structure
type LogEntry struct {
	Timestamp string  `json:"timestamp"`
	Title     string  `json:"title,omitempty"`
	AppName   string  `json:"appName,omitempty"`
	PID       int     `json:"pid,omitempty"`
	Cmdline   string  `json:"cmdline,omitempty"`
	IdleTimeS float64 `json:"idle_time_s,omitempty"`
	IsSwitch  bool    `json:"is_switch,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// WindowInfo is the JSON structure expected from our D-Bus extension
type WindowInfo struct {
	Title   string `json:"title"`
	Pid     int    `json:"pid"`
	AppName string `json:"appName"`
	Err     string `json:"err"`
}

func main() {
	// FIX 5: Implement -v (verbose) flag
	verbose := flag.Bool("v", false, "Enable verbose stdout logging for each successful poll")
	flag.Parse()

	// FIX 3: Connect to D-Bus once and reuse the connection
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Could not connect to D-Bus: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// --- NEW: Open local log file for appending ---
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Could not get home directory: %v\n", err)
		os.Exit(1)
	}
	logPath := filepath.Join(homeDir, localLogFile)
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Could not open local log file %s: %v\n", logPath, err)
		os.Exit(1)
	}
	defer logFile.Close()

	// --- NEW: Start the background network worker ---
	sendQueue := make(chan []byte, queueSize)
	go logSenderWorker(sendQueue)

	var lastFocusedPID int

	fmt.Println("Starting focus logger agent...")
	fmt.Printf("Polling every %s.\n", pollInterval)
	fmt.Printf("Appending logs to %s\n", logPath)
	fmt.Printf("Pushing data to %s via background worker\n", webAppEndpoint)

	if !*verbose {
		fmt.Println("Run with -v to see verbose logging for each poll.")
	}

	for {
		// 1. Collect all data for this poll
		entry := collectLogEntry(conn, &lastFocusedPID)

		// 2. Marshal the entry to JSON ONCE
		jsonData, jsonErr := json.Marshal(entry)
		if jsonErr != nil {
			fmt.Fprintf(os.Stderr, "Error marshaling log: %v\n", jsonErr)
			time.Sleep(pollInterval)
			continue
		}

		// 3. (Optional) Print to stdout if verbose
		if *verbose {
			fmt.Println(string(jsonData))
		}

		// 4. ALWAYS append to local log file immediately
		fileLine := append(jsonData, '\n') // Add a newline
		if _, writeErr := logFile.Write(fileLine); writeErr != nil {
			fmt.Fprintf(os.Stderr, "Error writing to local log file %s: %v\n", logPath, writeErr)
		}

		// 5. Send to the background worker (Non-blocking)
		select {
		case sendQueue <- jsonData:
			// Success: queued for upload
		default:
			// Failure: queue is full (network is likely down for a long time)
			// We drop this upload packet to ensure the main loop (and disk logging) isn't blocked.
			fmt.Fprintf(os.Stderr, "Network queue full! Dropping upload for this entry (Data saved to disk)\n")
		}

		// 6. Wait for next poll
		time.Sleep(pollInterval)
	}
}

// logSenderWorker handles the HTTP posts and backoff logic independently
func logSenderWorker(queue <-chan []byte) {
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}
	currentBackoff := pollInterval

	for jsonData := range queue {
		// Retry loop for the CURRENT message
		// We do not move to the next message until this one succeeds,
		// ensuring strictly ordered uploads (unless the queue filled up and we dropped packets in main).
		for {
			err := sendActivityLog(httpClient, jsonData)
			if err == nil {
				// Success! Reset backoff and break the retry loop to get next message
				currentBackoff = pollInterval
				break
			}

			// Failure: Apply backoff
			fmt.Fprintf(os.Stderr, "Send failed: %v. Backing off for %s\n", err, currentBackoff)
			time.Sleep(currentBackoff)

			// Increase backoff for next retry
			currentBackoff *= 2
			if currentBackoff > maxBackoff {
				currentBackoff = maxBackoff
			}
		}
	}
}

// collectLogEntry gathers all data (unchanged)
func collectLogEntry(conn *dbus.Conn, lastPID *int) LogEntry {
	entry := LogEntry{Timestamp: time.Now().Format(time.RFC3339)}

	// Get focused window
	title, pid, appName, err := getFocusedViaExtension(conn)
	if err == nil {
		entry.Title = title
		entry.PID = pid
		entry.AppName = appName

		if pid != 0 && pid != *lastPID {
			entry.IsSwitch = true
			*lastPID = pid
		}

		if pid != 0 {
			if cmd, e := readCmdline(pid); e == nil {
				entry.Cmdline = cmd
			}
		}
	} else {
		entry.Error = err.Error()
		*lastPID = 0
	}

	// Get user idle time
	idleTime, idleErr := getIdleTime(conn)
	if idleErr == nil {
		entry.IdleTimeS = idleTime.Seconds()
	} else {
		if entry.Error != "" {
			entry.Error += " | " + idleErr.Error()
		} else {
			entry.Error = idleErr.Error()
		}
	}
	return entry
}

// sendActivityLog performs the HTTP POST
func sendActivityLog(client *http.Client, jsonData []byte) error {
	req, err := http.NewRequest("POST", webAppEndpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("http.NewRequest failed: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	//req.Header.Set("Authorization", "Bearer "+webAppApiKey)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("client.Do failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("server returned non-2xx status: %s", resp.Status)
	}

	return nil
}

// getFocusedViaExtension (unchanged)
func getFocusedViaExtension(conn *dbus.Conn) (title string, pid int, appName string, err error) {
	obj := conn.Object("org.gnome.Shell", "/org/example/WindowLogger")
	call := obj.Call("org.example.WindowLogger.GetFocusedWindow", 0)
	if call.Err != nil {
		return "", 0, "", fmt.Errorf("D-Bus call failed: %w", call.Err)
	}

	if len(call.Body) < 1 {
		return "", 0, "", fmt.Errorf("unexpected D-Bus response: %v", call.Body)
	}
	jsonPayload, ok := call.Body[0].(string)
	if !ok {
		return "", 0, "", fmt.Errorf("D-Bus returned non-string: %T", call.Body[0])
	}

	var info WindowInfo
	if jerr := json.Unmarshal([]byte(jsonPayload), &info); jerr != nil {
		return "", 0, "", fmt.Errorf("json parse failed: %w (payload: %q)", jerr, jsonPayload)
	}

	if info.Err != "" {
		return info.Title, info.Pid, info.AppName, fmt.Errorf("extension error: %s", info.Err)
	}

	return info.Title, info.Pid, info.AppName, nil
}

// getIdleTime (unchanged)
func getIdleTime(conn *dbus.Conn) (time.Duration, error) {
	obj := conn.Object("org.freedesktop.ScreenSaver", "/org/freedesktop/ScreenSaver")
	call := obj.Call("org.freedesktop.ScreenSaver.GetSessionIdleTime", 0)
	if call.Err != nil {
		obj = conn.Object("org.gnome.Mutter.IdleMonitor", "/org/gnome/Mutter/IdleMonitor/Core")
		call = obj.Call("org.gnome.Mutter.IdleMonitor.GetIdletime", 0)
		if call.Err != nil {
			return 0, fmt.Errorf("GetSessionIdleTime call failed: %w", call.Err)
		}
	}

	if len(call.Body) < 1 {
		return 0, fmt.Errorf("unexpected idle time response: %v", call.Body)
	}
	idleTimeMS, ok := call.Body[0].(uint64)
	if !ok {
		return 0, fmt.Errorf("idle time was not uint64: %T", call.Body[0])
	}

	return time.Duration(idleTimeMS) * time.Millisecond, nil
}

// readCmdline (unchanged)
func readCmdline(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid")
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	parts := strings.Split(string(b), "\x00")
	return strings.TrimSpace(strings.Join(parts, " ")), nil
}
