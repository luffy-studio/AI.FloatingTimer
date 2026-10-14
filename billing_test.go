//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

func configureBillingTest(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	oldEnabled, oldPath, oldBackup, oldToken := billingStore.enabled, billingStore.path, billingStore.backupDir, billingStore.token
	oldClient := timerHTTP
	store.Lock()
	oldPCs := store.PCs
	store.PCs = nil
	store.Unlock()
	billingStore.enabled = true
	billingStore.path = filepath.Join(directory, "billing.xlsx")
	billingStore.backupDir = filepath.Join(directory, "backups")
	billingStore.token = "test-token"
	timerHTTP = &http.Client{Timeout: 2 * time.Second}
	t.Cleanup(func() {
		billingStore.enabled, billingStore.path, billingStore.backupDir, billingStore.token = oldEnabled, oldPath, oldBackup, oldToken
		timerHTTP = oldClient
		store.Lock()
		store.PCs = oldPCs
		store.Unlock()
	})
	return directory
}

func setBillingTestPCs(pcs ...PC) {
	store.Lock()
	store.PCs = append([]PC(nil), pcs...)
	store.Unlock()
}

func createLegacyBillingWorkbook(t *testing.T, path string, session Session) {
	t.Helper()
	f := excelize.NewFile()
	if err := f.SetSheetName("Sheet1", "Sessions"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.NewSheet("Payments"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.NewSheet("Daily Summary"); err != nil {
		t.Fatal(err)
	}
	if err := writeRow(f, "Sessions", 1, legacySessionHeaders); err != nil {
		t.Fatal(err)
	}
	if err := writeRow(f, "Sessions", 2, rowFor(session)); err != nil {
		t.Fatal(err)
	}
	if err := writeRow(f, "Payments", 1, paymentHeaders); err != nil {
		t.Fatal(err)
	}
	if err := writeRow(f, "Payments", 2, []string{"P-legacy", session.ID, session.PaymentAt, "10", "partial"}); err != nil {
		t.Fatal(err)
	}
	if err := writeRow(f, "Daily Summary", 1, []string{"Date", "Collected Amount", "Payment Count"}); err != nil {
		t.Fatal(err)
	}
	if err := f.SaveAs(path); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyBillingWorkbookMigratesAndPreservesPaymentData(t *testing.T) {
	directory := configureBillingTest(t)
	sessionStart := time.Date(2026, time.January, 2, 16, 0, 0, 0, time.Local)
	sessionCreated := time.Date(2026, time.January, 2, 15, 4, 5, 0, time.Local)
	legacy := Session{
		ID: "S-legacy", PCID: "pc-1", PCName: "Console 1", PCAddress: "http://127.0.0.1:18081/1",
		Category: "PC Pro", CustomerName: "Legacy Customer", Players: 2, Status: "open",
		StartedAt: sessionStart.Format(time.RFC3339Nano), ExpectedEndAt: sessionStart.Add(45 * time.Minute).Format(time.RFC3339Nano),
		PurchasedMinutes: 30, AdditionalMinutes: 15, PricingSnapshot: "legacy price", EstimatedAmount: 20,
		FinalAmount: 30, AmountPaid: 10, PaymentStatus: "partial", PaymentAt: sessionCreated.Format(time.RFC3339Nano),
		CreatedAt: sessionCreated.Format(time.RFC3339Nano), UpdatedAt: sessionCreated.Format(time.RFC3339Nano),
	}
	billingStore.path = filepath.Join(directory, "billing.xlsx")
	createLegacyBillingWorkbook(t, billingStore.path, legacy)

	f, err := billingStore.openLocked()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := sessionRows(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !reflect.DeepEqual(rows[0], sessionHeaders) {
		t.Fatalf("unexpected migrated sheet rows: %#v", rows)
	}
	if got := f.GetSheetList(); !reflect.DeepEqual(got, []string{"Sessions"}) {
		t.Fatalf("expected only Sessions worksheet after migration, got %v", got)
	}
	for cell, want := range map[string]string{
		"A2": sessionCreated.Format("02-01-2006"),
		"B2": sessionCreated.Format("15:04:05"),
		"C2": legacy.CustomerName,
		"D2": legacy.PCName,
		"E2": "2",
		"F2": sessionStart.Format("15:04:05"),
		"G2": "45",
		"H2": "₹10",
	} {
		got, err := f.GetCellValue("Sessions", cell)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", cell, got, want)
		}
	}
	for cell, minValue := range map[string]float64{"A2": 40000, "B2": 0, "F2": 0, "H2": 0} {
		raw, err := f.GetCellValue("Sessions", cell, excelize.Options{RawCellValue: true})
		if err != nil {
			t.Fatal(err)
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value < minValue || ((cell == "B2" || cell == "F2") && value >= 1) {
			t.Errorf("%s is not a correctly stored Excel number: %q (%v)", cell, raw, err)
		}
	}
	migrated, err := sessionsFromWorkbook(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated) != 1 || migrated[0].ID != legacy.ID || migrated[0].PaymentStatus != "partial" ||
		migrated[0].AmountPaid != 10 || len(migrated[0].payments) != 1 || migrated[0].payments[0].ID != "P-legacy" {
		t.Fatalf("legacy session or payment history was not preserved: %+v", migrated)
	}
	style, err := f.GetCellStyle("Sessions", "A1")
	if err != nil || style == 0 {
		t.Fatalf("expected formatted header, style=%d err=%v", style, err)
	}
	backups, err := filepath.Glob(filepath.Join(billingStore.backupDir, "*.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected one pre-migration backup, found %d", len(backups))
	}

	if _, err := billingStore.update(legacy.ID, "pay", 0, 0); err != nil {
		t.Fatal(err)
	}
	afterUpdate, err := billingStore.sessions("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterUpdate) != 1 || afterUpdate[0].AmountPaid != legacy.FinalAmount || len(afterUpdate[0].payments) != 2 {
		t.Fatalf("session update duplicated/lost workbook state: %+v", afterUpdate)
	}
}

func newBillingTimerServer(t *testing.T, failPlay bool) (*httptest.Server, *sync.Mutex, *[]string, *TimerStatus) {
	t.Helper()
	var mu sync.Mutex
	requests := []string{}
	state := &TimerStatus{RemainingSeconds: 77, TimerStatus: "paused"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/1/status" && !strings.HasPrefix(r.URL.Path, "/1/") {
			http.Error(w, "unexpected timer target", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/1/status" {
			mu.Lock()
			response := *state
			mu.Unlock()
			writeJSON(w, http.StatusOK, response)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/1/set":
			var body struct {
				Seconds int `json:"seconds"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid set request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			state.RemainingSeconds = body.Seconds
			mu.Unlock()
		case "/1/play":
			if failPlay {
				writeJSON(w, http.StatusBadGateway, map[string]string{"status": "error", "message": "simulated play failure"})
				return
			}
			mu.Lock()
			state.IsRunning = true
			state.TimerStatus = "running"
			mu.Unlock()
		case "/1/pause":
			mu.Lock()
			state.IsRunning = false
			state.TimerStatus = "paused"
			mu.Unlock()
		case "/1/hide":
			mu.Lock()
			state.TimeUpVisible = false
			mu.Unlock()
		default:
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
	}))
	t.Cleanup(server.Close)
	timerHTTP = server.Client()
	return server, &mu, &requests, state
}

func TestSessionStartAPIStartsOnlySelectedSharedHostTimer(t *testing.T) {
	configureBillingTest(t)
	server, mu, requests, state := newBillingTimerServer(t, false)
	setBillingTestPCs(
		PC{ID: "host", Name: "Host Timer", Address: server.URL},
		PC{ID: "console-1", Name: "Console 1", Address: server.URL + "/1"},
	)
	entry := pricingCatalog().Entries[0]
	mux := http.NewServeMux()
	registerBillingHandlers(mux)
	body := fmt.Sprintf(`{"pc_id":"console-1","category":%q,"customer_name":"Test Customer","players":%d,"duration_minutes":%d}`,
		entry.Category, entry.Players, entry.DurationMinutes)
	request := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	request.Header.Set("X-Billing-Token", "test-token")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("session start returned HTTP %d: %s", response.Code, response.Body.String())
	}
	var started Session
	if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.AmountPaid != entry.Amount || started.PaymentStatus != "paid" {
		t.Fatalf("session did not record the full selected price: %+v", started)
	}
	mu.Lock()
	gotRequests := append([]string(nil), (*requests)...)
	remaining, running := state.RemainingSeconds, state.IsRunning
	mu.Unlock()
	if !reflect.DeepEqual(gotRequests, []string{"GET /1/status", "POST /1/set", "POST /1/play"}) {
		t.Fatalf("unexpected timer requests (must target only /1): %v", gotRequests)
	}
	if remaining != entry.DurationMinutes*60 || !running {
		t.Fatalf("selected timer did not start at purchased duration: remaining=%d running=%v", remaining, running)
	}
	f, err := excelize.OpenFile(billingStore.path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := sessionRows(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !reflect.DeepEqual(rows[0], sessionHeaders) {
		t.Fatalf("unexpected billing workbook content: %#v", rows)
	}
	wantCells := map[string]string{
		"A2": started.CreatedAt,
		"C2": started.CustomerName,
		"D2": started.PCName,
		"E2": strconv.Itoa(started.Players),
		"G2": strconv.Itoa(entry.DurationMinutes),
		"H2": "₹" + strconv.Itoa(entry.Amount),
	}
	createdAt, err := sessionTimestamp(started.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	startedAt, err := sessionTimestamp(started.StartedAt)
	if err != nil {
		t.Fatal(err)
	}
	wantCells["A2"] = createdAt.Format("02-01-2006")
	wantCells["B2"] = createdAt.Format("15:04:05")
	wantCells["F2"] = startedAt.Format("15:04:05")
	for cell, want := range wantCells {
		got, err := f.GetCellValue("Sessions", cell)
		if err != nil || got != want {
			t.Errorf("workbook %s = %q, err=%v; want %q", cell, got, err, want)
		}
	}
	_ = f.Close()

	request = httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	request.Header.Set("X-Billing-Token", "test-token")
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("duplicate session start returned HTTP %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*requests) != len(gotRequests) {
		t.Fatalf("duplicate start sent additional timer commands: %v", *requests)
	}
}

func TestSessionStartTimerFailureRestoresTimerAndDoesNotWriteSession(t *testing.T) {
	configureBillingTest(t)
	server, mu, requests, state := newBillingTimerServer(t, true)
	setBillingTestPCs(PC{ID: "console-1", Name: "Console 1", Address: server.URL + "/1"})
	entry := pricingCatalog().Entries[0]
	mux := http.NewServeMux()
	registerBillingHandlers(mux)
	body := fmt.Sprintf(`{"pc_id":"console-1","category":%q,"customer_name":"Test Customer","players":%d,"duration_minutes":%d}`,
		entry.Category, entry.Players, entry.DurationMinutes)
	request := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	request.Header.Set("X-Billing-Token", "test-token")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "previous timer state was restored") {
		t.Fatalf("timer failure was not reported with rollback: HTTP %d %s", response.Code, response.Body.String())
	}
	if sessions, err := billingStore.sessions("", ""); err != nil || len(sessions) != 0 {
		t.Fatalf("failed timer start created a session: sessions=%v err=%v", sessions, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if state.RemainingSeconds != 77 || state.IsRunning || state.TimerStatus != "paused" {
		t.Fatalf("previous timer state was not restored: %+v", state)
	}
	if !reflect.DeepEqual(*requests, []string{
		"GET /1/status", "POST /1/set", "POST /1/play", "POST /1/set", "POST /1/pause", "POST /1/hide",
	}) {
		t.Fatalf("unexpected timer rollback sequence: %v", *requests)
	}
}

func TestSessionStartWorkbookFailureRestoresSelectedTimer(t *testing.T) {
	configureBillingTest(t)
	f, err := billingStore.openLocked()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(billingStore.backupDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(billingStore.backupDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	server, mu, requests, state := newBillingTimerServer(t, false)
	setBillingTestPCs(PC{ID: "console-1", Name: "Console 1", Address: server.URL + "/1"})
	entry := pricingCatalog().Entries[0]
	mux := http.NewServeMux()
	registerBillingHandlers(mux)
	body := fmt.Sprintf(`{"pc_id":"console-1","category":%q,"customer_name":"Test Customer","players":%d,"duration_minutes":%d}`,
		entry.Category, entry.Players, entry.DurationMinutes)
	request := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	request.Header.Set("X-Billing-Token", "test-token")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "previous timer state was restored") {
		t.Fatalf("workbook failure was not reported with timer rollback: HTTP %d %s", response.Code, response.Body.String())
	}
	if sessions, err := billingStore.sessions("", ""); err != nil || len(sessions) != 0 {
		t.Fatalf("workbook failure created a session: sessions=%v err=%v", sessions, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if state.RemainingSeconds != 77 || state.IsRunning || state.TimerStatus != "paused" {
		t.Fatalf("previous timer state was not restored: %+v", state)
	}
	if !reflect.DeepEqual(*requests, []string{
		"GET /1/status", "POST /1/set", "POST /1/play", "POST /1/set", "POST /1/pause", "POST /1/hide",
	}) {
		t.Fatalf("unexpected timer rollback sequence: %v", *requests)
	}
}

func TestUnsupportedLegacyPaymentDataDoesNotReplaceWorkbook(t *testing.T) {
	directory := configureBillingTest(t)
	path := filepath.Join(directory, "billing.xlsx")
	sessionStart := time.Now().Format(time.RFC3339Nano)
	legacy := Session{ID: "S-orphan", StartedAt: sessionStart, CreatedAt: sessionStart}
	createLegacyBillingWorkbook(t, path, legacy)
	f, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRow(f, "Payments", 2, []string{"P-orphan", "missing-session", sessionStart, "5", "paid"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err = billingStore.openLocked()
	if err == nil {
		_ = f.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "no matching session") {
		t.Fatalf("expected explicit orphan-payment migration error, got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed migration modified the source workbook")
	}
}
