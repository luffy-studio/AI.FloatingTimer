//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

type ExtensionTransaction struct {
	ID                      string `json:"id"`
	SessionID               string `json:"session_id"`
	Timestamp               string `json:"timestamp"`
	PCID                    string `json:"pc_id"`
	PCName                  string `json:"pc_name"`
	CustomerName            string `json:"customer_name"`
	PreviousDurationMinutes int    `json:"previous_duration_minutes"`
	AddedMinutes            int    `json:"added_minutes"`
	UpdatedDurationMinutes  int    `json:"updated_duration_minutes"`
	PreviousBill            int    `json:"previous_bill"`
	Amount                  int    `json:"amount"`
	UpdatedBill             int    `json:"updated_bill"`
	RequestID               string `json:"request_id,omitempty"`
}

type PaymentTransaction struct {
	ID           string `json:"id"`
	RequestID    string `json:"request_id,omitempty"`
	SessionID    string `json:"session_id"`
	Timestamp    string `json:"timestamp"`
	Amount       int    `json:"amount"`
	Method       string `json:"method"`
	PCID         string `json:"pc_id"`
	PCName       string `json:"pc_name"`
	CustomerName string `json:"customer_name"`
}

type SnackMenuItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Category  string `json:"category"`
	UnitPrice int    `json:"unit_price"`
	Available bool   `json:"available"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type SnackOrder struct {
	ID           string `json:"id"`
	RequestID    string `json:"request_id,omitempty"`
	SessionID    string `json:"session_id"`
	Timestamp    string `json:"timestamp"`
	PCID         string `json:"pc_id"`
	PCName       string `json:"pc_name"`
	CustomerName string `json:"customer_name"`
	ItemID       string `json:"item_id"`
	ItemName     string `json:"item_name"`
	Category     string `json:"category"`
	Quantity     int    `json:"quantity"`
	UnitPrice    int    `json:"unit_price"`
	TotalAmount  int    `json:"total_amount"`
	Kind         string `json:"kind"`
}

type PaymentRequest struct {
	Amount    int    `json:"amount"`
	Method    string `json:"method"`
	RequestID string `json:"request_id,omitempty"`
}

type SnackOrderRequest struct {
	PCID      string `json:"pc_id,omitempty"`
	PCAddress string `json:"pc_address,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	ItemID    string `json:"item_id"`
	Quantity  int    `json:"quantity"`
	RequestID string `json:"request_id,omitempty"`
}

type SnackChargeRequest struct {
	Amount    int    `json:"amount"`
	RequestID string `json:"request_id,omitempty"`
}

type SnackChargeAdjustmentRequest struct {
	OrderID     string `json:"order_id"`
	AmountDelta int    `json:"amount_delta"`
	RequestID   string `json:"request_id,omitempty"`
}

type SnackAdjustmentRequest struct {
	ItemID        string `json:"item_id"`
	QuantityDelta int    `json:"quantity_delta"`
	RequestID     string `json:"request_id,omitempty"`
}

var snackCategories = map[string]struct{}{
	"Snacks": {}, "Cold Drinks": {}, "Water": {}, "Packaged Food": {}, "Other": {},
}

var (
	extensionSheetHeaders = []string{"Extension ID", "Request ID", "Session ID", "Timestamp", "PC", "Customer", "Previous Minutes", "Added Minutes", "Updated Minutes", "Previous Bill", "Amount", "Updated Bill"}
	ledgerPaymentHeaders  = []string{"Payment ID", "Request ID", "Session ID", "Timestamp", "Amount", "Method", "PC", "Customer"}
	snackOrderHeaders     = []string{"Order ID", "Request ID", "Session ID", "Timestamp", "PC", "Customer", "Item ID", "Item", "Category", "Quantity", "Unit Price", "Total", "Kind"}
	snackMenuHeaders      = []string{"Item ID", "Name", "Category", "Unit Price", "Available", "Created At", "Updated At"}
)

func ledgerNow() string { return time.Now().In(billingTimezone).Format(time.RFC3339Nano) }

