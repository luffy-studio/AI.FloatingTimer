//go:build windows

package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xuri/excelize/v2"
)

const billingWorkbookName = "billing.xlsx"

type BillingConfig struct {
	Enabled         bool   `json:"enabled"`
	WorkbookPath    string `json:"workbook_path"`
	BackupDirectory string `json:"backup_directory"`
	AdminToken      string `json:"admin_token"`
}
type Session struct {
	ID                string `json:"id"`
	PCID              string `json:"pc_id"`
	PCName            string `json:"pc_name"`
	PCAddress         string `json:"pc_address"`
	Category          string `json:"category"`
	CustomerName      string `json:"customer_name"`
	Players           int    `json:"players"`
	Status            string `json:"status"`
	StartedAt         string `json:"started_at"`
	ExpectedEndAt     string `json:"expected_end_at"`
	EndedAt           string `json:"ended_at,omitempty"`
	PurchasedMinutes  int    `json:"purchased_minutes"`
	AdditionalMinutes int    `json:"additional_minutes"`
	PricingSnapshot   string `json:"pricing_snapshot"`
	EstimatedAmount   int    `json:"estimated_amount"`
	FinalAmount       int    `json:"final_amount"`
	AmountPaid        int    `json:"amount_paid"`
	PaymentStatus     string `json:"payment_status"`
	PaymentAt         string `json:"payment_at,omitempty"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
	payments          []paymentRecord
}
type paymentRecord struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Amount    int    `json:"amount"`
	Status    string `json:"status"`
}
type sessionMetadata struct {
	Version  int             `json:"version"`
	Session  Session         `json:"session"`
	Payments []paymentRecord `json:"payments,omitempty"`
}
type BillingSummary struct {
	Enabled             bool   `json:"enabled"`
	TodayCollected      int    `json:"today_collected"`
	OpenSessions        int    `json:"open_sessions"`
	ConfigurationStatus string `json:"configuration_status"`
}
type StartSessionRequest struct {
	PCID            string `json:"pc_id"`
	Category        string `json:"category"`
	CustomerName    string `json:"customer_name"`
	Players         int    `json:"players"`
	DurationMinutes int    `json:"duration_minutes"`
	StartedAt       string `json:"started_at"`
}
type BillingStore struct {
	mu                     sync.Mutex
	enabled                bool
	path, backupDir, token string
}

var billingStore BillingStore
var sessionHeaders = []string{"Date", "Time", "Customer", "System", "Players", "Start Time", "Purchased Minutes", "Amount Paid"}
var legacySessionHeaders = []string{"Session ID", "System ID", "System", "Address", "Category", "Customer", "Players", "Status", "Start", "Expected End", "End", "Purchased Minutes", "Additional Minutes", "Pricing Snapshot", "Estimated Amount", "Final Amount", "Amount Paid", "Payment Status", "Payment At", "Created At", "Updated At"}
var paymentHeaders = []string{"Payment ID", "Session ID", "Timestamp", "Amount", "Payment Status"}

func normalizeBillingConfig() {
	appConfig.Billing.WorkbookPath = strings.TrimSpace(appConfig.Billing.WorkbookPath)
	appConfig.Billing.BackupDirectory = strings.TrimSpace(appConfig.Billing.BackupDirectory)
	if appConfig.Billing.WorkbookPath != "" && !filepath.IsAbs(appConfig.Billing.WorkbookPath) {
		appConfig.Billing.WorkbookPath = appDataPath(appConfig.Billing.WorkbookPath)
	}
	if appConfig.Billing.BackupDirectory != "" && !filepath.IsAbs(appConfig.Billing.BackupDirectory) {
		appConfig.Billing.BackupDirectory = appDataPath(appConfig.Billing.BackupDirectory)
	}
}
func (s *BillingStore) open(c BillingConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = c.Enabled
	s.token = c.AdminToken
	if !c.Enabled {
		return nil
	}
	s.path = c.WorkbookPath
	if s.path == "" {
		s.path = appDataPath(billingWorkbookName)
	}
	s.backupDir = c.BackupDirectory
	if s.backupDir == "" {
		s.backupDir = filepath.Join(filepath.Dir(s.path), "billing-backups")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	f, err := s.openLocked()
	if err != nil {
		return err
	}
	return f.Close()
}
func (s *BillingStore) close() {}
func (s *BillingStore) openLocked() (*excelize.File, error) {
	if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
		f := excelize.NewFile()
		f.SetSheetName("Sheet1", "Sessions")
		if err := writeRow(f, "Sessions", 1, sessionHeaders); err != nil {
			return nil, err
		}
		if err := formatSessionsSheet(f, "Sessions", 1); err != nil {
			return nil, err
		}
		if err := s.saveLocked(f); err != nil {
			_ = f.Close()
			return nil, err
		}
		return f, nil
	} else if err != nil {
		return nil, fmt.Errorf("cannot access billing workbook: %w", err)
	}
	f, err := excelize.OpenFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("cannot open billing workbook; it may be locked or corrupt: %w", err)
	}
	if err := s.ensureSessionSchemaLocked(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (s *BillingStore) ensureSessionSchemaLocked(f *excelize.File) error {
	index, err := f.GetSheetIndex("Sessions")
	if err != nil {
		return err
	}
	if index == -1 {
		index, err = f.NewSheet("Sessions")
		if err != nil {
			return err
		}
		if index == -1 {
			return errors.New("could not create Sessions worksheet")
		}
		if err := writeRow(f, "Sessions", 1, sessionHeaders); err != nil {
			return err
		}
		if err := formatSessionsSheet(f, "Sessions", 1); err != nil {
			return err
		}
		return s.saveLocked(f)
	}

	rows, err := f.GetRows("Sessions")
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if err := writeRow(f, "Sessions", 1, sessionHeaders); err != nil {
			return err
		}
		if err := formatSessionsSheet(f, "Sessions", 1); err != nil {
			return err
		}
		return s.saveLocked(f)
	}
	switch {
	case equalStrings(rows[0], sessionHeaders):
		return nil
	case equalStrings(rows[0], legacySessionHeaders):
		return s.migrateLegacyWorkbookLocked(f, rows)
	default:
		return fmt.Errorf("billing workbook has an unsupported Sessions header; no data was changed")
	}
}

func (s *BillingStore) migrateLegacyWorkbookLocked(f *excelize.File, rows [][]string) error {
	sessions := make([]Session, 0, len(rows)-1)
	byID := make(map[string]int, len(rows)-1)
	for rowIndex, row := range rows[1:] {
		x := parseSession(row)
		if x.ID == "" {
			if strings.Join(row, "") != "" {
				return fmt.Errorf("legacy billing row %d has no session ID; workbook was not changed", rowIndex+2)
			}
			continue
		}
		if _, exists := byID[x.ID]; exists {
			return fmt.Errorf("legacy billing workbook contains duplicate session ID %q; workbook was not changed", x.ID)
		}
		byID[x.ID] = len(sessions)
		sessions = append(sessions, x)
	}

	if paymentIndex, err := f.GetSheetIndex("Payments"); err != nil {
		return err
	} else if paymentIndex != -1 {
		paymentRows, err := f.GetRows("Payments")
		if err != nil {
			return err
		}
		if len(paymentRows) > 0 && !equalStrings(paymentRows[0], paymentHeaders) {
			return errors.New("legacy billing workbook has an unsupported Payments header; workbook was not changed")
		}
		for rowIndex, row := range paymentRows[1:] {
			get := func(i int) string {
				if i < len(row) {
					return row[i]
				}
				return ""
			}
			sessionIndex, exists := byID[get(1)]
			if !exists {
				if strings.Join(row, "") == "" {
					continue
				}
				return fmt.Errorf("legacy payment row %d has no matching session; workbook was not changed", rowIndex+2)
			}
			amount, err := strconv.Atoi(get(3))
			if err != nil {
				return fmt.Errorf("legacy payment row %d has an invalid amount; workbook was not changed", rowIndex+2)
			}
			sessions[sessionIndex].payments = append(sessions[sessionIndex].payments, paymentRecord{
				ID: get(0), Timestamp: get(2), Amount: amount, Status: get(4),
			})
		}
	}

	const replacementSheet = "_BillingSessions"
	if index, err := f.GetSheetIndex(replacementSheet); err != nil {
		return err
	} else if index != -1 {
		return errors.New("billing workbook contains a reserved migration worksheet; workbook was not changed")
	}
	if _, err := f.NewSheet(replacementSheet); err != nil {
		return fmt.Errorf("could not create replacement Sessions worksheet: %w", err)
	}
	if err := writeRow(f, replacementSheet, 1, sessionHeaders); err != nil {
		return err
	}
	if err := formatSessionsSheet(f, replacementSheet, len(sessions)+1); err != nil {
		return err
	}
	for i, session := range sessions {
		if err := writeSessionRow(f, replacementSheet, i+2, session, false); err != nil {
			return fmt.Errorf("could not migrate legacy session %q: %w", session.ID, err)
		}
	}
	if err := f.DeleteSheet("Sessions"); err != nil {
		return fmt.Errorf("could not replace legacy Sessions worksheet: %w", err)
	}
	if err := f.SetSheetName(replacementSheet, "Sessions"); err != nil {
		return err
	}
	for _, sheet := range []string{"Payments", "Daily Summary"} {
		index, err := f.GetSheetIndex(sheet)
		if err != nil {
			return err
		}
		if index != -1 {
			if err := f.DeleteSheet(sheet); err != nil {
				return fmt.Errorf("could not remove migrated %s worksheet: %w", sheet, err)
			}
		}
	}
	index, err := f.GetSheetIndex("Sessions")
	if err != nil {
		return err
	}
	f.SetActiveSheet(index)
	if err := s.saveLocked(f); err != nil {
		return fmt.Errorf("could not save migrated billing workbook: %w", err)
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func formatSessionsSheet(f *excelize.File, sheet string, rows int) error {
	header, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"20344A"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center"},
	})
	if err != nil {
		return err
	}
	if err := f.SetCellStyle(sheet, "A1", "H1", header); err != nil {
		return err
	}
	widths := []float64{14, 12, 24, 22, 10, 14, 20, 16}
	for i, width := range widths {
		if err := f.SetColWidth(sheet, column(i+1), column(i+1), width); err != nil {
			return err
		}
	}
	if rows > 1 {
		return formatSessionDataRows(f, sheet, 2, rows)
	}
	return nil
}

func formatSessionDataRows(f *excelize.File, sheet string, firstRow, lastRow int) error {
	dateFormat := "dd-mm-yyyy"
	timeFormat := "hh:mm:ss"
	amountFormat := `"₹"#,##0`
	dateStyle, err := f.NewStyle(&excelize.Style{CustomNumFmt: &dateFormat, Alignment: &excelize.Alignment{Horizontal: "center"}})
	if err != nil {
		return err
	}
	timeStyle, err := f.NewStyle(&excelize.Style{CustomNumFmt: &timeFormat, Alignment: &excelize.Alignment{Horizontal: "center"}})
	if err != nil {
		return err
	}
	amountStyle, err := f.NewStyle(&excelize.Style{CustomNumFmt: &amountFormat, Alignment: &excelize.Alignment{Horizontal: "right"}})
	if err != nil {
		return err
	}
	first, last := strconv.Itoa(firstRow), strconv.Itoa(lastRow)
	for _, item := range []struct {
		start, end string
		style      int
	}{
		{"A" + first, "A" + last, dateStyle},
		{"B" + first, "B" + last, timeStyle},
		{"F" + first, "F" + last, timeStyle},
		{"H" + first, "H" + last, amountStyle},
	} {
		if err := f.SetCellStyle(sheet, item.start, item.end, item.style); err != nil {
			return err
		}
	}
	return nil
}

