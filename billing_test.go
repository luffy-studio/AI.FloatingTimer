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
	oldEnabled, oldPath, oldBackup := billingStore.enabled, billingStore.path, billingStore.backupDir
	oldClient := timerHTTP
	billingSessionLinks.Lock()
	oldLinks := billingSessionLinks.items
	billingSessionLinks.items = make(map[string]billingSessionLink)
	billingSessionLinks.Unlock()
	store.Lock()
	oldPCs := store.PCs
	store.PCs = nil
	store.Unlock()
	billingStore.enabled = true
	billingStore.path = filepath.Join(directory, "billing.xlsx")
	billingStore.backupDir = filepath.Join(directory, "backups")
	timerHTTP = &http.Client{Timeout: 2 * time.Second}
	t.Cleanup(func() {
		billingStore.enabled, billingStore.path, billingStore.backupDir = oldEnabled, oldPath, oldBackup
		timerHTTP = oldClient
		billingSessionLinks.Lock()
		billingSessionLinks.items = oldLinks
		billingSessionLinks.Unlock()
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

func TestBillingAPIIsAccessibleWithoutCredentials(t *testing.T) {
	configureBillingTest(t)
	mux := http.NewServeMux()
	registerBillingHandlers(mux)
	request := httptest.NewRequest(http.MethodGet, "/api/billing/summary", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("billing summary without credentials returned HTTP %d: %s", response.Code, response.Body.String())
	}
}

func TestBillingSessionLinkHandlerScopesLinkToExactTimer(t *testing.T) {
	configureBillingTest(t)
	const timerAddress = "http://192.0.2.10:8080/1"
	mux := http.NewServeMux()
	setBillingSessionLinkHandler(mux, "/1/billing-session", timerAddress)
	link := billingSessionLink{
		SessionID:       "S-123",
		PCAddress:       timerAddress,
		ManagerEndpoint: "http://192.0.2.20:18082/internal/api/sessions/extend",
	}
	body, err := json.Marshal(link)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/1/billing-session", strings.NewReader(string(body)))
	request.RemoteAddr = "198.51.100.25:18082"
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("registering the link returned HTTP %d: %s", response.Code, response.Body.String())
	}
	if got, ok := billingSessionLinkForEndpoint(timerAddress); !ok || got.SessionID != link.SessionID {
		t.Fatalf("registered link was not stored for its exact endpoint: %+v", got)
	}

	link.SessionID = "S-stale"
	body, err = json.Marshal(link)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodDelete, "/1/billing-session", strings.NewReader(string(body)))
	request.RemoteAddr = "198.51.100.25:18082"
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("clearing a stale link returned HTTP %d: %s", response.Code, response.Body.String())
	}
	if got, ok := billingSessionLinkForEndpoint(timerAddress); !ok || got.SessionID != "S-123" {
		t.Fatalf("stale cleanup cleared another session's link: %+v", got)
	}
}

