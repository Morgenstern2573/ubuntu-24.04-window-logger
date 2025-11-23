package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/rs/xid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/sqliteshim"
)

// --- Configuration ---
const (
	pollInterval = time.Second
	dbFileName   = "activity.sqlite"
	queueSize    = 1000

	// Batching Configuration
	// We hold logs in memory and write them in one transaction to save SSD wear.
	batchSize    = 300             // Write to disk after collecting 300 entries...
	batchTimeout = 5 * time.Minute // ...or every 5 minutes, whichever comes first.
)

// LogEntry represents a row in the database
type LogEntry struct {
	bun.BaseModel `bun:"table:activity_logs,alias:al"`

	ID        string    `bun:",pk" json:"id"`
	Timestamp time.Time `bun:"timestamp,notnull" json:"timestamp"`
	Title     string    `bun:"title" json:"title,omitempty"`
	AppName   string    `bun:"app_name" json:"appName,omitempty"`
	PID       int       `bun:"pid" json:"pid,omitempty"`
	Cmdline   string    `bun:"cmdline" json:"cmdline,omitempty"`
	IdleTimeS float64   `bun:"idle_time_s" json:"idle_time_s,omitempty"`
	IsSwitch  bool      `bun:"is_switch" json:"is_switch,omitempty"`
	Error     string    `bun:"error" json:"error,omitempty"`
}

type WindowInfo struct {
	Title   string `json:"title"`
	Pid     int    `json:"pid"`
	AppName string `json:"appName"`
	Err     string `json:"err"`
}

func main() {
	verbose := flag.Bool("v", false, "Enable verbose stdout logging")
	flag.Parse()

	// 1. Create cancellable context for graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 2. Setup Database
	db, err := setupDatabase(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Database setup failed: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// 3. Setup D-Bus
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Could not connect to D-Bus: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// 4. Start Background DB Worker
	var wg sync.WaitGroup
	writeQueue := make(chan LogEntry, queueSize)

	wg.Add(1)
	go dbWriterWorker(db, writeQueue, &wg)

	fmt.Println("Starting focus logger agent... (Press Ctrl+C to stop)")
	fmt.Printf("Polling every %s.\n", pollInterval)
	fmt.Printf("Saving data to SQLite DB at: ~/%s\n", dbFileName)
	fmt.Printf("Batching enabled: Writing every %d entries or %s.\n", batchSize, batchTimeout)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastFocusedPID int

	// 5. Main Loop
loop:
	for {
		select {
		case <-ctx.Done():
			// Context cancelled (Ctrl+C)
			fmt.Println("\nShutdown signal received. Stopping poller...")
			break loop
		case <-ticker.C:
			// Poll
			entry := collectLogEntry(conn, &lastFocusedPID)

			if *verbose {
				fmt.Printf("[%s] %s (%s) [ID: %s]\n",
					entry.Timestamp.Format(time.TimeOnly),
					entry.AppName,
					entry.Title,
					entry.ID,
				)
			}

			select {
			case writeQueue <- entry:
			default:
				fmt.Fprintf(os.Stderr, "Warning: DB Write queue full. Dropping log entry.\n")
			}
		}
	}

	// 6. Cleanup
	close(writeQueue) // Signal worker that no more data is coming
	fmt.Println("Waiting for database worker to finish pending writes...")
	wg.Wait() // Block until worker drains the queue
	fmt.Println("Done.")
}

// dbWriterWorker accepts context and WaitGroup for lifecycle management.
// It buffers logs and writes them in batches to reduce disk I/O.
func dbWriterWorker(db *bun.DB, queue <-chan LogEntry, wg *sync.WaitGroup) {
	defer wg.Done()

	// Buffer to hold logs in memory
	buffer := make([]LogEntry, 0, batchSize)

	// Ticker to force a write if the buffer doesn't fill up fast enough
	ticker := time.NewTicker(batchTimeout)
	defer ticker.Stop()

	// Helper function to write the current buffer to DB
	flush := func() {
		if len(buffer) == 0 {
			return
		}

		// Use context.Background() here. If 'main' cancels 'ctx' (Ctrl+C),
		// we still want to finish writing the logs currently in the buffer
		// rather than aborting mid-write.
		_, err := db.NewInsert().Model(&buffer).Exec(context.Background())
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing batch to DB: %v\n", err)
		}

		// Reset buffer (keep capacity to avoid reallocation)
		buffer = buffer[:0]
	}

	for {
		select {
		case entry, ok := <-queue:
			if !ok {
				// Channel closed (Main function finished)
				flush() // Write whatever is left
				return
			}

			buffer = append(buffer, entry)

			// If buffer is full, write immediately
			if len(buffer) >= batchSize {
				flush()
				// Reset ticker so we don't write again immediately after a full batch
				ticker.Reset(batchTimeout)
			}

		case <-ticker.C:
			// Timer expired, write whatever we have
			flush()
		}
	}
}

// setupDatabase initializes SQLite
func setupDatabase(ctx context.Context) (*bun.DB, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("getting home dir: %w", err)
	}

	dbPath := filepath.Join(homeDir, dbFileName)
	sqldb, err := sql.Open(sqliteshim.ShimName, dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite file: %w", err)
	}

	// Enable WAL (Write-Ahead Logging) to improve concurrency and durability
	// This persists in the database file, so running once is enough.
	if _, err := sqldb.ExecContext(ctx, "PRAGMA journal_mode=WAL;"); err != nil {
		return nil, fmt.Errorf("enabling WAL mode: %w", err)
	}
	// Recommended pragmas when using WAL. These are best-effort; failures are non-fatal.
	if _, err := sqldb.ExecContext(ctx, "PRAGMA synchronous=OFF;"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set synchronous=NORMAL: %v\n", err)
	}
	if _, err := sqldb.ExecContext(ctx, "PRAGMA busy_timeout=5000;"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set busy_timeout: %v\n", err)
	}

	db := bun.NewDB(sqldb, sqlitedialect.New())

	_, err = db.NewCreateTable().
		Model((*LogEntry)(nil)).
		IfNotExists().
		Exec(ctx)

	if err != nil {
		return nil, fmt.Errorf("creating table: %w", err)
	}

	return db, nil
}

// collectLogEntry gathers all data
func collectLogEntry(conn *dbus.Conn, lastPID *int) LogEntry {
	entry := LogEntry{
		ID:        xid.New().String(),
		Timestamp: time.Now(),
	}

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