func writeRow(f *excelize.File, sheet string, row int, values []string) error {
	for i, v := range values {
		if err := f.SetCellValue(sheet, fmt.Sprintf("%s%d", column(i+1), row), v); err != nil {
			return err
		}
	}
	return nil
}
func column(n int) string {
	v := ""
	for n > 0 {
		n--
		v = string(rune('A'+n%26)) + v
		n /= 26
	}
	return v
}
func (s *BillingStore) saveLocked(f *excelize.File) error {
	if err := os.MkdirAll(s.backupDir, 0755); err != nil {
		return err
	}
	if _, err := os.Stat(s.path); err == nil {
		b := filepath.Join(s.backupDir, "billing-"+time.Now().Format("20060102-150405.000000000")+".xlsx")
		source, err := os.Open(s.path)
		if err != nil {
			return fmt.Errorf("could not open workbook for backup: %w", err)
		}
		backup, err := os.OpenFile(b, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			_ = source.Close()
			return fmt.Errorf("could not create workbook backup: %w", err)
		}
		_, copyErr := io.Copy(backup, source)
		sourceCloseErr := source.Close()
		backupCloseErr := backup.Close()
		if err := errors.Join(copyErr, sourceCloseErr, backupCloseErr); err != nil {
			return fmt.Errorf("could not finish workbook backup: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("could not inspect billing workbook before saving: %w", err)
	}
	tmp := s.path + ".tmp.xlsx"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("could not remove stale temporary workbook: %w", err)
	}
	if err := f.SaveAs(tmp); err != nil {
		return fmt.Errorf("could not save temporary workbook: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		if removeErr := os.Remove(tmp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("could not replace workbook: %v; temporary cleanup failed: %w", err, removeErr)
		}
		return fmt.Errorf("could not replace workbook (close it in Excel and retry): %w", err)
	}
	return nil
}
func sessionRows(f *excelize.File) ([][]string, error) { return f.GetRows("Sessions") }

func sessionTimestamp(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid session timestamp %q", value)
}

func excelDateSerial(value time.Time) float64 {
	local := value.In(time.Local)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	epoch := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	return date.Sub(epoch).Hours() / 24
}

func excelTimeFraction(value time.Time) float64 {
	local := value.In(time.Local)
	seconds := local.Hour()*3600 + local.Minute()*60 + local.Second()
	return float64(seconds) / 86400
}

func writeSessionRow(f *excelize.File, sheet string, row int, x Session, replaceExisting bool) error {
	created, err := sessionTimestamp(x.CreatedAt)
	if err != nil {
		return fmt.Errorf("session %q has no valid creation time: %w", x.ID, err)
	}
	started, err := sessionTimestamp(x.StartedAt)
	if err != nil {
		return fmt.Errorf("session %q has no valid start time: %w", x.ID, err)
	}
	values := []interface{}{
		excelDateSerial(created), excelTimeFraction(created), x.CustomerName, x.PCName,
		x.Players, excelTimeFraction(started), x.PurchasedMinutes + x.AdditionalMinutes, x.AmountPaid,
	}
	for i, value := range values {
		cell := fmt.Sprintf("%s%d", column(i+1), row)
		if err := f.SetCellValue(sheet, cell, value); err != nil {
			return err
		}
	}
	metadata, err := json.Marshal(sessionMetadata{Version: 1, Session: x, Payments: x.payments})
	if err != nil {
		return err
	}
	if len(metadata) > 32000 {
		return fmt.Errorf("session %q metadata exceeds Excel's comment limit", x.ID)
	}
	if replaceExisting {
		comments, err := f.GetComments(sheet)
		if err != nil {
			return err
		}
		for _, comment := range comments {
			if strings.ReplaceAll(comment.Cell, "$", "") == fmt.Sprintf("A%d", row) {
				if err := f.DeleteComment(sheet, comment.Cell); err != nil {
					return err
				}
			}
		}
	}
	if err := f.AddComment(sheet, excelize.Comment{
		Cell: fmt.Sprintf("A%d", row), Author: "OverlayTimer", Text: string(metadata),
	}); err != nil {
		return err
	}
	return nil
}

func sessionsFromWorkbook(f *excelize.File) ([]Session, error) {
	stored, err := storedSessionsFromWorkbook(f)
	if err != nil {
		return nil, err
	}
	result := make([]Session, 0, len(stored))
	for _, item := range stored {
		result = append(result, item.Session)
	}
	return result, nil
}

type storedBillingSession struct {
	Session Session
	Row     int
}

func storedSessionsFromWorkbook(f *excelize.File) ([]storedBillingSession, error) {
	rows, err := sessionRows(f)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || !equalStrings(rows[0], sessionHeaders) {
		return nil, errors.New("billing workbook has an invalid Sessions header")
	}
	comments, err := f.GetComments("Sessions")
	if err != nil {
		return nil, err
	}
	metadataByRow := make(map[int]sessionMetadata, len(comments))
	for _, comment := range comments {
		cell := strings.ReplaceAll(comment.Cell, "$", "")
		if len(cell) < 2 || cell[0] != 'A' {
			continue
		}
		row, err := strconv.Atoi(cell[1:])
		if err != nil || row < 2 {
			continue
		}
		var metadata sessionMetadata
		if err := json.Unmarshal([]byte(comment.Text), &metadata); err != nil {
			return nil, fmt.Errorf("billing metadata in A%d is invalid: %w", row, err)
		}
		if metadata.Version != 1 || metadata.Session.ID == "" {
			return nil, fmt.Errorf("billing metadata in A%d is unsupported", row)
		}
		if _, exists := metadataByRow[row]; exists {
			return nil, fmt.Errorf("billing row %d has duplicate session metadata", row)
		}
		metadata.Session.payments = metadata.Payments
		metadataByRow[row] = metadata
	}
	result := make([]storedBillingSession, 0, len(rows)-1)
	seenRows := make(map[int]struct{}, len(rows)-1)
	for rowIndex, row := range rows[1:] {
		if len(row) == 0 || strings.Join(row, "") == "" {
			continue
		}
		rowNumber := rowIndex + 2
		metadata, exists := metadataByRow[rowNumber]
		if !exists {
			return nil, fmt.Errorf("billing row %d is missing its session metadata; no data was changed", rowNumber)
		}
		seenRows[rowNumber] = struct{}{}
		result = append(result, storedBillingSession{Session: metadata.Session, Row: rowNumber})
	}
	for rowNumber := range metadataByRow {
		if _, exists := seenRows[rowNumber]; !exists {
			return nil, fmt.Errorf("billing metadata in A%d has no session row; no data was changed", rowNumber)
		}
	}
	return result, nil
}
func parseSession(r []string) Session {
	get := func(i int) string {
		if i < len(r) {
			return r[i]
		}
		return ""
	}
	num := func(i int) int { v, _ := strconv.Atoi(get(i)); return v }
	return Session{ID: get(0), PCID: get(1), PCName: get(2), PCAddress: get(3), Category: get(4), CustomerName: get(5), Players: num(6), Status: get(7), StartedAt: get(8), ExpectedEndAt: get(9), EndedAt: get(10), PurchasedMinutes: num(11), AdditionalMinutes: num(12), PricingSnapshot: get(13), EstimatedAmount: num(14), FinalAmount: num(15), AmountPaid: num(16), PaymentStatus: get(17), PaymentAt: get(18), CreatedAt: get(19), UpdatedAt: get(20)}
}
func rowFor(s Session) []string {
	return []string{s.ID, s.PCID, s.PCName, s.PCAddress, s.Category, s.CustomerName, strconv.Itoa(s.Players), s.Status, s.StartedAt, s.ExpectedEndAt, s.EndedAt, strconv.Itoa(s.PurchasedMinutes), strconv.Itoa(s.AdditionalMinutes), s.PricingSnapshot, strconv.Itoa(s.EstimatedAmount), strconv.Itoa(s.FinalAmount), strconv.Itoa(s.AmountPaid), s.PaymentStatus, s.PaymentAt, s.CreatedAt, s.UpdatedAt}
}
func (s *BillingStore) require() error {
	if !s.enabled {
		return errors.New("billing is disabled on this machine")
	}
	return nil
}
func (s *BillingStore) sessions(search, payment string) ([]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return nil, err
	}
	f, err := s.openLocked()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := sessionsFromWorkbook(f)
	if err != nil {
		return nil, err
	}
	var out []Session
	search = strings.ToLower(strings.TrimSpace(search))
	for _, x := range rows {
		if payment != "" && x.PaymentStatus != payment {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(x.CustomerName+" "+x.PCName+" "+x.ID), search) {
			continue
		}
		out = append(out, x)
	}
	return out, nil
}

