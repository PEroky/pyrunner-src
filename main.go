package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/net/websocket"
	_ "modernc.org/sqlite"
)

var version = "dev"

//go:embed index.html
var indexHTML []byte

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type config struct {
	Port string
	User string
	Pass string
	Data string
}

var cfg config

func loadConfig() config {
	port := envOr("PYRUNNER_LISTEN_PORT", envOr("PYRUNNER_PORT", "8000"))
	// Kubernetes/Docker may inject PYRUNNER_PORT=tcp://x.x.x.x:8000 when
	// the service is named "pyrunner". Detect and ignore that.
	if strings.Contains(port, "://") {
		port = "8000"
	}
	c := config{
		Port: port,
		User: envOr("PYRUNNER_USER", ""),
		Pass: envOr("PYRUNNER_PASS", ""),
		Data: envOr("PYRUNNER_DATA", "."),
	}
	masked := "(disabled)"
	if c.Pass != "" {
		masked = "****"
	}
	log.Printf("config: port=%s user=%q pass=%s data=%s", c.Port, c.User, masked, c.Data)
	return c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Globals
// ---------------------------------------------------------------------------

var (
	db           *sql.DB
	runningProcs sync.Map // run_id string -> *procEntry
	sessions     sync.Map // token string -> expiry time.Time
)

type procEntry struct {
	cancel context.CancelFunc
	cmd    *exec.Cmd
}

// killGroup kills the entire process group (the process and all its children).
func (p *procEntry) killGroup() {
	if p.cmd.Process != nil {
		// Kill the entire process group: negative PID = process group
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	p.cancel()
}

type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// ---------------------------------------------------------------------------
// Terminal state (single session)
// ---------------------------------------------------------------------------

var (
	termMu   sync.Mutex
	termProc *os.Process // current terminal shell process
)

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	jsonResponse(w, status, map[string]string{"error": msg})
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validSession(token string) bool {
	if v, ok := sessions.Load(token); ok {
		if expiry, ok2 := v.(time.Time); ok2 && time.Now().Before(expiry) {
			return true
		}
		sessions.Delete(token)
	}
	return false
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always allow these paths without auth (terminal does its own check)
		if r.URL.Path == "/" || r.URL.Path == "/api/login" || r.URL.Path == "/api/auth/check" || r.URL.Path == "/api/terminal" {
			next.ServeHTTP(w, r)
			return
		}
		// If auth not enabled, pass through
		if cfg.User == "" && cfg.Pass == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Check session cookie
		cookie, err := r.Cookie("pyrunner_session")
		if err != nil || !validSession(cookie.Value) {
			jsonError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if body.Username != cfg.User || body.Password != cfg.Pass {
		jsonError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	token, err := generateToken()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "Failed to generate session")
		return
	}
	sessions.Store(token, time.Now().Add(24*time.Hour))
	http.SetCookie(w, &http.Cookie{
		Name:     "pyrunner_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("pyrunner_session"); err == nil {
		sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "pyrunner_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	authEnabled := cfg.User != "" || cfg.Pass != ""
	authenticated := false
	if !authEnabled {
		authenticated = true
	} else {
		if cookie, err := r.Cookie("pyrunner_session"); err == nil {
			authenticated = validSession(cookie.Value)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"authenticated": authenticated,
		"auth_enabled":  authEnabled,
	})
}

// ---------------------------------------------------------------------------
// Database
// ---------------------------------------------------------------------------

func initDB(dataDir string) (*sql.DB, error) {
	dbPath := filepath.Join(dataDir, "pyrunner.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	schema := `
	CREATE TABLE IF NOT EXISTS scripts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE NOT NULL,
		filename TEXT UNIQUE NOT NULL,
		description TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		script_id INTEGER REFERENCES scripts(id),
		status TEXT DEFAULT 'running',
		exit_code INTEGER,
		started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		finished_at DATETIME,
		output TEXT DEFAULT ''
	);`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("create tables: %w", err)
	}
	log.Printf("database initialized: %s", dbPath)
	return db, nil
}

// ---------------------------------------------------------------------------
// Script helpers
// ---------------------------------------------------------------------------

func scriptsDir() string {
	return filepath.Join(cfg.Data, "scripts")
}

func scriptPath(filename string) string {
	return filepath.Join(scriptsDir(), filename)
}

// ---------------------------------------------------------------------------
// Script handlers
// ---------------------------------------------------------------------------

func handleListScripts(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
		SELECT s.id, s.name, s.filename, s.description, s.created_at, s.updated_at,
		       lr.status, lr.exit_code, lr.started_at, lr.finished_at
		FROM scripts s
		LEFT JOIN (
			SELECT r1.* FROM runs r1
			INNER JOIN (SELECT script_id, MAX(id) AS max_id FROM runs WHERE script_id IS NOT NULL GROUP BY script_id) r2
			ON r1.id = r2.max_id
		) lr ON s.id = lr.script_id
		ORDER BY s.name`)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type scriptRow struct {
		ID          int64      `json:"id"`
		Name        string     `json:"name"`
		Filename    string     `json:"filename"`
		Description string     `json:"description"`
		CreatedAt   string     `json:"created_at"`
		UpdatedAt   string     `json:"updated_at"`
		LastStatus  *string    `json:"last_status"`
		LastExit    *int       `json:"last_exit_code"`
		LastStarted *string    `json:"last_started_at"`
		LastFinish  *string    `json:"last_finished_at"`
	}

	var result []scriptRow
	for rows.Next() {
		var s scriptRow
		if err := rows.Scan(&s.ID, &s.Name, &s.Filename, &s.Description,
			&s.CreatedAt, &s.UpdatedAt,
			&s.LastStatus, &s.LastExit, &s.LastStarted, &s.LastFinish); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		result = append(result, s)
	}
	if result == nil {
		result = []scriptRow{}
	}
	jsonResponse(w, http.StatusOK, result)
}

func handleCreateScript(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if body.Name == "" {
		jsonError(w, http.StatusBadRequest, "Name is required")
		return
	}

	filename := body.Name + ".py"
	fp := scriptPath(filename)

	if err := os.WriteFile(fp, []byte(body.Content), 0644); err != nil {
		jsonError(w, http.StatusInternalServerError, fmt.Sprintf("write file: %v", err))
		return
	}

	result, err := db.Exec(`INSERT INTO scripts (name, filename, description) VALUES (?, ?, ?)`,
		body.Name, filename, body.Description)
	if err != nil {
		os.Remove(fp)
		jsonError(w, http.StatusConflict, fmt.Sprintf("create script: %v", err))
		return
	}
	id, _ := result.LastInsertId()
	log.Printf("created script %d: %s", id, body.Name)
	jsonResponse(w, http.StatusCreated, map[string]interface{}{"id": id, "name": body.Name, "filename": filename})
}

func handleGetScript(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.PathValue("id"))

	var s struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Filename    string `json:"filename"`
		Description string `json:"description"`
		CreatedAt   string `json:"created_at"`
		UpdatedAt   string `json:"updated_at"`
	}
	err := db.QueryRow(`SELECT id, name, filename, description, created_at, updated_at FROM scripts WHERE id = ?`, id).
		Scan(&s.ID, &s.Name, &s.Filename, &s.Description, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		jsonError(w, http.StatusNotFound, "Script not found")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	content, err := os.ReadFile(scriptPath(s.Filename))
	if err != nil {
		content = []byte("")
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"id":          s.ID,
		"name":        s.Name,
		"filename":    s.Filename,
		"description": s.Description,
		"created_at":  s.CreatedAt,
		"updated_at":  s.UpdatedAt,
		"content":     string(content),
	})
}

func handleUpdateScript(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.PathValue("id"))

	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	var oldFilename string
	err := db.QueryRow(`SELECT filename FROM scripts WHERE id = ?`, id).Scan(&oldFilename)
	if err == sql.ErrNoRows {
		jsonError(w, http.StatusNotFound, "Script not found")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	newFilename := oldFilename
	if body.Name != "" {
		newFilename = body.Name + ".py"
	}

	// Write content to the new filename location
	if err := os.WriteFile(scriptPath(newFilename), []byte(body.Content), 0644); err != nil {
		jsonError(w, http.StatusInternalServerError, fmt.Sprintf("write file: %v", err))
		return
	}

	// Handle rename: remove old file if filename changed
	if newFilename != oldFilename {
		os.Remove(scriptPath(oldFilename))
	}

	_, err = db.Exec(`UPDATE scripts SET name=?, filename=?, description=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		body.Name, newFilename, body.Description, id)
	if err != nil {
		jsonError(w, http.StatusConflict, fmt.Sprintf("update script: %v", err))
		return
	}

	log.Printf("updated script %s", id)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func handleDeleteScript(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.PathValue("id"))

	var filename string
	err := db.QueryRow(`SELECT filename FROM scripts WHERE id = ?`, id).Scan(&filename)
	if err == sql.ErrNoRows {
		jsonError(w, http.StatusNotFound, "Script not found")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	db.Exec(`DELETE FROM runs WHERE script_id = ?`, id)
	db.Exec(`DELETE FROM scripts WHERE id = ?`, id)
	os.Remove(scriptPath(filename))

	log.Printf("deleted script %s", id)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// ---------------------------------------------------------------------------
// Run script
// ---------------------------------------------------------------------------

func handleRunScript(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.PathValue("id"))

	var filename string
	var scriptID int64
	err := db.QueryRow(`SELECT id, filename FROM scripts WHERE id = ?`, id).Scan(&scriptID, &filename)
	if err == sql.ErrNoRows {
		jsonError(w, http.StatusNotFound, "Script not found")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Stop any previous running instance of this script
	rows, _ := db.Query(`SELECT id FROM runs WHERE script_id = ? AND status = 'running'`, scriptID)
	if rows != nil {
		for rows.Next() {
			var prevID int64
			rows.Scan(&prevID)
			prevIDStr := fmt.Sprintf("%d", prevID)
			if v, ok := runningProcs.Load(prevIDStr); ok {
				v.(*procEntry).killGroup()
				log.Printf("auto-stopped previous run %d for script %d", prevID, scriptID)
			}
		}
		rows.Close()
	}

	result, err := db.Exec(`INSERT INTO runs (script_id, status) VALUES (?, 'running')`, scriptID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	runID, _ := result.LastInsertId()
	runIDStr := fmt.Sprintf("%d", runID)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "python3", "-u", scriptPath(filename))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	sw := &syncWriter{}
	cmd.Stdout = sw
	cmd.Stderr = sw

	if err := cmd.Start(); err != nil {
		cancel()
		db.Exec(`UPDATE runs SET status='error', exit_code=-1, finished_at=CURRENT_TIMESTAMP, output=? WHERE id=?`,
			err.Error(), runID)
		jsonError(w, http.StatusInternalServerError, fmt.Sprintf("start: %v", err))
		return
	}

	runningProcs.Store(runIDStr, &procEntry{cancel: cancel, cmd: cmd})
	log.Printf("started run %d for script %d (%s)", runID, scriptID, filename)

	go func() {
		defer cancel()
		defer runningProcs.Delete(runIDStr)

		// Ticker to flush output to DB
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		for {
			select {
			case <-ticker.C:
				output := sw.String()
				db.Exec(`UPDATE runs SET output=? WHERE id=?`, output, runID)
			case err := <-done:
				output := sw.String()
				exitCode := 0
				status := "success"
				if err != nil {
					status = "error"
					if exitErr, ok := err.(*exec.ExitError); ok {
						exitCode = exitErr.ExitCode()
					} else {
						exitCode = -1
					}
				}
				if ctx.Err() == context.Canceled && status == "error" {
					status = "stopped"
				}
				db.Exec(`UPDATE runs SET status=?, exit_code=?, finished_at=CURRENT_TIMESTAMP, output=? WHERE id=?`,
					status, exitCode, output, runID)
				log.Printf("run %d finished: status=%s exit_code=%d", runID, status, exitCode)
				return
			}
		}
	}()

	jsonResponse(w, http.StatusOK, map[string]interface{}{"run_id": runID})
}

// ---------------------------------------------------------------------------
// Run-related handlers
// ---------------------------------------------------------------------------

func handleLatestRun(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.PathValue("id"))
	var runID int64
	err := db.QueryRow(`SELECT id FROM runs WHERE script_id = ? ORDER BY id DESC LIMIT 1`, id).Scan(&runID)
	if err == sql.ErrNoRows {
		jsonError(w, http.StatusNotFound, "No runs found")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"run_id": runID})
}

func handleStopRun(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.PathValue("id"))

	if v, ok := runningProcs.Load(id); ok {
		entry := v.(*procEntry)
		entry.killGroup()
		log.Printf("stopped run %s (killed process group)", id)
		jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
		return
	}
	jsonError(w, http.StatusNotFound, "Run not found or not running")
}

func handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.PathValue("id"))

	var run struct {
		ID         int64   `json:"id"`
		ScriptID   *int64  `json:"script_id"`
		Status     string  `json:"status"`
		ExitCode   *int    `json:"exit_code"`
		StartedAt  string  `json:"started_at"`
		FinishedAt *string `json:"finished_at"`
		Output     string  `json:"output"`
	}
	err := db.QueryRow(`SELECT id, script_id, status, exit_code, started_at, finished_at, output FROM runs WHERE id = ?`, id).
		Scan(&run.ID, &run.ScriptID, &run.Status, &run.ExitCode, &run.StartedAt, &run.FinishedAt, &run.Output)
	if err == sql.ErrNoRows {
		jsonError(w, http.StatusNotFound, "Run not found")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, run)
}

func handleStreamRun(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.PathValue("id"))

	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "Streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	var lastLen int
	for {
		var status, output string
		err := db.QueryRow(`SELECT status, output FROM runs WHERE id = ?`, id).Scan(&status, &output)
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", "Run not found")
			flusher.Flush()
			return
		}

		if len(output) > lastLen {
			newData := output[lastLen:]
			lastLen = len(output)
			// Encode as JSON string to safely transmit newlines
			encoded, _ := json.Marshal(newData)
			fmt.Fprintf(w, "event: output\ndata: %s\n\n", encoded)
			flusher.Flush()
		}

		if status != "running" {
			meta, _ := json.Marshal(map[string]interface{}{"status": status})
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", meta)
			flusher.Flush()
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}

// ---------------------------------------------------------------------------
// Pip handlers
// ---------------------------------------------------------------------------

func handlePipList(w http.ResponseWriter, r *http.Request) {
	cmd := exec.Command("pip3", "list", "--format", "json")
	output, err := cmd.Output()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, fmt.Sprintf("pip3 list: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(output)
}

func startPipRun(command string, args []string) (int64, error) {
	result, err := db.Exec(`INSERT INTO runs (script_id, status) VALUES (NULL, 'running')`)
	if err != nil {
		return 0, err
	}
	runID, _ := result.LastInsertId()
	runIDStr := fmt.Sprintf("%d", runID)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	sw := &syncWriter{}
	cmd.Stdout = sw
	cmd.Stderr = sw

	if err := cmd.Start(); err != nil {
		cancel()
		db.Exec(`UPDATE runs SET status='error', exit_code=-1, finished_at=CURRENT_TIMESTAMP, output=? WHERE id=?`,
			err.Error(), runID)
		return runID, fmt.Errorf("start: %w", err)
	}

	runningProcs.Store(runIDStr, &procEntry{cancel: cancel, cmd: cmd})
	log.Printf("started pip run %d: %s %v", runID, command, args)

	go func() {
		defer cancel()
		defer runningProcs.Delete(runIDStr)

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		for {
			select {
			case <-ticker.C:
				output := sw.String()
				db.Exec(`UPDATE runs SET output=? WHERE id=?`, output, runID)
			case err := <-done:
				output := sw.String()
				exitCode := 0
				status := "success"
				if err != nil {
					status = "error"
					if exitErr, ok := err.(*exec.ExitError); ok {
						exitCode = exitErr.ExitCode()
					} else {
						exitCode = -1
					}
				}
				if ctx.Err() == context.Canceled && status == "error" {
					status = "stopped"
				}
				db.Exec(`UPDATE runs SET status=?, exit_code=?, finished_at=CURRENT_TIMESTAMP, output=? WHERE id=?`,
					status, exitCode, output, runID)
				log.Printf("pip run %d finished: status=%s exit_code=%d", runID, status, exitCode)
				if status == "success" {
					pipFreezeToFile()
				}
				return
			}
		}
	}()

	return runID, nil
}

func handlePipInstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Packages string `json:"packages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if strings.TrimSpace(body.Packages) == "" {
		jsonError(w, http.StatusBadRequest, "Packages required")
		return
	}

	args := append([]string{"install", "--break-system-packages"}, strings.Fields(body.Packages)...)
	runID, err := startPipRun("pip3", args)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"run_id": runID})
}

func handlePipUninstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Packages string `json:"packages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if strings.TrimSpace(body.Packages) == "" {
		jsonError(w, http.StatusBadRequest, "Packages required")
		return
	}

	args := append([]string{"uninstall", "-y", "--break-system-packages"}, strings.Fields(body.Packages)...)
	runID, err := startPipRun("pip3", args)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"run_id": runID})
}

// ---------------------------------------------------------------------------
// Terminal WebSocket handler
// ---------------------------------------------------------------------------

func handleTerminalWS(ws *websocket.Conn) {
	// Manual auth check (websocket bypasses normal middleware)
	if cfg.User != "" || cfg.Pass != "" {
		cookie, err := ws.Request().Cookie("pyrunner_session")
		if err != nil || !validSession(cookie.Value) {
			ws.Write([]byte("\r\nUnauthorized\r\n"))
			ws.Close()
			return
		}
	}

	// Kill previous terminal session if any
	termMu.Lock()
	if termProc != nil {
		termProc.Kill()
		termProc.Wait()
		termProc = nil
	}
	termMu.Unlock()

	// Start bash with a PTY
	cmd := exec.Command("/bin/bash")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		ws.Write([]byte(fmt.Sprintf("\r\nFailed to start shell: %v\r\n", err)))
		ws.Close()
		return
	}

	termMu.Lock()
	termProc = cmd.Process
	termMu.Unlock()

	log.Printf("terminal session started (pid %d)", cmd.Process.Pid)

	defer func() {
		ptmx.Close()
		cmd.Process.Kill()
		cmd.Wait()
		termMu.Lock()
		if termProc != nil && termProc.Pid == cmd.Process.Pid {
			termProc = nil
		}
		termMu.Unlock()
		log.Printf("terminal session ended (pid %d)", cmd.Process.Pid)
	}()

	// PTY stdout -> websocket
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				break
			}
			if _, err := ws.Write(buf[:n]); err != nil {
				break
			}
		}
		ws.Close()
	}()

	// Websocket -> PTY stdin
	buf := make([]byte, 4096)
	for {
		n, err := ws.Read(buf)
		if err != nil {
			break
		}
		if _, err := ptmx.Write(buf[:n]); err != nil {
			break
		}
	}
}

// ---------------------------------------------------------------------------
// PATCH content handler
// ---------------------------------------------------------------------------

func handlePatchContent(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.PathValue("id"))

	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	var filename string
	err := db.QueryRow(`SELECT filename FROM scripts WHERE id = ?`, id).Scan(&filename)
	if err == sql.ErrNoRows {
		jsonError(w, http.StatusNotFound, "Script not found")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := os.WriteFile(scriptPath(filename), []byte(body.Content), 0644); err != nil {
		jsonError(w, http.StatusInternalServerError, fmt.Sprintf("write file: %v", err))
		return
	}

	db.Exec(`UPDATE scripts SET updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// ---------------------------------------------------------------------------