func TestOverlayExtensionUsesItsRegisteredBillingSession(t *testing.T) {
	configureBillingTest(t)
	var got ExtendSessionRequest
	manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/api/sessions/extend" {
			t.Errorf("unexpected billing callback: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode billing callback: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, ExtendSessionResponse{SessionFound: true, RemainingSeconds: 1800})
	}))
	defer manager.Close()

	localPC := localRegistryPC()
	setBillingTestPCs(localPC)
	billingStore.enabled = false
	billingSessionLinks.Lock()
	billingSessionLinks.items[normalizeSessionAddress(localPC.Address)] = billingSessionLink{
		SessionID:       "S-overlay",
		PCAddress:       localPC.Address,
		ManagerEndpoint: manager.URL + "/internal/api/sessions/extend",
	}
	billingSessionLinks.Unlock()

	handled, remaining, err := notifyManagerExtension(15 * 60)
	if err != nil {
		t.Fatalf("overlay extension failed: %v", err)
	}
	if !handled || remaining != 1800 {
		t.Fatalf("overlay extension was not confirmed by its billing manager: handled=%v remaining=%d", handled, remaining)
	}
	if got.SessionID != "S-overlay" || got.PCAddress != localPC.Address || got.PCID != localPC.ID || got.Seconds != 15*60 || got.RequestID == "" {
		t.Fatalf("overlay sent an incorrect session extension: %+v", got)
	}
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
		if r.URL.Path == "/1/billing-session" {
			var link billingSessionLink
			if err := json.NewDecoder(r.Body).Decode(&link); err != nil {
				http.Error(w, "invalid billing session link", http.StatusBadRequest)
				return
			}
			key := normalizeSessionAddress(link.PCAddress)
			billingSessionLinks.Lock()
			if r.Method == http.MethodPost {
				billingSessionLinks.items[key] = link
			} else if r.Method == http.MethodDelete {
				delete(billingSessionLinks.items, key)
			} else {
				billingSessionLinks.Unlock()
				http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
				return
			}
			billingSessionLinks.Unlock()
			writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
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
		case "/1/tweak":
			var body struct {
				Amount int `json:"amount"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid tweak request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			state.RemainingSeconds += body.Amount
			remaining := state.RemainingSeconds
			mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "remaining_seconds": remaining})
			return
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
	if !reflect.DeepEqual(gotRequests, []string{"GET /1/status", "POST /1/set", "POST /1/play", "POST /1/billing-session"}) {
		t.Fatalf("unexpected timer requests (must target only /1): %v", gotRequests)
	}
	link, linked := billingSessionLinkForEndpoint(server.URL + "/1")
	if !linked || link.SessionID != started.ID || link.PCAddress != server.URL+"/1" {
		t.Fatalf("selected timer was not linked to its exact billing session: %+v", link)
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
		"GET /1/status", "POST /1/set", "POST /1/play", "POST /1/billing-session", "DELETE /1/billing-session",
		"POST /1/set", "POST /1/pause", "POST /1/hide",
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

func TestSessionExtensionUpdatesBillingAndTimer(t *testing.T) {
	configureBillingTest(t)
	server, mu, requests, state := newBillingTimerServer(t, false)
	setBillingTestPCs(PC{ID: "pc-3", Name: "PC3", Address: server.URL + "/1"})

	mux := http.NewServeMux()
	registerBillingHandlers(mux)

	// 1. Start a session for PC Lite, 1 player, 30 min (₹30)
	body := `{"pc_id":"pc-3","category":"PC Lite","customer_name":"Alice","players":1,"duration_minutes":30}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got HTTP %d: %s", resp.Code, resp.Body.String())
	}

	mu.Lock()
	if state.RemainingSeconds != 1800 || !state.IsRunning {
		t.Fatalf("timer was not started with 1800s: %+v", state)
	}
	mu.Unlock()

	// Verify initial session in workbook
	sessions, err := billingStore.sessions("", "")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %v (err: %v)", sessions, err)
	}
	if sessions[0].PurchasedMinutes != 30 || sessions[0].AmountPaid != 30 {
		t.Fatalf("unexpected initial billing: %+v", sessions[0])
	}

	// 2. Extend the session by 15 minutes (lookup price for PC Lite, 1 player, 15 min is ₹15 -> total ₹45)
	extBody := `{"pc_id":"pc-3","minutes":15,"request_id":"req-ext-1"}`
	extReq := httptest.NewRequest(http.MethodPost, "/api/sessions/extend", strings.NewReader(extBody))
	extResp := httptest.NewRecorder()
	mux.ServeHTTP(extResp, extReq)
	if extResp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on extend, got HTTP %d: %s", extResp.Code, extResp.Body.String())
	}

	var extResult ExtendSessionResponse
	if err := json.Unmarshal(extResp.Body.Bytes(), &extResult); err != nil {
		t.Fatal(err)
	}
	if !extResult.SessionFound || extResult.Session == nil {
		t.Fatalf("expected session_found=true, got %+v", extResult)
	}
	if extResult.Session.PurchasedMinutes != 45 || extResult.Session.AmountPaid != 45 {
		t.Fatalf("expected 45 purchased minutes and 45 amount paid, got minutes=%d amount=%d",
			extResult.Session.PurchasedMinutes, extResult.Session.AmountPaid)
	}

	// Verify overlay timer was tweaked by 900s
	mu.Lock()
	if state.RemainingSeconds != 1800+900 {
		t.Fatalf("timer was not extended: remaining=%d", state.RemainingSeconds)
	}
	mu.Unlock()

	// Verify exactly 1 row remains in workbook (no duplicate row created)
	sessionsAfter, err := billingStore.sessions("", "")
	if err != nil || len(sessionsAfter) != 1 {
		t.Fatalf("expected exactly 1 row in workbook, got %d rows", len(sessionsAfter))
	}
	if sessionsAfter[0].PurchasedMinutes != 45 || sessionsAfter[0].AmountPaid != 45 {
		t.Fatalf("workbook row did not update: %+v", sessionsAfter[0])
	}

	// 3. Retry with same request_id -> must be idempotent, no extra minutes added
	extReq2 := httptest.NewRequest(http.MethodPost, "/api/sessions/extend", strings.NewReader(extBody))
	extResp2 := httptest.NewRecorder()
	mux.ServeHTTP(extResp2, extReq2)
	if extResp2.Code != http.StatusOK {
		t.Fatalf("idempotent retry failed: %d %s", extResp2.Code, extResp2.Body.String())
	}
	sessionsAfterRetry, _ := billingStore.sessions("", "")
	if sessionsAfterRetry[0].PurchasedMinutes != 45 || sessionsAfterRetry[0].AmountPaid != 45 {
		t.Fatalf("retry modified session: %+v", sessionsAfterRetry[0])
	}

	// Verify requests sent to timer
	if !reflect.DeepEqual(*requests, []string{
		"GET /1/status", "POST /1/set", "POST /1/play", "POST /1/billing-session", "POST /1/tweak", "GET /1/status",
	}) {
		t.Fatalf("unexpected timer requests: %v", *requests)
	}
}