func (s *BillingStore) prepareStart(q StartSessionRequest) (Session, PC, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, PC{}, err
	}
	pc, ok := store.find(q.PCID)
	if !ok {
		return Session{}, PC{}, errors.New("gaming system not found")
	}
	if pc.Inactive {
		return Session{}, PC{}, errors.New("gaming system is inactive")
	}
	q.CustomerName = strings.TrimSpace(q.CustomerName)
	if q.CustomerName == "" {
		return Session{}, PC{}, errors.New("Please enter the customer name before starting the session.")
	}
	if len(q.CustomerName) > 120 {
		return Session{}, PC{}, errors.New("customer name is too long")
	}
	if q.Players < 1 || q.Players > 4 {
		return Session{}, PC{}, errors.New("players must be between 1 and 4")
	}
	entry, err := lookupPrice(q.Category, q.Players, q.DurationMinutes)
	if err != nil {
		return Session{}, PC{}, err
	}
	f, err := s.openLocked()
	if err != nil {
		return Session{}, PC{}, apiError(http.StatusInternalServerError, err)
	}
	defer f.Close()
	rows, err := sessionsFromWorkbook(f)
	if err != nil {
		return Session{}, PC{}, apiError(http.StatusInternalServerError, err)
	}
	for _, x := range rows {
		if x.PCID == pc.ID && x.Status == "open" {
			return Session{}, PC{}, apiError(http.StatusConflict, errors.New("system already has an open session"))
		}
	}
	now := time.Now()
	x := Session{
		ID: "S-" + now.UTC().Format("20060102150405.000000000"), PCID: pc.ID,
		PCName: pc.Name, PCAddress: pc.Address, Category: q.Category,
		CustomerName: q.CustomerName, Players: q.Players, Status: "open",
		PurchasedMinutes: q.DurationMinutes, PricingSnapshot: pricingSnapshot(entry),
		EstimatedAmount: entry.Amount, FinalAmount: entry.Amount, AmountPaid: entry.Amount,
		PaymentStatus: "paid",
	}
	return x, pc, nil
}

