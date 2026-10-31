//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *BillingStore) ensureDeviceIDLocked() error {
	if s.deviceID != "" {
		if s.configuredID != "" && s.deviceID != s.configuredID {
			return errors.New("configured billing device_id does not match this device's persisted ID")
		}
		return nil
	}
	path := filepath.Join(filepath.Dir(s.path), billingDeviceIDName)
	if data, err := os.ReadFile(path); err == nil {
		s.deviceID = strings.TrimSpace(string(data))
		if s.deviceID == "" {
			return errors.New("billing device ID file is empty")
		}
		if s.configuredID != "" && s.deviceID != s.configuredID {
			return errors.New("configured billing device_id does not match this device's persisted ID")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("could not read billing device ID: %w", err)
	}
	if s.configuredID != "" {
		s.deviceID = s.configuredID
	} else {
		var randomID [16]byte
		if _, err := rand.Read(randomID[:]); err != nil {
			return fmt.Errorf("could not generate billing device ID: %w", err)
		}
		s.deviceID = "PC-" + hex.EncodeToString(randomID[:])
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(s.deviceID+"\n"), 0600); err != nil {
		return fmt.Errorf("could not save billing device ID: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("could not install billing device ID: %w", err)
	}
	return nil
}

func (s *BillingStore) syncStatusPath() string {
	return filepath.Join(filepath.Dir(s.path), billingSyncStateName)
}

func (s *BillingStore) loadSyncStatusesLocked() error {
	data, err := os.ReadFile(s.syncStatusPath())
	if errors.Is(err, os.ErrNotExist) {
		s.syncStatuses = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not read billing synchronization state: %w", err)
	}
	if err := json.Unmarshal(data, &s.syncStatuses); err != nil {
		return fmt.Errorf("could not decode billing synchronization state: %w", err)
	}
	for i := range s.syncStatuses {
		s.syncStatuses[i].Online = false
		if s.syncStatuses[i].Error == "" {
			s.syncStatuses[i].Error = "awaiting synchronization check"
		}
	}
	return nil
}

func (s *BillingStore) replaceSyncStatuses(statuses []BillingDeviceStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return err
	}
	next := append([]BillingDeviceStatus(nil), statuses...)
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.syncStatusPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("could not write billing synchronization state: %w", err)
	}
	if err := os.Rename(tmp, s.syncStatusPath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("could not replace billing synchronization state: %w", err)
	}
	s.syncStatuses = next
	return nil
}

func (s *BillingStore) billingSyncStatuses() []BillingDeviceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]BillingDeviceStatus(nil), s.syncStatuses...)
}

func (s *BillingStore) pendingSync() (BillingSyncExport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return BillingSyncExport{}, err
	}
	f, err := s.openLocked()
	if err != nil {
		return BillingSyncExport{}, err
	}
	defer f.Close()
	sessions, err := sessionsFromWorkbook(f)
	if err != nil {
		return BillingSyncExport{}, err
	}
	pending := make([]Session, 0)
	for _, session := range sessions {
		if session.DeviceID == "" {
			session.DeviceID = s.deviceID
		}
		if session.SyncedUpdatedAt != session.UpdatedAt {
			pending = append(pending, session)
		}
	}
	return BillingSyncExport{DeviceID: s.deviceID, Name: configuredFriendlyName(), Sessions: pending}, nil
}

func (s *BillingStore) acknowledgeSync(ack BillingSyncAck) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return 0, err
	}
	if err := s.ensureDeviceIDLocked(); err != nil {
		return 0, err
	}
	if ack.DeviceID != s.deviceID {
		return 0, errors.New("synchronization acknowledgement has the wrong device ID")
	}
	f, err := s.openLocked()
	if err != nil {
		return 0, err
	}
	defer f.Close()
	stored, err := storedSessionsFromWorkbook(f)
	if err != nil {
		return 0, err
	}
	acknowledgements := make(map[string]string, len(ack.Sessions))
	for _, item := range ack.Sessions {
		if item.ID == "" || item.UpdatedAt == "" {
			return 0, errors.New("synchronization acknowledgement is missing a session ID or update timestamp")
		}
		acknowledgements[item.ID] = item.UpdatedAt
	}
	changed := false
	acknowledged := 0
	for _, item := range stored {
		session := item.Session
		updatedAt, exists := acknowledgements[session.ID]
		if !exists || session.UpdatedAt != updatedAt {
			continue
		}
		acknowledged++
		if session.DeviceID == "" {
			session.DeviceID = s.deviceID
		}
		if session.SyncedUpdatedAt == updatedAt {
			continue
		}
		session.SyncedUpdatedAt = updatedAt
		if err := writeSessionRow(f, "Sessions", item.Row, session, true); err != nil {
			return 0, err
		}
		changed = true
	}
	if changed {
		if err := s.saveLocked(f); err != nil {
			return 0, err
		}
	}
	return acknowledged, nil
}