func TestConcurrentSessionsAndExtensionsStayIsolatedByEndpoint(t *testing.T) {
	configureBillingTest(t)
	serverA, muA, requestsA, stateA := newBillingTimerServer(t, false)
	serverB, muB, _, stateB := newBillingTimerServer(t, false)
	setBillingTestPCs(
		PC{ID: "pc-a", Name: "PC A", Address: serverA.URL + "/1"},
		PC{ID: "pc-b", Name: "PC B", Address: serverB.URL + "/1"},
	)

	type startResult struct {
		session Session
		err     error
	}
	results := make(chan startResult, 2)
	go func() {
		session, err := startBillingSession(StartSessionRequest{
			PCID: "pc-a", Category: "PC Lite", CustomerName: "Customer A", Players: 1, DurationMinutes: 30,
		})
		results <- startResult{session: session, err: err}
	}()
	go func() {
		session, err := startBillingSession(StartSessionRequest{
			PCID: "pc-b", Category: "PC Pro", CustomerName: "Customer B", Players: 2, DurationMinutes: 60,
		})
		results <- startResult{session: session, err: err}
	}()
	started := make([]Session, 0, 2)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		started = append(started, result.session)
	}
	if started[0].ID == started[1].ID {
		t.Fatalf("simultaneous sessions received the same ID: %q", started[0].ID)
	}

	requestA := ExtendSessionRequest{
		PCID: "pc-a", PCAddress: serverA.URL + "/1", Seconds: 15 * 60, RequestID: "stable-extension-a",
	}
	requestB := ExtendSessionRequest{
		PCID: "pc-b", PCAddress: serverB.URL + "/1", Seconds: 15 * 60, RequestID: "stable-extension-b",
	}
	extensionResults := make(chan struct {
		result ExtendSessionResponse
		err    error
	}, 2)
	go func() {
		result, err := extendSessionForSystem(requestA, true)
		extensionResults <- struct {
			result ExtendSessionResponse
			err    error
		}{result, err}
	}()
	go func() {
		result, err := extendSessionForSystem(requestB, true)
		extensionResults <- struct {
			result ExtendSessionResponse
			err    error
		}{result, err}
	}()
	extended := make([]ExtendSessionResponse, 0, 2)
	for range 2 {
		outcome := <-extensionResults
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		extended = append(extended, outcome.result)
	}
	for _, result := range extended {
		if !result.SessionFound || result.Session == nil {
			t.Fatalf("extension did not resolve a started session: %+v", result)
		}
		if (result.Session.PCID == "pc-a" && result.Session.PurchasedMinutes != 45) ||
			(result.Session.PCID == "pc-b" && result.Session.PurchasedMinutes != 75) {
			t.Fatalf("extension updated the wrong session or duration: %+v", result.Session)
		}
	}
	muA.Lock()
	if stateA.RemainingSeconds != 2700 {
		t.Fatalf("PC A timer did not increase by exactly 15 minutes: %+v", stateA)
	}
	requestsAfterExtension := len(*requestsA)
	muA.Unlock()
	muB.Lock()
	if stateB.RemainingSeconds != 4500 {
		t.Fatalf("PC B timer was not extended independently: %+v", stateB)
	}
	muB.Unlock()

	retried, err := extendSessionForSystem(requestA, true)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Session == nil || retried.Session.PurchasedMinutes != 45 {
		t.Fatalf("retry changed the extension result: %+v", retried)
	}
	muA.Lock()
	if stateA.RemainingSeconds != 2700 || len(*requestsA) != requestsAfterExtension+1 {
		t.Fatalf("retry changed timer or failed to use the persisted idempotency key: state=%+v requests=%v", stateA, *requestsA)
	}
	muA.Unlock()

	sessions, err := billingStore.sessions("", "")
	if err != nil || len(sessions) != 2 {
		t.Fatalf("expected two workbook sessions without duplicate rows; got %d, err=%v", len(sessions), err)
	}
	for _, session := range sessions {
		if session.PCID == "pc-b" && (session.PurchasedMinutes != 75 || session.AmountPaid != 115) {
			t.Fatalf("PC B session has an unexpected independent extension: %+v", session)
		}
	}
}