func newLedgerID(prefix string) (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func normalizePaymentMethod(method string) (string, error) {
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" {
		return "cash", nil
	}
	switch method {
	case "cash":
		return "cash", nil
	case "online", "upi", "qr", "bank transfer", "bank_transfer":
		return "online", nil
	default:
		return "", errors.New("payment method must be Cash or Online")
	}
}

func normalizeSessionLedger(x *Session) {
	if x.GamingAmount == 0 {
		x.GamingAmount = x.PurchasedAmount + x.AdditionalAmount
		if x.GamingAmount == 0 && x.FinalAmount > 0 {
			x.GamingAmount = x.FinalAmount - x.SnackAmount
		}
	}
	if len(x.SnackOrders) > 0 {
		x.SnackAmount = 0
		for _, order := range x.SnackOrders {
			x.SnackAmount += order.TotalAmount
		}
	}
	if len(x.PaymentHistory) > 0 {
		x.AmountPaid, x.CashPaid, x.OnlinePaid = 0, 0, 0
		for _, payment := range x.PaymentHistory {
			x.AmountPaid += payment.Amount
			switch payment.Method {
			case "cash":
				x.CashPaid += payment.Amount
			case "online":
				x.OnlinePaid += payment.Amount
			}
		}
	}
	x.FinalAmount = x.GamingAmount + x.SnackAmount
	x.AmountDue = max(0, x.FinalAmount-x.AmountPaid)
	switch {
	case x.AmountPaid <= 0:
		x.PaymentStatus = "unpaid"
	case x.AmountDue > 0:
		x.PaymentStatus = "partial"
	default:
		x.PaymentStatus = "paid"
	}
}

// collectedChargeAllocation keeps reporting deterministic when a combined
// gaming + snack bill is paid in several installments. Payments are applied
// to gaming charges first, then to snacks; the underlying payment records are
// never rewritten.
func collectedChargeAllocation(x Session) (gaming, snacks int) {
	paid := max(0, x.AmountPaid)
	gaming = min(paid, max(0, x.GamingAmount))
	snacks = min(max(0, paid-gaming), max(0, x.SnackAmount))
	return gaming, snacks
}

func ensureLedgerSheet(f *excelize.File, name string, headers []string) (bool, error) {
	index, err := f.GetSheetIndex(name)
	if err != nil {
		return false, err
	}
	if index == -1 {
		if _, err := f.NewSheet(name); err != nil {
			return false, err
		}
		if err := writeRow(f, name, 1, headers); err != nil {
			return false, err
		}
		return true, nil
	}
	rows, err := f.GetRows(name)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		if err := writeRow(f, name, 1, headers); err != nil {
			return false, err
		}
		return true, nil
	}
	if !equalStrings(rows[0], headers) {
		return false, fmt.Errorf("billing workbook has an unsupported %s worksheet header", name)
	}
	return false, nil
}

func ensureLedgerSheetsLocked(f *excelize.File) (bool, error) {
	changed := false
	for _, item := range []struct {
		name    string
		headers []string
	}{
		{"Extensions", extensionSheetHeaders}, {"Payments", ledgerPaymentHeaders},
		{"Snacks", snackOrderHeaders}, {"Menu", snackMenuHeaders},
	} {
		created, err := ensureLedgerSheet(f, item.name, item.headers)
		if err != nil {
			return false, err
		}
		changed = changed || created
	}
	return changed, nil
}

func recreateLedgerSheet(f *excelize.File, name string, headers []string) error {
	if index, err := f.GetSheetIndex(name); err != nil {
		return err
	} else if index != -1 {
		if err := f.DeleteSheet(name); err != nil {
			return err
		}
	}
	if _, err := f.NewSheet(name); err != nil {
		return err
	}
	return writeRow(f, name, 1, headers)
}