// Pip persistence helpers
// ---------------------------------------------------------------------------

func pipFreezeToFile() {
	out, err := exec.Command("pip3", "freeze", "--break-system-packages").Output()
	if err != nil {
		log.Printf("pip freeze failed: %v", err)
		return
	}
	reqFile := filepath.Join(cfg.Data, "requirements.txt")
	if err := os.WriteFile(reqFile, out, 0644); err != nil {
		log.Printf("write requirements.txt failed: %v", err)
	}
}

func pipRestoreFromFile() {
	reqFile := filepath.Join(cfg.Data, "requirements.txt")
	if _, err := os.Stat(reqFile); err != nil {
		return // no file, skip
	}
	log.Printf("restoring pip packages from %s", reqFile)
	cmd := exec.Command("pip3", "install", "--break-system-packages", "-r", reqFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("pip restore failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg = loadConfig()

	// Create scripts directory
	if err := os.MkdirAll(scriptsDir(), 0755); err != nil {
		log.Fatalf("create scripts dir: %v", err)
	}

	// Initialize database
	var err error
	db, err = initDB(cfg.Data)
	if err != nil {
		log.Fatalf("init db: %v", err)
	}
	defer db.Close()

	// Restore pip packages from requirements.txt if present
	pipRestoreFromFile()

	mux := http.NewServeMux()

	// Serve embedded HTML
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	// Auth endpoints
	mux.HandleFunc("POST /api/login", handleLogin)
	mux.HandleFunc("POST /api/logout", handleLogout)
	mux.HandleFunc("GET /api/auth/check", handleAuthCheck)

	// Script endpoints
	mux.HandleFunc("GET /api/scripts", handleListScripts)
	mux.HandleFunc("POST /api/scripts", handleCreateScript)
	mux.HandleFunc("GET /api/scripts/{id}", handleGetScript)
	mux.HandleFunc("PUT /api/scripts/{id}", handleUpdateScript)
	mux.HandleFunc("PATCH /api/scripts/{id}/content", handlePatchContent)
	mux.HandleFunc("DELETE /api/scripts/{id}", handleDeleteScript)
	mux.HandleFunc("POST /api/scripts/{id}/run", handleRunScript)

	// Run endpoints
	mux.HandleFunc("GET /api/scripts/{id}/latest-run", handleLatestRun)
	mux.HandleFunc("POST /api/runs/{id}/stop", handleStopRun)
	mux.HandleFunc("GET /api/runs/{id}", handleGetRun)
	mux.HandleFunc("GET /api/runs/{id}/stream", handleStreamRun)

	// Pip endpoints
	mux.HandleFunc("GET /api/pip/list", handlePipList)
	mux.HandleFunc("POST /api/pip/install", handlePipInstall)
	mux.HandleFunc("POST /api/pip/uninstall", handlePipUninstall)

	// Terminal WebSocket (auth handled inside handler)
	mux.Handle("GET /api/terminal", websocket.Handler(handleTerminalWS))

	handler := authMiddleware(mux)

	addr := ":" + cfg.Port
	log.Printf("pyrunner %s listening on %s", version, addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// Ensure io import is used (used by syncWriter implementing io.Writer)
var _ io.Writer = (*syncWriter)(nil)
