//go:build windows

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type VirtualTimer struct {
	sync.Mutex
	Config OnBehalfConfig
	Countdown
	AlertVisible bool
	activity     ActivityLog
}

var virtualTimers = struct {
	sync.RWMutex
	items map[string]*VirtualTimer
}{items: make(map[string]*VirtualTimer)}

func initializeOnBehalfTimers() {
	base := appDataPath("")
	virtualTimers.Lock()
	defer virtualTimers.Unlock()
	for _, config := range appConfig.OnBehalfOf {
		id := strings.Trim(strings.TrimSpace(config.ID), "/")
		if !isNumericID(id) || virtualTimers.items[id] != nil {
			logf("onbehalf", "ignoring invalid numeric id %q", config.ID)
			continue
		}
		config.ID = id
		config.SpecialCommands = normalizeSpecialCommands(config.SpecialCommands, "on-behalf timer "+id)
		if config.Name == "" {
			config.Name = "Agent " + id
		}
		if config.CommandTimeoutSeconds < 1 {
			config.CommandTimeoutSeconds = 10
		}
		directory := filepath.Join(base, id)
		_ = os.MkdirAll(directory, 0755)
		virtualTimers.items[id] = &VirtualTimer{Config: config, activity: ActivityLog{path: filepath.Join(directory, activityFileName)}}
	}
	go tickVirtualTimers()
}

func tickVirtualTimers() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		for _, timer := range virtualTimerList() {
			timer.Lock()
			hitZero := timer.tick()
			if hitZero && !timer.AlertVisible {
				timer.AlertVisible = true
			}
			timer.Unlock()
			if hitZero {
				go timer.runHook("show", timer.Config.ShowCommand)
			}
		}
	}
}

func (timer *VirtualTimer) status() TimerStatus {
	timer.Lock()
	defer timer.Unlock()
	status := timer.Countdown.status(timer.Config.ID, timer.Config.Name, timer.AlertVisible)
	status.SpecialCommands = specialCommandLabels(timer.Config.SpecialCommands)
	return status
}

func virtualTimerList() []*VirtualTimer {
	virtualTimers.RLock()
	ids := make([]string, 0, len(virtualTimers.items))
	for id := range virtualTimers.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]*VirtualTimer, 0, len(ids))
	for _, id := range ids {
		items = append(items, virtualTimers.items[id])
	}
	virtualTimers.RUnlock()
	return items
}

func onBehalfStatuses() []TimerStatus {
	items := virtualTimerList()
	result := make([]TimerStatus, 0, len(items))
	for _, timer := range items {
		result = append(result, timer.status())
	}
	return result
}

func (timer *VirtualTimer) runHook(action string, command []string) {
	if len(command) == 0 {
		timer.log(action, "success", "no command configured")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timer.Config.CommandTimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	output, err := cmd.CombinedOutput()
	detail := strings.TrimSpace(string(output))
	result := "success"
	if err != nil {
		result = "failed"
		detail = strings.TrimSpace(detail + " " + err.Error())
	}
	if ctx.Err() == context.DeadlineExceeded {
		result = "failed"
		detail = "command timed out"
	}
	timer.log(action, result, detail)
}

func (timer *VirtualTimer) log(action, result, detail string) {
	timer.activity.append("on_behalf_command", PC{ID: timer.Config.ID, Name: timer.Config.Name}, action, "", result, 200, detail)
}

func registerOnBehalfHandlers(mux *http.ServeMux) {
	virtualTimers.RLock()
	defer virtualTimers.RUnlock()
	for id, timer := range virtualTimers.items {
		prefix := "/" + id
		mux.HandleFunc(prefix+"/status", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				writeJSON(w, 405, map[string]string{"message": "GET required"})
				return
			}
			writeJSON(w, 200, timer.status())
		})
		registerTimerCommands(mux, prefix, timer.applyAction)
		owner := localRegistryPC()
		owner.ID, owner.Name, owner.Address = id, timer.Config.Name, owner.Address+prefix
		setBillingSessionLinkHandler(mux, prefix+"/billing-session", owner.Address)
		registerSpecialCommands(mux, prefix, timer.Config.SpecialCommands, timer.Config.CommandTimeoutSeconds, owner, &timer.activity)
	}
}

func (timer *VirtualTimer) applyAction(action string, value int) int {
	timer.Lock()
	timer.apply(action, value)
	timer.AlertVisible = action == "show"
	hook := timer.Config.HideCommand
	if timer.AlertVisible {
		hook = timer.Config.ShowCommand
	}
	remaining := timer.Remaining
	timer.Unlock()
	go timer.runHook(action, hook)
	timer.log(action, "success", fmt.Sprintf("remaining_seconds=%d", remaining))
	return remaining
}