func (s *BillingStore) refreshLedgerSheetsLocked(f *excelize.File) error {
	sessions, err := sessionsFromWorkbook(f)
	if err != nil {
		return err
	}
	if _, err := ensureLedgerSheet(f, "Menu", snackMenuHeaders); err != nil {
		return err
	}
	if err := recreateLedgerSheet(f, "Extensions", extensionSheetHeaders); err != nil {
		return err
	}
	if err := recreateLedgerSheet(f, "Payments", ledgerPaymentHeaders); err != nil {
		return err
	}
	if err := recreateLedgerSheet(f, "Snacks", snackOrderHeaders); err != nil {
		return err
	}
	extensionRow, paymentRow, snackRow := 2, 2, 2
	for _, session := range sessions {
		for _, extension := range session.Extensions {
			values := []interface{}{extension.ID, extension.RequestID, extension.SessionID, extension.Timestamp, extension.PCName, extension.CustomerName, extension.PreviousDurationMinutes, extension.AddedMinutes, extension.UpdatedDurationMinutes, extension.PreviousBill, extension.Amount, extension.UpdatedBill}
			if err := writeLedgerValues(f, "Extensions", extensionRow, values); err != nil {
				return err
			}
			extensionRow++
		}
		for _, payment := range session.PaymentHistory {
			values := []interface{}{payment.ID, payment.RequestID, payment.SessionID, payment.Timestamp, payment.Amount, payment.Method, payment.PCName, payment.CustomerName}
			if err := writeLedgerValues(f, "Payments", paymentRow, values); err != nil {
				return err
			}
			paymentRow++
		}
		for _, order := range session.SnackOrders {
			values := []interface{}{order.ID, order.RequestID, order.SessionID, order.Timestamp, order.PCName, order.CustomerName, order.ItemID, order.ItemName, order.Category, order.Quantity, order.UnitPrice, order.TotalAmount, order.Kind}
			if err := writeLedgerValues(f, "Snacks", snackRow, values); err != nil {
				return err
			}
			snackRow++
		}
	}
	return nil
}

func writeLedgerValues(f *excelize.File, sheet string, row int, values []interface{}) error {
	for index, value := range values {
		if err := f.SetCellValue(sheet, fmt.Sprintf("%s%d", column(index+1), row), value); err != nil {
			return err
		}
	}
	return nil
}

func (s *BillingStore) menuLocked(f *excelize.File) ([]SnackMenuItem, error) {
	rows, err := f.GetRows("Menu")
	if err != nil {
		return nil, err
	}
	items := make([]SnackMenuItem, 0, max(0, len(rows)-1))
	for _, row := range rows[1:] {
		get := func(index int) string {
			if index < len(row) {
				return row[index]
			}
			return ""
		}
		price, _ := strconv.Atoi(get(3))
		available, _ := strconv.ParseBool(get(4))
		items = append(items, SnackMenuItem{ID: get(0), Name: get(1), Category: get(2), UnitPrice: price, Available: available, CreatedAt: get(5), UpdatedAt: get(6)})
	}
	return items, nil
}

func writeMenuLocked(f *excelize.File, items []SnackMenuItem) error {
	if err := recreateLedgerSheet(f, "Menu", snackMenuHeaders); err != nil {
		return err
	}
	for row, item := range items {
		if err := writeLedgerValues(f, "Menu", row+2, []interface{}{item.ID, item.Name, item.Category, item.UnitPrice, item.Available, item.CreatedAt, item.UpdatedAt}); err != nil {
			return err
		}
	}
	return nil
}

func findMenuItem(items []SnackMenuItem, id string) (SnackMenuItem, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return SnackMenuItem{}, false
}

