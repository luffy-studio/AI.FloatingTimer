//go:build windows

package main

import (
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pcFileName       = "overlay_ui_pcs.json"
	activityFileName = "overlay_ui_activity.csv"
)

type ActivityLog struct {
	sync.Mutex
	path string
}

var (
	store       PCStore
	activityLog ActivityLog
	timerHTTP   = &http.Client{Timeout: 4 * time.Second}
)

func startManagementService(listener net.Listener) {
	store.path = appDataPath(pcFileName)
	activityLog.path = appDataPath(activityFileName)
	if err := store.load(); err != nil {
		log.Printf("could not load PC list: %v", err)
		return
	}
	if err := billingStore.open(appConfig.Billing); err != nil {
		log.Printf("could not open billing database: %v", err)
	}
	defer billingStore.close()
	observeLocalTimers()
	mux := http.NewServeMux()
	mux.HandleFunc("/", servePage)
	mux.HandleFunc("/api/pcs", pcsHandler)
	mux.HandleFunc("/api/pcs/", pcHandler)
	registerDiscoveryHandlers(mux)
	registerBillingHandlers(mux)
	server := &http.Server{
		Addr: fmt.Sprintf(":%d", appConfig.UIDiscoveryPort), Handler: cors(headers(mux)),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	startDiscoveryScheduler()
	log.Printf("Timer Manager: http://localhost:%d", appConfig.UIDiscoveryPort)
	if err := server.Serve(listener); err != nil {
		logf("ui", "management server stopped: %v", err)
	}
}

func appDataPath(fileName string) string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), fileName)
	}
	return fileName
}

func (a *ActivityLog) append(event string, pc PC, action, payload, result string, status int, detail string) {
	a.Lock()
	defer a.Unlock()

	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("could not open activity log: %v", err)
		return
	}
	defer file.Close()

	info, _ := file.Stat()
	writer := csv.NewWriter(file)
	if info == nil || info.Size() == 0 {
		_ = writer.Write([]string{"timestamp", "event", "pc_id", "pc_name", "pc_address", "action", "payload", "result", "http_status", "detail"})
	}
	_ = writer.Write([]string{
		time.Now().Format(time.RFC3339), event, pc.ID, pc.Name, pc.Address,
		action, payload, result, fmt.Sprint(status), detail,
	})
	writer.Flush()
	if err := writer.Error(); err != nil {
		log.Printf("could not write activity log: %v", err)
	}
}

func normalizeAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" {
		return "", errors.New("enter a hostname or IP address")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return "", errors.New("address cannot include credentials, query parameters, fragments, or escaped paths")
	}
	path := strings.TrimSuffix(u.Path, "/")
	if path != "" && !isNumericID(strings.TrimPrefix(path, "/")) {
		return "", errors.New("address path must be a numeric on-behalf timer ID, such as /1")
	}
	port := appConfig.TimerPort
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return "", errors.New("timer port must be between 1 and 65535")
		}
	}
	return "http://" + net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(port)) + path, nil
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self' http: https:")
		next.ServeHTTP(w, r)
	})
}

func servePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, pageHTML)
}

func readPC(r *http.Request) (PC, error) {
	var pc PC
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pc); err != nil {
		return pc, errors.New("invalid JSON")
	}
	pc.Name = strings.TrimSpace(pc.Name)
	if pc.Name == "" {
		return pc, errors.New("name is required")
	}
	return pc, nil
}

func pcsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, store.list())
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "GET or POST required"})
		return
	}
	pc, err := readPC(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	pc, status, err := store.edit("", pc, false)
	if err != nil {
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	activityLog.append("pc_added", pc, "", "", "success", http.StatusCreated, "")
	writeJSON(w, 201, pc)
}

func pcHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/pcs/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		handleRecord(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "active" && r.Method == http.MethodPut {
		var body struct {
			Active *bool `json:"active"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body) != nil || body.Active == nil {
			writeJSON(w, 400, map[string]string{"error": "active must be true or false"})
			return
		}
		pc, err := store.setActive(parts[0], *body.Active)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, pc)
		return
	}
	http.NotFound(w, r)
}

func isNumericID(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func handleRecord(w http.ResponseWriter, r *http.Request, id string) {
	remove := r.Method == http.MethodDelete
	if !remove && r.Method != http.MethodPut {
		writeJSON(w, 405, map[string]string{"error": "PUT or DELETE required"})
		return
	}
	var pc PC
	var err error
	if !remove {
		pc, err = readPC(r)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	pc, status, err := store.edit(id, pc, remove)
	if err != nil {
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	event := "pc_updated"
	if remove {
		event = "pc_removed"
	}
	activityLog.append(event, pc, "", "", "success", status, "")
	if remove {
		writeJSON(w, status, map[string]string{"status": "success"})
	} else {
		writeJSON(w, status, pc)
	}
}

//go:embed ui.html
var pageHTML string