func (s *BillingStore) commitStart(x Session, startedAt time.Time) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	f, err := s.openLocked()
	if err != nil {
		return Session{}, apiError(http.StatusInternalServerError, err)
	}
	defer f.Close()
	sessions, err := sessionsFromWorkbook(f)
	if err != nil {
		return Session{}, apiError(http.StatusInternalServerError, err)
	}
	for _, current := range sessions {
		if current.PCID == x.PCID && current.Status == "open" {
			return Session{}, apiError(http.StatusConflict, errors.New("system already has an open session"))
		}
	}
	x.CreatedAt = time.Now().Format(time.RFC3339Nano)
	x.UpdatedAt = x.CreatedAt
	x.PaymentAt = startedAt.Format(time.RFC3339Nano)
	x.StartedAt = x.PaymentAt
	x.ExpectedEndAt = startedAt.Add(time.Duration(x.PurchasedMinutes) * time.Minute).Format(time.RFC3339Nano)
	rows, err := sessionRows(f)
	if err != nil {
		return Session{}, apiError(http.StatusInternalServerError, err)
	}
	row := len(rows) + 1
	if err := writeSessionRow(f, "Sessions", row, x, false); err != nil {
		return Session{}, apiError(http.StatusInternalServerError, err)
	}
	if err := formatSessionDataRows(f, "Sessions", row, row); err != nil {
		return Session{}, apiError(http.StatusInternalServerError, err)
	}
	if err := s.saveLocked(f); err != nil {
		return Session{}, apiError(http.StatusInternalServerError, err)
	}
	return x, nil
}