func (s *BillingStore) addPayment(id string, request PaymentRequest) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	method, err := normalizePaymentMethod(request.Method)
	if err != nil {
		return Session{}, err
	}
	if request.Amount < 0 {
		return Session{}, errors.New("payment amount cannot be negative")
	}
	if strings.TrimSpace(request.RequestID) == "" {
		return Session{}, errors.New("payment request ID is required")
	}
	if len(request.RequestID) > 200 {
		return Session{}, errors.New("payment request ID is too long")
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
		normalizeSessionLedger(&x)
		for _, payment := range x.PaymentHistory {
			if payment.RequestID != request.RequestID {
				continue
			}
			if payment.Method != method || (request.Amount > 0 && payment.Amount != request.Amount) {
				return Session{}, errors.New("payment request ID was already used with different billing values")
			}
			return x, nil
		}
		amount := request.Amount
		if amount <= 0 {
			amount = x.AmountDue
		}
		if amount <= 0 {
			return Session{}, errors.New("no amount is due")
		}
		if amount > x.AmountDue {
			return Session{}, errors.New("payment cannot exceed the outstanding balance")
		}
		stamp := ledgerNow()
		paymentID, err := newLedgerID("PAY-")
		if err != nil {
			return Session{}, err
		}
		x.PaymentHistory = append(x.PaymentHistory, PaymentTransaction{ID: paymentID, RequestID: request.RequestID, SessionID: x.ID, Timestamp: stamp, Amount: amount, Method: method, PCID: x.PCID, PCName: x.PCName, CustomerName: x.CustomerName})
		x.PaymentAt, x.UpdatedAt, x.SyncedUpdatedAt = stamp, stamp, ""
		x.payments = append(x.payments, paymentRecord{ID: paymentID, RequestID: request.RequestID, Timestamp: stamp, Amount: amount, Method: method, Status: "payment"})
		normalizeSessionLedger(&x)
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

func (s *BillingStore) addSnackOrder(id string, request SnackOrderRequest) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	if request.Quantity < 1 || request.Quantity > 1000 {
		return Session{}, errors.New("snack quantity must be between 1 and 1000")
	}
	if strings.TrimSpace(request.RequestID) == "" {
		return Session{}, errors.New("snack order request ID is required")
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
		if stored.Session.ID != id {
			continue
		}
		for _, order := range stored.Session.SnackOrders {
			if order.RequestID == request.RequestID {
				return stored.Session, nil
			}
		}
	}
	items, err := s.menuLocked(f)
	if err != nil {
		return Session{}, err
	}
	item, ok := findMenuItem(items, request.ItemID)
	if !ok {
		return Session{}, errors.New("snack menu item not found")
	}
	if !item.Available {
		return Session{}, errors.New("snack menu item is out of stock")
	}
	if item.UnitPrice <= 0 {
		return Session{}, errors.New("snack menu item has no configured price")
	}
	for _, stored := range rows {
		x := stored.Session
		if x.ID != id {
			continue
		}
		if x.Status != "open" {
			return Session{}, errors.New("snacks can only be added to an open session")
		}
		for _, order := range x.SnackOrders {
			if order.RequestID == request.RequestID {
				return x, nil
			}
		}
		stamp := ledgerNow()
		orderID, err := newLedgerID("SNACK-")
		if err != nil {
			return Session{}, err
		}
		order := SnackOrder{ID: orderID, RequestID: request.RequestID, SessionID: x.ID, Timestamp: stamp, PCID: x.PCID, PCName: x.PCName, CustomerName: x.CustomerName, ItemID: item.ID, ItemName: item.Name, Category: item.Category, Quantity: request.Quantity, UnitPrice: item.UnitPrice, TotalAmount: item.UnitPrice * request.Quantity, Kind: "sale"}
		x.SnackOrders = append(x.SnackOrders, order)
		x.UpdatedAt, x.SyncedUpdatedAt = stamp, ""
		normalizeSessionLedger(&x)
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

func (s *BillingStore) addSnackCharge(id string, request SnackChargeRequest) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	if request.Amount < 1 || request.Amount > 9999999 {
		return Session{}, errors.New("snack charge must be between ₹1 and ₹9999999")
	}
	if strings.TrimSpace(request.RequestID) == "" || len(request.RequestID) > 200 {
		return Session{}, errors.New("snack charge request ID is required and must be at most 200 characters")
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
		for _, order := range x.SnackOrders {
			if order.RequestID == request.RequestID {
				if order.TotalAmount != request.Amount || order.Kind != "charge" {
					return Session{}, errors.New("snack charge request ID was already used with different billing values")
				}
				return x, nil
			}
		}
		if x.Status != "open" {
			return Session{}, errors.New("snack charges can only be added to an open session")
		}
		stamp := ledgerNow()
		orderID, err := newLedgerID("SNACK-")
		if err != nil {
			return Session{}, err
		}
		order := SnackOrder{
			ID: orderID, RequestID: request.RequestID, SessionID: x.ID, Timestamp: stamp,
			PCID: x.PCID, PCName: x.PCName, CustomerName: x.CustomerName,
			ItemID: orderID, ItemName: "Snacks & Beverages", Category: "Other", Quantity: 1,
			UnitPrice: request.Amount, TotalAmount: request.Amount, Kind: "charge",
		}
		x.SnackOrders = append(x.SnackOrders, order)
		x.UpdatedAt, x.SyncedUpdatedAt = stamp, ""
		normalizeSessionLedger(&x)
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

func (s *BillingStore) adjustSnackCharge(id string, request SnackChargeAdjustmentRequest) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	if strings.TrimSpace(request.OrderID) == "" {
		return Session{}, errors.New("snack charge transaction ID is required")
	}
	if request.AmountDelta == 0 || request.AmountDelta < -9999999 || request.AmountDelta > 9999999 {
		return Session{}, errors.New("snack charge correction must be between -₹9999999 and ₹9999999")
	}
	if strings.TrimSpace(request.RequestID) == "" || len(request.RequestID) > 200 {
		return Session{}, errors.New("snack charge correction request ID is required and must be at most 200 characters")
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
		for _, order := range x.SnackOrders {
			if order.RequestID == request.RequestID {
				return x, nil
			}
		}
		if x.Status != "open" {
			return Session{}, errors.New("snack charges can only be corrected on an open session")
		}
		var source *SnackOrder
		for index := range x.SnackOrders {
			order := &x.SnackOrders[index]
			if order.ID == request.OrderID && order.Kind == "charge" {
				source = order
				break
			}
		}
		if source == nil {
			return Session{}, errors.New("snack charge transaction was not found")
		}
		currentAmount := 0
		for _, order := range x.SnackOrders {
			if order.ID == source.ID || order.ItemID == source.ID {
				currentAmount += order.TotalAmount
			}
		}
		if currentAmount+request.AmountDelta < 0 {
			return Session{}, errors.New("snack charge cannot be corrected below zero")
		}
		stamp := ledgerNow()
		adjustmentID, err := newLedgerID("SNACK-")
		if err != nil {
			return Session{}, err
		}
		adjustment := SnackOrder{
			ID: adjustmentID, RequestID: request.RequestID, SessionID: x.ID, Timestamp: stamp,
			PCID: x.PCID, PCName: x.PCName, CustomerName: x.CustomerName,
			ItemID: source.ID, ItemName: source.ItemName, Category: source.Category,
			Quantity: 1, UnitPrice: request.AmountDelta, TotalAmount: request.AmountDelta, Kind: "charge_adjustment",
		}
		x.SnackOrders = append(x.SnackOrders, adjustment)
		x.UpdatedAt, x.SyncedUpdatedAt = stamp, ""
		normalizeSessionLedger(&x)
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

func (s *BillingStore) adjustSnackOrder(id string, request SnackAdjustmentRequest) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	if request.QuantityDelta == 0 || request.QuantityDelta < -1000 || request.QuantityDelta > 1000 {
		return Session{}, errors.New("snack quantity change must be between -1000 and 1000")
	}
	if strings.TrimSpace(request.RequestID) == "" {
		return Session{}, errors.New("snack order request ID is required")
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
		if x.Status != "open" {
			return Session{}, errors.New("snacks can only be adjusted on an open session")
		}
		for _, order := range x.SnackOrders {
			if order.RequestID == request.RequestID {
				return x, nil
			}
		}
		var source SnackOrder
		quantity := 0
		for _, order := range x.SnackOrders {
			if order.ItemID == request.ItemID {
				quantity += order.Quantity
				if source.ID == "" {
					source = order
				}
			}
		}
		if source.ID == "" {
			return Session{}, errors.New("snack order item was not found on this session")
		}
		if quantity+request.QuantityDelta < 0 {
			return Session{}, errors.New("snack quantity cannot be negative")
		}
		stamp := ledgerNow()
		orderID, err := newLedgerID("SNACK-")
		if err != nil {
			return Session{}, err
		}
		adjustment := SnackOrder{ID: orderID, RequestID: request.RequestID, SessionID: x.ID, Timestamp: stamp, PCID: x.PCID, PCName: x.PCName, CustomerName: x.CustomerName, ItemID: source.ItemID, ItemName: source.ItemName, Category: source.Category, Quantity: request.QuantityDelta, UnitPrice: source.UnitPrice, TotalAmount: source.UnitPrice * request.QuantityDelta, Kind: "adjustment"}
		x.SnackOrders = append(x.SnackOrders, adjustment)
		x.UpdatedAt, x.SyncedUpdatedAt = stamp, ""
		normalizeSessionLedger(&x)
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

func (s *BillingStore) listMenu() ([]SnackMenuItem, error) {
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
	return s.menuLocked(f)
}

func (s *BillingStore) replaceMenu(items []SnackMenuItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return err
	}
	f, err := s.openLocked()
	if err != nil {
		return err
	}
	defer f.Close()
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.Name) == "" || len(item.Name) > 120 {
			return errors.New("each snack menu item needs an ID and a name of at most 120 characters")
		}
		if _, exists := seen[item.ID]; exists {
			return errors.New("snack menu contains duplicate item IDs")
		}
		seen[item.ID] = struct{}{}
		if _, ok := snackCategories[strings.TrimSpace(item.Category)]; !ok {
			return errors.New("invalid snack category")
		}
		if item.UnitPrice < 0 || item.UnitPrice > 9999999 {
			return errors.New("snack price must be between ₹0 and ₹9999999")
		}
	}
	if err := writeMenuLocked(f, items); err != nil {
		return err
	}
	return s.saveLocked(f)
}

func (s *BillingStore) mutateMenu(item SnackMenuItem, remove bool) (SnackMenuItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return SnackMenuItem{}, err
	}
	f, err := s.openLocked()
	if err != nil {
		return SnackMenuItem{}, err
	}
	defer f.Close()
	items, err := s.menuLocked(f)
	if err != nil {
		return SnackMenuItem{}, err
	}
	now := ledgerNow()
	if remove {
		filtered := items[:0]
		for _, current := range items {
			if current.ID != item.ID {
				filtered = append(filtered, current)
			}
		}
		if len(filtered) == len(items) {
			return SnackMenuItem{}, errors.New("snack menu item not found")
		}
		if err := writeMenuLocked(f, filtered); err != nil {
			return SnackMenuItem{}, err
		}
		if err := s.saveLocked(f); err != nil {
			return SnackMenuItem{}, err
		}
		return item, nil
	}
	item.Name = strings.TrimSpace(item.Name)
	item.Category = strings.TrimSpace(item.Category)
	if item.Name == "" || len(item.Name) > 120 {
		return SnackMenuItem{}, errors.New("snack name is required and must be at most 120 characters")
	}
	if _, ok := snackCategories[item.Category]; !ok {
		return SnackMenuItem{}, errors.New("invalid snack category")
	}
	if item.UnitPrice < 0 || item.UnitPrice > 9999999 {
		return SnackMenuItem{}, errors.New("snack price must be between ₹0 and ₹9999999")
	}
	if item.ID == "" {
		item.ID, err = newLedgerID("MENU-")
		if err != nil {
			return SnackMenuItem{}, err
		}
		item.CreatedAt = now
	}
	if item.CreatedAt == "" {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	found := false
	for index := range items {
		if items[index].ID == item.ID {
			items[index] = item
			found = true
			break
		}
	}
	if !found {
		items = append(items, item)
	}
	if err := writeMenuLocked(f, items); err != nil {
		return SnackMenuItem{}, err
	}
	if err := s.saveLocked(f); err != nil {
		return SnackMenuItem{}, err
	}
	return item, nil
}

func addSnackHandler(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var request SnackOrderRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid snack order"})
		return
	}
	if request.RequestID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request_id is required"})
		return
	}
	result, err := addSnackForDashboardContext(r.Context(), sessionID, request)
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func paymentHandler(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var request PaymentRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payment"})
		return
	}
	if request.RequestID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request_id is required"})
		return
	}
	result, err := recordPaymentForDashboardContext(r.Context(), sessionID, request)
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func menuHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := billingStore.listMenu()
		if err != nil {
			writeBillingError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost, http.MethodPut:
		var item SnackMenuItem
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&item) != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid snack menu item"})
			return
		}
		if r.Method == http.MethodPost {
			item.ID = ""
		}
		updated, err := billingStore.mutateMenu(item, false)
		if err != nil {
			writeBillingError(w, err)
			return
		}
		if items, syncErr := billingStore.listMenu(); syncErr == nil {
			syncMenuToPeers(r.Context(), items)
		}
		writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "menu item id is required"})
			return
		}
		removed, err := billingStore.mutateMenu(SnackMenuItem{ID: id}, true)
		if err != nil {
			writeBillingError(w, err)
			return
		}
		if items, syncErr := billingStore.listMenu(); syncErr == nil {
			syncMenuToPeers(r.Context(), items)
		}
		writeJSON(w, http.StatusOK, removed)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET, POST, PUT, or DELETE required"})
	}
}

func localMenuSyncHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var items []SnackMenuItem
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&items); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid snack menu"})
		return
	}
	if err := billingStore.replaceMenu(items); err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func syncMenuToPeers(ctx context.Context, items []SnackMenuItem) {
	for _, pc := range store.list() {
		if localBillingPC(pc) {
			continue
		}
		if err := callBillingPeerContext(ctx, pc, http.MethodPost, "/internal/api/snacks/menu/sync", items, nil); err != nil {
			logf("billing", "snack menu sync failed for pc_id=%s: %v", pc.ID, err)
		}
	}
}

func recordPaymentForDashboardContext(ctx context.Context, id string, request PaymentRequest) (Session, error) {
	session, err := billingStore.sessionByID(id)
	if err != nil {
		return Session{}, err
	}
	pc, ok := store.find(session.PCID)
	if !ok || normalizeSessionAddress(pc.Address) != normalizeSessionAddress(session.PCAddress) {
		return Session{}, errors.New("the session's exact system endpoint is not registered")
	}
	if localBillingPC(pc) {
		return billingStore.addPayment(id, request)
	}
	payload := struct {
		PCID      string `json:"pc_id"`
		PCAddress string `json:"pc_address"`
		DeviceID  string `json:"device_id"`
		PaymentRequest
	}{PCID: session.PCID, PCAddress: session.PCAddress, DeviceID: session.DeviceID, PaymentRequest: request}
	var updated Session
	if err := callBillingPeerContext(ctx, pc, http.MethodPost, "/internal/api/local/sessions/"+url.PathEscape(id)+"/pay", payload, &updated); err != nil {
		return Session{}, err
	}
	if err := billingStore.mergeSyncedSessions(updated.DeviceID, []Session{updated}); err != nil {
		return Session{}, fmt.Errorf("the owning PC recorded payment, but this dashboard could not save the synchronized record: %w", err)
	}
	return updated, nil
}