func (s *BillingStore) mergeSyncedSessions(deviceID string, incoming []Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return err
	}
	if deviceID == "" {
		return errors.New("billing synchronization payload has no device ID")
	}
	f, err := s.openLocked()
	if err != nil {
		return err
	}
	defer f.Close()
	stored, err := storedSessionsFromWorkbook(f)
	if err != nil {
		return err
	}
	byKey := make(map[string]storedBillingSession, len(stored))
	for _, item := range stored {
		key := item.Session.DeviceID + "\x00" + item.Session.ID
		byKey[key] = item
	}
	candidates := make([]Session, 0, len(incoming))
	candidateIndex := make(map[string]int, len(incoming))
	for _, session := range incoming {
		if session.ID == "" || session.DeviceID != deviceID || session.UpdatedAt == "" {
			return errors.New("billing synchronization payload contains an invalid session identity")
		}
		if _, err := sessionTimestamp(session.UpdatedAt); err != nil {
			return fmt.Errorf("session %q has an invalid update timestamp: %w", session.ID, err)
		}
		key := deviceID + "\x00" + session.ID
		if index, exists := candidateIndex[key]; exists {
			priorUpdated, _ := sessionTimestamp(candidates[index].UpdatedAt)
			incomingUpdated, _ := sessionTimestamp(session.UpdatedAt)
			if incomingUpdated.After(priorUpdated) {
				candidates[index] = session
			}
			continue
		}
		candidateIndex[key] = len(candidates)
		candidates = append(candidates, session)
	}
	rows, err := sessionRows(f)
	if err != nil {
		return err
	}
	changed := false
	for _, session := range candidates {
		key := deviceID + "\x00" + session.ID
		if existing, exists := byKey[key]; exists {
			oldTime, oldErr := sessionTimestamp(existing.Session.UpdatedAt)
			newTime, newErr := sessionTimestamp(session.UpdatedAt)
			if oldErr != nil || newErr != nil {
				return fmt.Errorf("session %q has an invalid update timestamp", session.ID)
			}
			if !newTime.After(oldTime) {
				continue
			}
			session.SyncedUpdatedAt = session.UpdatedAt
			if err := writeSessionRow(f, "Sessions", existing.Row, session, true); err != nil {
				return err
			}
			changed = true
			continue
		}
		session.SyncedUpdatedAt = session.UpdatedAt
		row := len(rows) + 1
		if err := writeSessionRow(f, "Sessions", row, session, false); err != nil {
			return err
		}
		if err := formatSessionDataRows(f, "Sessions", row, row); err != nil {
			return err
		}
		rows = append(rows, nil)
		changed = true
	}
	if changed {
		return s.saveLocked(f)
	}
	return nil
}

func (s *BillingStore) sessionByID(id string) (Session, error) {
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
	sessions, err := sessionsFromWorkbook(f)
	if err != nil {
		return Session{}, err
	}
	var found *Session
	for i := range sessions {
		if sessions[i].ID != id {
			continue
		}
		if found != nil {
			return Session{}, errors.New("session ID is ambiguous across billing devices")
		}
		found = &sessions[i]
	}
	if found == nil {
		return Session{}, errors.New("session not found")
	}
	return *found, nil
}

func (s *BillingStore) report(date string) (BillingReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return BillingReport{}, err
	}
	parsedDate, err := time.ParseInLocation("2006-01-02", date, billingTimezone)
	if err != nil || parsedDate.Format("2006-01-02") != date {
		return BillingReport{}, errors.New("date must use YYYY-MM-DD")
	}
	f, err := s.openLocked()
	if err != nil {
		return BillingReport{}, err
	}
	defer f.Close()
	sessions, err := sessionsFromWorkbook(f)
	if err != nil {
		return BillingReport{}, err
	}
	report := BillingReport{
		Date: date, Timezone: billingTimezoneName,
		Devices: append([]BillingDeviceStatus(nil), s.syncStatuses...),
	}
	report.SystemCount = len(report.Devices)
	report.Complete = len(report.Devices) > 0
	for _, device := range report.Devices {
		if !device.Online || device.PendingRecords > 0 || device.Error != "" {
			report.Complete = false
		}
	}
	pcIndex := make(map[string]int)
	customers := make(map[string]struct{})
	for _, session := range sessions {
		normalizeSessionLedger(&session)
		gamingCollected, snackCollected := collectedChargeAllocation(session)
		started, parseErr := sessionTimestamp(session.StartedAt)
		if parseErr != nil || started.In(billingTimezone).Format("2006-01-02") != date {
			continue
		}
		report.Records = append(report.Records, session)
		report.SessionCount++
		report.CustomerCount++
		customer := strings.ToLower(strings.TrimSpace(session.CustomerName))
		if customer != "" {
			customers[customer] = struct{}{}
		}
		report.BilledAmount += session.FinalAmount
		report.CollectedAmount += session.AmountPaid
		report.Revenue += session.AmountPaid
		report.GamingRevenueCollected += gamingCollected
		report.SnackRevenueCollected += snackCollected
		due := session.AmountDue
		if due > 0 {
			report.Outstanding += due
		}
		report.CashCollected += session.CashPaid
		report.OnlineCollected += session.OnlinePaid
		report.SnackSales += session.SnackAmount
		switch session.PaymentStatus {
		case "unpaid":
			report.UnpaidSessions++
		case "partial":
			report.PartiallyPaidSessions++
		case "paid":
			report.PaidSessions++
		}
		key := session.DeviceID + "\x00" + session.PCID
		index, exists := pcIndex[key]
		if !exists {
			index = len(report.PCs)
			pcIndex[key] = index
			report.PCs = append(report.PCs, BillingReportPC{
				DeviceID: session.DeviceID, PCID: session.PCID, PCName: session.PCName,
			})
		}
		report.PCs[index].SessionCount++
		report.PCs[index].CustomerCount++
		report.PCs[index].BilledAmount += session.FinalAmount
		report.PCs[index].CollectedAmount += session.AmountPaid
		report.PCs[index].Revenue += session.AmountPaid
		report.PCs[index].GamingRevenueCollected += gamingCollected
		report.PCs[index].SnackRevenueCollected += snackCollected
		if due > 0 {
			report.PCs[index].Outstanding += due
		}
		report.PCs[index].CashCollected += session.CashPaid
		report.PCs[index].OnlineCollected += session.OnlinePaid
		report.PCs[index].SnackSales += session.SnackAmount
	}
	report.UniqueCustomerCount = len(customers)
	return report, nil
}