type billingHTTPError struct {
	Status int
	Err    error
}

func (e *billingHTTPError) Error() string { return e.Err.Error() }
func (e *billingHTTPError) Unwrap() error { return e.Err }

func apiError(status int, err error) error {
	return &billingHTTPError{Status: status, Err: err}
}

var billingStartMu sync.Mutex

func startBillingSession(q StartSessionRequest) (Session, error) {
	billingStartMu.Lock()
	defer billingStartMu.Unlock()

	x, pc, err := billingStore.prepareStart(q)
	if err != nil {
		return Session{}, err
	}
	previous, err := getTimerStatus(pc.Address)
	if err != nil {
		return Session{}, apiError(http.StatusBadGateway, fmt.Errorf("could not read timer status for %s: %w", pc.Name, err))
	}
	restoreAfterFailure := func(cause error, status int) error {
		if restoreErr := restoreTimerStatus(pc.Address, previous); restoreErr != nil {
			logf("billing", "session start failed for pc_id=%s; timer rollback failed: %v", pc.ID, restoreErr)
			return apiError(http.StatusBadGateway, fmt.Errorf("%v; restoring the previous timer state also failed: %w", cause, restoreErr))
		}
		return apiError(status, fmt.Errorf("%v; previous timer state was restored", cause))
	}

	if err := timerAction(pc.Address, "set", x.PurchasedMinutes*60); err != nil {
		return Session{}, restoreAfterFailure(fmt.Errorf("could not set timer for %s: %w", pc.Name, err), http.StatusBadGateway)
	}
	startedAt := time.Now()
	if err := timerAction(pc.Address, "play", 0); err != nil {
		return Session{}, restoreAfterFailure(fmt.Errorf("could not start timer for %s: %w", pc.Name, err), http.StatusBadGateway)
	}
	x, err = billingStore.commitStart(x, startedAt)
	if err != nil {
		var statusErr *billingHTTPError
		if errors.As(err, &statusErr) {
			return Session{}, restoreAfterFailure(statusErr.Err, statusErr.Status)
		}
		return Session{}, restoreAfterFailure(err, http.StatusInternalServerError)
	}
	return x, nil
}