func addSnackForDashboardContext(ctx context.Context, id string, request SnackOrderRequest) (Session, error) {
	session, err := billingStore.sessionByID(id)
	if err != nil {
		return Session{}, err
	}
	pc, ok := store.find(session.PCID)
	if !ok || normalizeSessionAddress(pc.Address) != normalizeSessionAddress(session.PCAddress) {
		return Session{}, errors.New("the session's exact system endpoint is not registered")
	}
	request.PCID, request.PCAddress, request.DeviceID = session.PCID, session.PCAddress, session.DeviceID
	if localBillingPC(pc) {
		return billingStore.addSnackOrder(id, request)
	}
	var updated Session
	if err := callBillingPeerContext(ctx, pc, http.MethodPost, "/internal/api/local/sessions/"+url.PathEscape(id)+"/snacks", request, &updated); err != nil {
		return Session{}, err
	}
	if err := billingStore.mergeSyncedSessions(updated.DeviceID, []Session{updated}); err != nil {
		return Session{}, fmt.Errorf("the owning PC recorded snack order, but this dashboard could not save the synchronized record: %w", err)
	}
	return updated, nil
}

func addSnackChargeForDashboardContext(ctx context.Context, id string, request SnackChargeRequest) (Session, error) {
	session, err := billingStore.sessionByID(id)
	if err != nil {
		return Session{}, err
	}
	pc, ok := store.find(session.PCID)
	if !ok || normalizeSessionAddress(pc.Address) != normalizeSessionAddress(session.PCAddress) {
		return Session{}, errors.New("the session's exact system endpoint is not registered")
	}
	if localBillingPC(pc) {
		return billingStore.addSnackCharge(id, request)
	}
	payload := struct {
		PCID      string `json:"pc_id"`
		PCAddress string `json:"pc_address"`
		DeviceID  string `json:"device_id"`
		SnackChargeRequest
	}{session.PCID, session.PCAddress, session.DeviceID, request}
	var updated Session
	if err := callBillingPeerContext(ctx, pc, http.MethodPost, "/internal/api/local/sessions/"+url.PathEscape(id)+"/snacks/charge", payload, &updated); err != nil {
		return Session{}, err
	}
	if err := billingStore.mergeSyncedSessions(updated.DeviceID, []Session{updated}); err != nil {
		return Session{}, fmt.Errorf("the owning PC recorded snack charge, but this dashboard could not save the synchronized record: %w", err)
	}
	return updated, nil
}