func TestSessionExtensionWorkbookFailureRollsBackTimer(t *testing.T) {
	configureBillingTest(t)
	server, mu, _, state := newBillingTimerServer(t, false)
	setBillingTestPCs(PC{ID: "pc-fail", Name: "PC failure", Address: server.URL + "/1"})
	session, err := startBillingSession(StartSessionRequest{
		PCID: "pc-fail", Category: "PC Lite", CustomerName: "Customer", Players: 1, DurationMinutes: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(billingStore.backupDir), "not-a-directory"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	billingStore.backupDir = filepath.Join(filepath.Dir(billingStore.backupDir), "not-a-directory")
	_, err = extendSessionForSystem(ExtendSessionRequest{
		SessionID: session.ID, PCID: session.PCID, PCAddress: session.PCAddress,
		Minutes: 15, RequestID: "workbook-failure",
	}, true)
	if err == nil || !strings.Contains(err.Error(), "could not update Excel billing record") {
		t.Fatalf("expected explicit Excel write failure, got %v", err)
	}
	mu.Lock()
	if state.RemainingSeconds != 1800 {
		t.Fatalf("timer was not rolled back after workbook failure: %+v", state)
	}
	mu.Unlock()
	sessions, readErr := billingStore.sessions("", "")
	if readErr != nil || len(sessions) != 1 || sessions[0].PurchasedMinutes != 30 {
		t.Fatalf("failed extension modified the workbook: sessions=%+v err=%v", sessions, readErr)
	}
}

func TestSessionExtensionNoActiveSession(t *testing.T) {
	configureBillingTest(t)
	setBillingTestPCs(PC{ID: "pc-unbilled", Name: "Unbilled PC", Address: "http://127.0.0.1:55555"})

	mux := http.NewServeMux()
	registerBillingHandlers(mux)

	extBody := `{"pc_id":"pc-unbilled","minutes":15,"request_id":"req-unbilled-1"}`
	extReq := httptest.NewRequest(http.MethodPost, "/api/sessions/extend", strings.NewReader(extBody))
	extResp := httptest.NewRecorder()
	mux.ServeHTTP(extResp, extReq)
	if extResp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got HTTP %d: %s", extResp.Code, extResp.Body.String())
	}

	var extResult ExtendSessionResponse
	if err := json.Unmarshal(extResp.Body.Bytes(), &extResult); err != nil {
		t.Fatal(err)
	}
	if extResult.SessionFound {
		t.Fatalf("expected session_found=false for unbilled PC, got true")
	}

	// Ensure no fabricated session created in workbook
	sessions, err := billingStore.sessions("", "")
	if err != nil || len(sessions) != 0 {
		t.Fatalf("fabricated session was created: %v", sessions)
	}
}

func TestInternalOverlayExtensionNeedsNoAuthentication(t *testing.T) {
	configureBillingTest(t)
	server, mu, _, state := newBillingTimerServer(t, false)
	pc := PC{ID: "registered-pc", Name: "Registered PC", Address: server.URL + "/1"}
	setBillingTestPCs(pc)
	session, err := startBillingSession(StartSessionRequest{
		PCID: pc.ID, Category: "PC Lite", CustomerName: "Overlay customer", Players: 1, DurationMinutes: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBillingHandlers(mux)
	body, err := json.Marshal(ExtendSessionRequest{
		SessionID: session.ID, PCID: "overlay-local-id", PCAddress: pc.Address, Seconds: 15 * 60, RequestID: "overlay-extension",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/api/sessions/extend", strings.NewReader(string(body)))
	req.RemoteAddr = "127.0.0.1:54321"
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("registered overlay extension rejected: HTTP %d: %s", response.Code, response.Body.String())
	}
	mu.Lock()
	if state.RemainingSeconds != 2700 {
		t.Fatalf("overlay timer did not increase exactly 15 minutes: %+v", state)
	}
	mu.Unlock()

	req = httptest.NewRequest(http.MethodPost, "/internal/api/sessions/extend", strings.NewReader(string(body)))
	req.RemoteAddr = "198.51.100.25:54321"
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("extension without an authentication credential returned HTTP %d: %s", response.Code, response.Body.String())
	}
	mu.Lock()
	if state.RemainingSeconds != 2700 {
		t.Fatalf("idempotent retry changed timer state: %+v", state)
	}
	mu.Unlock()
}