func timerEndpoint(address, action string) string {
	return strings.TrimRight(address, "/") + "/" + action
}

func getTimerStatus(address string) (TimerStatus, error) {
	request, err := http.NewRequest(http.MethodGet, timerEndpoint(address, "status"), nil)
	if err != nil {
		return TimerStatus{}, err
	}
	response, err := timerHTTP.Do(request)
	if err != nil {
		return TimerStatus{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return TimerStatus{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return TimerStatus{}, fmt.Errorf("status endpoint returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var status TimerStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return TimerStatus{}, fmt.Errorf("invalid timer status response: %w", err)
	}
	if status.TimerStatus != "running" && status.TimerStatus != "paused" {
		return TimerStatus{}, errors.New("timer status response is missing a valid timer state")
	}
	return status, nil
}

func timerAction(address, action string, seconds int) error {
	payload := []byte("{}")
	if action == "set" {
		payload = fmt.Appendf(nil, `{"seconds":%d}`, seconds)
	}
	request, err := http.NewRequest(http.MethodPost, timerEndpoint(address, action), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := timerHTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("timer endpoint returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var result struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("invalid timer response: %w", err)
	}
	if result.Status != "success" {
		if result.Message == "" {
			result.Message = "timer command was not accepted"
		}
		return errors.New(result.Message)
	}
	return nil
}

func restoreTimerStatus(address string, previous TimerStatus) error {
	actions := []struct {
		action  string
		seconds int
	}{
		{"set", previous.RemainingSeconds},
		{"pause", 0},
		{"hide", 0},
	}
	if previous.IsRunning {
		actions[1].action = "play"
	}
	if previous.TimeUpVisible {
		actions[2].action = "show"
	}
	var failures []error
	for _, item := range actions {
		if err := timerAction(address, item.action, item.seconds); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", item.action, err))
		}
	}
	return errors.Join(failures...)
}

func (s *BillingStore) summary() BillingSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := BillingSummary{Enabled: s.enabled}
	if !s.enabled {
		out.ConfigurationStatus = "billing disabled"
		return out
	}
	f, err := s.openLocked()
	if err != nil {
		out.ConfigurationStatus = err.Error()
		return out
	}
	defer f.Close()
	rows, err := sessionsFromWorkbook(f)
	if err != nil {
		out.ConfigurationStatus = err.Error()
		return out
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	for _, x := range rows {
		if x.Status == "open" {
			out.OpenSessions++
		}
		if x.PaymentAt != "" {
			paidAt, err := sessionTimestamp(x.PaymentAt)
			if err != nil {
				out.ConfigurationStatus = fmt.Sprintf("invalid payment timestamp for session %s", x.ID)
				return out
			}
			if paidAt.In(time.Local).Format("2006-01-02") == today {
				out.TodayCollected += x.AmountPaid
			}
		}
	}
	return out
}

func (s *BillingStore) update(id, action string, minutes, amount int) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	f, err := s.openLocked()
	if err != nil {
		return Session{}, err
	}
	defer f.Close()
	rows, err := storedSessionsFromWorkbook(f)
	if err != nil {
		return Session{}, err
	}
	for _, stored := range rows {
		x := stored.Session
		if x.ID != id {
			continue
		}
		now := time.Now().Format(time.RFC3339Nano)
		switch action {
		case "extend":
			if x.Status != "open" {
				return Session{}, errors.New("only open sessions can be extended")
			}
			entry, e := lookupPrice(x.Category, x.Players, minutes)
			if e != nil {
				return Session{}, e
			}
			x.AdditionalMinutes += minutes
			x.FinalAmount += entry.Amount
			x.EstimatedAmount = x.FinalAmount
			switch {
			case x.AmountPaid >= x.FinalAmount:
				x.PaymentStatus = "paid"
			case x.AmountPaid > 0:
				x.PaymentStatus = "partial"
			default:
				x.PaymentStatus = "unpaid"
			}
			start, e := sessionTimestamp(x.StartedAt)
			if e == nil {
				x.ExpectedEndAt = start.Add(time.Duration(x.PurchasedMinutes+x.AdditionalMinutes) * time.Minute).Format(time.RFC3339)
			}
		case "pay":
			if amount <= 0 {
				amount = x.FinalAmount - x.AmountPaid
			}
			if amount <= 0 {
				return Session{}, errors.New("no amount is due")
			}
			x.AmountPaid += amount
			x.PaymentAt = now
			if x.AmountPaid >= x.FinalAmount {
				x.PaymentStatus = "paid"
			} else {
				x.PaymentStatus = "partial"
			}
			x.payments = append(x.payments, paymentRecord{
				ID:        "P-" + time.Now().UTC().Format("20060102150405.000000000"),
				Timestamp: now, Amount: amount, Status: x.PaymentStatus,
			})
		case "close":
			if x.Status == "open" {
				x.Status = "closed"
				x.EndedAt = now
			}
		default:
			return Session{}, errors.New("unknown session action")
		}
		x.UpdatedAt = now
		if err := writeSessionRow(f, "Sessions", stored.Row, x, true); err != nil {
			return Session{}, err
		}
		if err := formatSessionDataRows(f, "Sessions", stored.Row, stored.Row); err != nil {
			return Session{}, err
		}
		if err := s.saveLocked(f); err != nil {
			return Session{}, err
		}
		return x, nil
	}
	return Session{}, errors.New("session not found")
}
func billingAuthorized(r *http.Request) bool {
	t := billingStore.token
	if t == "" {
		return r.RemoteAddr == "" || strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") || strings.HasPrefix(r.RemoteAddr, "[::1]:")
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Billing-Token")), []byte(t)) == 1
}
func billingOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !billingAuthorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "billing authorization required"})
			return
		}
		next(w, r)
	}
}
func registerBillingHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/pricing", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", 405)
			return
		}
		writeJSON(w, 200, pricingCatalog())
	})
	mux.HandleFunc("/api/billing/summary", billingOnly(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, billingStore.summary()) }))
	mux.HandleFunc("/api/billing/download", billingOnly(downloadBilling))
	mux.HandleFunc("/api/sessions", billingOnly(sessionsHandler))
	mux.HandleFunc("/api/sessions/", billingOnly(sessionActionHandler))
}
func sessionsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		x, err := billingStore.sessions(r.URL.Query().Get("search"), r.URL.Query().Get("payment_status"))
		if err != nil {
			writeBillingError(w, err)
			return
		}
		writeJSON(w, 200, x)
	case http.MethodPost:
		var q StartSessionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&q); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid session data"})
			return
		}
		x, err := startBillingSession(q)
		if err != nil {
			writeBillingError(w, err)
			return
		}
		writeJSON(w, 201, x)
	default:
		http.Error(w, "GET or POST required", 405)
	}
}

func sessionActionHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/"), "/")
	if len(parts) != 2 || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Minutes int `json:"minutes"`
		Amount  int `json:"amount"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid session update"})
		return
	}
	x, err := billingStore.update(parts[0], parts[1], body.Minutes, body.Amount)
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, 200, x)
}
func downloadBilling(w http.ResponseWriter, r *http.Request) {
	billingStore.mu.Lock()
	defer billingStore.mu.Unlock()
	if err := billingStore.require(); err != nil {
		writeBillingError(w, err)
		return
	}
	f, err := billingStore.openLocked()
	if err != nil {
		writeBillingError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", "attachment; filename=billing.xlsx")
	if _, err = f.WriteTo(w); err != nil {
		logf("billing", "workbook download failed: %v", err)
	}
}
func writeBillingError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var httpErr *billingHTTPError
	if errors.As(err, &httpErr) {
		status = httpErr.Status
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