func adjustSnackChargeForDashboardContext(ctx context.Context, id string, request SnackChargeAdjustmentRequest) (Session, error) {
	session, err := billingStore.sessionByID(id)
	if err != nil {
		return Session{}, err
	}
	pc, ok := store.find(session.PCID)
	if !ok || normalizeSessionAddress(pc.Address) != normalizeSessionAddress(session.PCAddress) {
		return Session{}, errors.New("the session's exact system endpoint is not registered")
	}
	if localBillingPC(pc) {
		return billingStore.adjustSnackCharge(id, request)
	}
	payload := struct {
		PCID      string `json:"pc_id"`
		PCAddress string `json:"pc_address"`
		DeviceID  string `json:"device_id"`
		SnackChargeAdjustmentRequest
	}{session.PCID, session.PCAddress, session.DeviceID, request}
	var updated Session
	if err := callBillingPeerContext(ctx, pc, http.MethodPost, "/internal/api/local/sessions/"+url.PathEscape(id)+"/snacks/charge-adjust", payload, &updated); err != nil {
		return Session{}, err
	}
	if err := billingStore.mergeSyncedSessions(updated.DeviceID, []Session{updated}); err != nil {
		return Session{}, fmt.Errorf("the owning PC corrected snack charge, but this dashboard could not save the synchronized record: %w", err)
	}
	return updated, nil
}

func adjustSnackForDashboardContext(ctx context.Context, id string, request SnackAdjustmentRequest) (Session, error) {
	session, err := billingStore.sessionByID(id)
	if err != nil {
		return Session{}, err
	}
	pc, ok := store.find(session.PCID)
	if !ok || normalizeSessionAddress(pc.Address) != normalizeSessionAddress(session.PCAddress) {
		return Session{}, errors.New("the session's exact system endpoint is not registered")
	}
	if localBillingPC(pc) {
		return billingStore.adjustSnackOrder(id, request)
	}
	var updated Session
	payload := struct {
		PCID, PCAddress, DeviceID string
		SnackAdjustmentRequest
	}{session.PCID, session.PCAddress, session.DeviceID, request}
	if err := callBillingPeerContext(ctx, pc, http.MethodPost, "/internal/api/local/sessions/"+url.PathEscape(id)+"/snacks/adjust", payload, &updated); err != nil {
		return Session{}, err
	}
	if err := billingStore.mergeSyncedSessions(updated.DeviceID, []Session{updated}); err != nil {
		return Session{}, fmt.Errorf("the owning PC recorded the snack adjustment, but this dashboard could not save the synchronized record: %w", err)
	}
	return updated, nil
}
