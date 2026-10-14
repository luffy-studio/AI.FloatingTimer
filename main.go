//go:build windows

// Overlay Timer — always-on-top, borderless, click-through countdown widget
// for Windows. No titlebar, no taskbar entry, no Alt-Tab entry. Controlled
// via a tray icon (Pause/Play/Reset/Add-time/Quit) and a tiny local HTTP
// API — never by clicking the overlay itself, since it's intentionally
// click-through.
//
// LOGGING: every entry/exit, lock acquisition, and Win32/Fyne call is logged
// with a component tag and timestamp to a file, since this is a windowsgui
// build with no console to print to. Log file location is printed in the
// comment at initLogging() below — check there first if something hangs,
// the last line in the file (or the last "begin" with no matching "end")
// tells you exactly which call never returned.
//
// Build:   go build -ldflags="-H=windowsgui -s -w" -o OverlayTimer.exe .
// (the -H=windowsgui ldflag stops a console window from popping up alongside it)
package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// --- CONFIGURATION CONSTANTS ---
const (
	DefaultTimerPort = 18081
	DefaultUIPort    = 18082
	DefaultSeconds   = 60 // 1 minute by default
	TimerTextSize    = 15
	// Let Fyne use the actual rendered text height; clipping the canvas text
	// at 20px cuts off the bottom of the glyphs.

	PaddingLeft = 0
	PaddingTop  = -40
	WindowTitle = "Overlay Timer"
	TimeUpTitle = "Time's Up"
)

// --- LOGGING ---
// windowsgui builds have no console, so everything goes to a file next to
// the exe (falls back to the OS temp dir if that's not writable, e.g. if
// running from Program Files without admin rights).
const LogFilePrefix = "overlay_timer-"
const LegacyLogFileName = "overlay_timer.log"
const ConfigFileName = "overlay_timer_config.json"

var logMu sync.Mutex

type dailyLogWriter struct {
	sync.Mutex
	directory   string
	currentDate string
	file        *os.File
}

func dailyLogFileName(date string) string {
	return LogFilePrefix + date + ".log"
}

func (writer *dailyLogWriter) Write(data []byte) (int, error) {
	writer.Lock()
	defer writer.Unlock()

	date := time.Now().Format("2006-01-02")
	if writer.file == nil || writer.currentDate != date {
		path := filepath.Join(writer.directory, dailyLogFileName(date))
		newFile, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			if writer.file != nil {
				return writer.file.Write(data)
			}
			return 0, err
		}
		oldFile := writer.file
		writer.file = newFile
		writer.currentDate = date
		if oldFile != nil {
			_ = oldFile.Close()
		}
	}
	return writer.file.Write(data)
}

func (writer *dailyLogWriter) Close() error {
	writer.Lock()
	defer writer.Unlock()
	if writer.file == nil {
		return nil
	}
	err := writer.file.Close()
	writer.file = nil
	return err
}

var activeLogWriter *dailyLogWriter

type DiscoveryConfig struct {
	Enabled                    bool     `json:"enabled"`
	IntervalSeconds            int      `json:"interval_seconds"`
	CIDRRanges                 []string `json:"cidr_ranges"`
	ConnectTimeoutMilliseconds int      `json:"connect_timeout_milliseconds"`
	MaximumConcurrency         int      `json:"maximum_concurrency"`
}

type OnBehalfConfig struct {
	ID                    string                 `json:"id"`
	Name                  string                 `json:"name"`
	ShowCommand           []string               `json:"show_command"`
	HideCommand           []string               `json:"hide_command"`
	SpecialCommands       []SpecialCommandConfig `json:"special_commands"`
	CommandTimeoutSeconds int                    `json:"command_timeout_seconds"`
}

type AppConfig struct {
	KillOnTimeUp          bool                   `json:"kill_on_time_up"`
	AppsToCloseOnTimeUp   []string               `json:"apps_to_close_on_time_up"`
	ForceKillAfterSeconds int                    `json:"force_kill_after_seconds"`
	FriendlyName          string                 `json:"friendly_name"`
	TimerPort             int                    `json:"timer_port"`
	UIDiscoveryPort       int                    `json:"ui_discovery_port"`
	Billing               BillingConfig          `json:"billing"`
	Discovery             DiscoveryConfig        `json:"discovery"`
	OnBehalfOf            []OnBehalfConfig       `json:"on_behalf_of"`
	SpecialCommands       []SpecialCommandConfig `json:"special_commands"`
	CommandTimeoutSeconds int                    `json:"command_timeout_seconds"`
}

var appConfig = defaultAppConfig()

func defaultAppConfig() AppConfig {
	return AppConfig{
		AppsToCloseOnTimeUp:   []string{"TekkenGame-Win64-Shipping.exe"},
		ForceKillAfterSeconds: 2,
		TimerPort:             DefaultTimerPort,
		UIDiscoveryPort:       DefaultUIPort,
		Billing:               BillingConfig{Enabled: false},
		SpecialCommands:       []SpecialCommandConfig{},
		CommandTimeoutSeconds: 10,
		Discovery: DiscoveryConfig{
			Enabled: false, IntervalSeconds: 300,
			ConnectTimeoutMilliseconds: 350, MaximumConcurrency: 32,
		},
	}
}

func removeOldLogFiles(directory, todayName string) {
	matches, _ := filepath.Glob(filepath.Join(directory, LogFilePrefix+"*.log"))
	for _, path := range matches {
		if !strings.EqualFold(filepath.Base(path), todayName) {
			_ = os.Remove(path)
		}
	}
	_ = os.Remove(filepath.Join(directory, LegacyLogFileName))
}

func openDailyLogWriter(directory string) (*dailyLogWriter, string, error) {
	date := time.Now().Format("2006-01-02")
	fileName := dailyLogFileName(date)
	removeOldLogFiles(directory, fileName)

	path := filepath.Join(directory, fileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, path, err
	}
	return &dailyLogWriter{
		directory:   directory,
		currentDate: date,
		file:        file,
	}, path, nil
}

func initLogging() string {
	directory := "."
	if exe, err := os.Executable(); err == nil {
		directory = filepath.Dir(exe)
	}

	writer, logPath, err := openDailyLogWriter(directory)
	if err != nil {
		// Fall back to the OS temp directory if the executable directory is not writable.
		writer, logPath, err = openDailyLogWriter(os.TempDir())
		if err != nil {
			log.SetOutput(os.Stdout)
			return "(failed to open any log file: " + err.Error() + ")"
		}
	}

	activeLogWriter = writer
	log.SetOutput(writer)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	return logPath
}

func logf(component, format string, args ...interface{}) {
	logMu.Lock()
	defer logMu.Unlock()
	log.Printf("[%-10s] "+format, append([]interface{}{component}, args...)...)
}

func initConfig() string {
	configPath := ConfigFileName
	if exe, err := os.Executable(); err == nil {
		configPath = filepath.Join(filepath.Dir(exe), ConfigFileName)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		defaultJSON, _ := json.MarshalIndent(appConfig, "", "  ")
		if writeErr := os.WriteFile(configPath, append(defaultJSON, '\n'), 0644); writeErr != nil {
			logf("config", "could not read or create config at %s: read=%v write=%v; using defaults", configPath, err, writeErr)
		} else {
			logf("config", "created default config at %s", configPath)
		}
		return configPath
	}

	loaded := defaultAppConfig()
	if err := json.Unmarshal(data, &loaded); err != nil {
		logf("config", "invalid config at %s: %v; using defaults", configPath, err)
		return configPath
	}
	appConfig = loaded
	normalizeAppConfig()
	logf("config", "loaded config from %s: name=%q timer_port=%d ui_port=%d", configPath, configuredFriendlyName(), appConfig.TimerPort, appConfig.UIDiscoveryPort)
	return configPath
}

type TimerState struct {
	sync.Mutex
	Countdown
	LabelObj      *canvas.Text
	TimeUpVisible bool
}

var state = &TimerState{Countdown: Countdown{Remaining: DefaultSeconds}}

// The full-screen "TIME UP" window. Built once in main() and shown/hidden
// as needed rather than recreated each time.
var (
	timeUpWindow  fyne.Window
	overlayWindow fyne.Window
	backgroundBox *canvas.Rectangle
	overlayHwnd   uintptr // Global handle for the main overlay window
)

// --- NATIVE WINDOWS API CALLS ---
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutexW = kernel32.NewProc("CreateMutexW")
	procCloseHandle  = kernel32.NewProc("CloseHandle")

	user32                         = syscall.NewLazyDLL("user32.dll")
	procFindWindowW                = user32.NewProc("FindWindowW")
	procGetWindowLong              = user32.NewProc("GetWindowLongW")
	procSetWindowLong              = user32.NewProc("SetWindowLongW")
	procSetWindowPos               = user32.NewProc("SetWindowPos")
	procGetSystemMetrics           = user32.NewProc("GetSystemMetrics")
	procSetProcessDPIAware         = user32.NewProc("SetProcessDPIAware")
	procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	procShowWindow                 = user32.NewProc("ShowWindow")
	procReleaseCapture             = user32.NewProc("ReleaseCapture")
	procSendMessageW               = user32.NewProc("SendMessageW")
	procKeybdEvent                 = user32.NewProc("keybd_event")
)

// Windows API constants. The GWL_* ones are declared as plain vars (not a
// `const` block) on purpose: Go won't let you convert a negative untyped
// constant straight to uintptr at compile time (it's an unsigned type), but
// a runtime conversion of a negative int32 variable sign-extends correctly,
// which is what SetWindowLongW/GetWindowLongW actually expect.
var (
	GWL_STYLE   int32 = -16
	GWL_EXSTYLE int32 = -20
)

const (
	WS_CAPTION     = 0x00C00000
	WS_THICKFRAME  = 0x00040000
	WS_SYSMENU     = 0x00080000
	WS_MINIMIZEBOX = 0x00020000
	WS_MAXIMIZEBOX = 0x00010000
	WS_BORDER      = 0x00800000
	WS_DLGFRAME    = 0x00400000

	WS_EX_LAYERED     = 0x00080000
	WS_EX_TRANSPARENT = 0x00000020
	WS_EX_TOOLWINDOW  = 0x00000080 // keeps it out of the taskbar and Alt-Tab
	WS_EX_APPWINDOW   = 0x00040000 // explicitly forces a taskbar button; must be removed

	SM_CYSCREEN = 1

	SWP_NOSIZE       = 0x0001
	SWP_NOMOVE       = 0x0002
	SWP_FRAMECHANGED = 0x0020

	LWA_ALPHA = 0x00000002
	// 0 = fully invisible, 255 = fully opaque. Native window opacity affects
	// both the black panel and timer text. 120 keeps the overlay highly
	// transparent while leaving the digits readable.
	WindowAlpha = 90

	SW_HIDE = 0
	SW_SHOW = 5

	WM_NCLBUTTONDOWN = 0x00A1
	HTCAPTION        = 2

	VK_LWIN         = 0x5B
	VK_SHIFT        = 0x10
	VK_M            = 0x4D
	KEYEVENTF_KEYUP = 0x0002

	ERROR_ALREADY_EXISTS = 183
)

var HWND_TOPMOST = ^uintptr(0) // -1

var instanceMutexHandle uintptr

func normalizeAppConfig() {
	if appConfig.CommandTimeoutSeconds < 1 {
		appConfig.CommandTimeoutSeconds = 10
	}
	appConfig.SpecialCommands = normalizeSpecialCommands(appConfig.SpecialCommands, "local timer")
	if appConfig.TimerPort < 1 || appConfig.TimerPort > 65535 {
		appConfig.TimerPort = DefaultTimerPort
	}
	if appConfig.UIDiscoveryPort < 1 || appConfig.UIDiscoveryPort > 65535 {
		appConfig.UIDiscoveryPort = DefaultUIPort
	}
	if appConfig.Discovery.IntervalSeconds < 10 {
		appConfig.Discovery.IntervalSeconds = 300
	}
	if appConfig.Discovery.ConnectTimeoutMilliseconds < 50 {
		appConfig.Discovery.ConnectTimeoutMilliseconds = 350
	}
	if appConfig.Discovery.MaximumConcurrency < 1 {
		appConfig.Discovery.MaximumConcurrency = 32
	}
	if appConfig.ForceKillAfterSeconds < 0 {
		appConfig.ForceKillAfterSeconds = 0
	}
	normalizeBillingConfig()
}

func configuredFriendlyName() string {
	if name := strings.TrimSpace(appConfig.FriendlyName); name != "" {
		return name
	}
	if name, err := os.Hostname(); err == nil {
		return name
	}
	return "Overlay Timer"
}

func acquireStartupResources() (net.Listener, net.Listener, bool) {
	mutexName, err := syscall.UTF16PtrFromString(fmt.Sprintf(`Local\OverlayTimer-%d-SingleInstance`, appConfig.TimerPort))
	if err != nil {
		return nil, nil, false
	}

	handle, _, createErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(mutexName)))
	if handle == 0 || createErr == syscall.Errno(ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			procCloseHandle.Call(handle)
		}
		return nil, nil, false
	}
	instanceMutexHandle = handle

	listener, uiListener, listenErr := reserveListeners(appConfig.TimerPort, appConfig.UIDiscoveryPort)
	if listenErr != nil {
		logf("main", "startup failed: %v", listenErr)
		procCloseHandle.Call(instanceMutexHandle)
		instanceMutexHandle = 0
		return nil, nil, false
	}
	return listener, uiListener, true
}

var windowControl = struct {
	sync.Mutex
	dragEnabled      bool
	windowsMinimized bool
}{
	dragEnabled:      false,
	windowsMinimized: false,
}

type dragSurface struct {
	widget.BaseWidget
}

func newDragSurface() *dragSurface {
	surface := &dragSurface{}
	surface.ExtendBaseWidget(surface)
	return surface
}

func (surface *dragSurface) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(canvas.NewRectangle(color.Transparent))
}

func (surface *dragSurface) MouseDown(_ *desktop.MouseEvent) {
	windowControl.Lock()
	dragEnabled := windowControl.dragEnabled
	windowControl.Unlock()
	if !dragEnabled || overlayHwnd == 0 {
		return
	}

	procReleaseCapture.Call()
	procSendMessageW.Call(overlayHwnd, uintptr(WM_NCLBUTTONDOWN), uintptr(HTCAPTION), 0)
}

func (surface *dragSurface) MouseUp(_ *desktop.MouseEvent) {}

func setDragEnabled(enabled bool) error {
	if overlayHwnd == 0 {
		return fmt.Errorf("overlay window is not ready")
	}

	exStyle, _, _ := procGetWindowLong.Call(overlayHwnd, uintptr(GWL_EXSTYLE))
	newExStyle := int32(exStyle)
	if enabled {
		newExStyle &^= WS_EX_TRANSPARENT
	} else {
		newExStyle |= WS_EX_TRANSPARENT
	}
	procSetWindowLong.Call(overlayHwnd, uintptr(GWL_EXSTYLE), uintptr(newExStyle))
	procSetWindowPos.Call(overlayHwnd, 0, 0, 0, 0, 0, uintptr(SWP_NOMOVE|SWP_NOSIZE|SWP_FRAMECHANGED))

	windowControl.Lock()
	windowControl.dragEnabled = enabled
	windowControl.Unlock()
	logf("drag", "drag mode enabled=%v", enabled)
	return nil
}

func moveOverlay(x, y int) error {
	if overlayHwnd == 0 {
		return fmt.Errorf("overlay window is not ready")
	}

	result, _, callErr := procSetWindowPos.Call(overlayHwnd, HWND_TOPMOST, uintptr(x), uintptr(y), 0, 0, uintptr(SWP_NOSIZE))
	if result == 0 {
		return fmt.Errorf("could not move overlay: %v", callErr)
	}
	logf("position", "moved overlay to x=%d y=%d", x, y)
	return nil
}

func closeConfiguredAppsOnTimeUp() {
	if !appConfig.KillOnTimeUp {
		logf("killapps", "disabled by config")
		return
	}
	if len(appConfig.AppsToCloseOnTimeUp) == 0 {
		logf("killapps", "enabled but no configured process names")
		return
	}

	ownExeName := ""
	if exe, err := os.Executable(); err == nil {
		ownExeName = filepath.Base(exe)
	}

	for _, configuredName := range appConfig.AppsToCloseOnTimeUp {
		processName := strings.TrimSpace(configuredName)
		if processName == "" {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(processName), ".exe") {
			processName += ".exe"
		}
		if ownExeName != "" && strings.EqualFold(processName, ownExeName) {
			logf("killapps", "refusing to close own process name %q", processName)
			continue
		}

		logf("killapps", "attempting graceful close for configured app %q", processName)
		gracefulCmd := exec.Command("taskkill", "/IM", processName, "/T")
		gracefulOutput, gracefulErr := gracefulCmd.CombinedOutput()
		logf("killapps", "taskkill graceful app=%q err=%v output=%q", processName, gracefulErr, strings.TrimSpace(string(gracefulOutput)))

		if appConfig.ForceKillAfterSeconds > 0 {
			time.Sleep(time.Duration(appConfig.ForceKillAfterSeconds) * time.Second)
			logf("killapps", "attempting force kill for configured app %q", processName)
			forceCmd := exec.Command("taskkill", "/IM", processName, "/T", "/F")
			forceOutput, forceErr := forceCmd.CombinedOutput()
			logf("killapps", "taskkill force app=%q err=%v output=%q", processName, forceErr, strings.TrimSpace(string(forceOutput)))
		}
	}
}

func minimizeAllWindows() {
	logf("minimize", "sending Win+M to minimize all normal desktop windows")
	procKeybdEvent.Call(uintptr(VK_LWIN), 0, 0, 0)
	time.Sleep(20 * time.Millisecond)
	procKeybdEvent.Call(uintptr(VK_M), 0, 0, 0)
	time.Sleep(20 * time.Millisecond)
	procKeybdEvent.Call(uintptr(VK_M), 0, uintptr(KEYEVENTF_KEYUP), 0)
	time.Sleep(20 * time.Millisecond)
	procKeybdEvent.Call(uintptr(VK_LWIN), 0, uintptr(KEYEVENTF_KEYUP), 0)

	windowControl.Lock()
	windowControl.windowsMinimized = true
	windowControl.Unlock()
	logf("minimize", "Win+M sent; restore is now pending")
}

func restoreMinimizedWindows() {
	windowControl.Lock()
	if !windowControl.windowsMinimized {
		windowControl.Unlock()
		logf("restore", "no app-initiated minimize operation is pending")
		return
	}
	// Consume the pending restore before sending keys so concurrent dismiss
	// requests cannot restore/toggle the desktop more than once.
	windowControl.windowsMinimized = false
	windowControl.Unlock()

	logf("restore", "sending Win+Shift+M to restore windows minimized by Win+M")
	procKeybdEvent.Call(uintptr(VK_LWIN), 0, 0, 0)
	time.Sleep(20 * time.Millisecond)
	procKeybdEvent.Call(uintptr(VK_SHIFT), 0, 0, 0)
	time.Sleep(20 * time.Millisecond)
	procKeybdEvent.Call(uintptr(VK_M), 0, 0, 0)
	time.Sleep(20 * time.Millisecond)
	procKeybdEvent.Call(uintptr(VK_M), 0, uintptr(KEYEVENTF_KEYUP), 0)
	time.Sleep(20 * time.Millisecond)
	procKeybdEvent.Call(uintptr(VK_SHIFT), 0, uintptr(KEYEVENTF_KEYUP), 0)
	time.Sleep(20 * time.Millisecond)
	procKeybdEvent.Call(uintptr(VK_LWIN), 0, uintptr(KEYEVENTF_KEYUP), 0)
	logf("restore", "Win+Shift+M sent")
}

func findWindowHandle(title string) uintptr {
	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		logf("win32", "UTF16PtrFromString FAILED: %v", err)
		return 0
	}
	hwnd, _, callErr := procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
	logf("win32", "FindWindowW(%q) -> hwnd=0x%X lastErr=%v", title, hwnd, callErr)
	return hwnd
}

func applyWindowsWindowTweaks(windowWidth, windowHeight float32) {

	logf("win32tweak", "begin")
	// Fyne creates the native window asynchronously and never hands you the
	// HWND directly, so GetParent(0) (what the original code called) always
	// returns 0 — it's asking Windows for the parent of the null handle.
	// FindWindowW by title, polled until it appears, is the reliable way to
	// grab it from outside the Fyne driver.
	var hwnd uintptr
	for i := 0; i < 500; i++ {
		hwnd = findWindowHandle(WindowTitle)
		if hwnd != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if hwnd == 0 {
		logf("win32tweak", "ABORT: never found hwnd for title %q after 5s of polling", WindowTitle)
		return

	}
	logf("win32tweak", "found hwnd=0x%X", hwnd)
	overlayHwnd = hwnd

	// 1. Strip all window chrome: caption bar, system menu (that's where the
	// minimize/maximize/close buttons live), resizable frame, border. What's
	// left is just the bare content — no OS-drawn UI at all.
	style, _, e1 := procGetWindowLong.Call(hwnd, uintptr(GWL_STYLE))
	logf("win32tweak", "GetWindowLongW(GWL_STYLE) -> 0x%X lastErr=%v", style, e1)
	newStyle := int32(style) &^ (WS_CAPTION | WS_THICKFRAME | WS_SYSMENU | WS_MINIMIZEBOX | WS_MAXIMIZEBOX | WS_BORDER | WS_DLGFRAME)
	r1, _, e2 := procSetWindowLong.Call(hwnd, uintptr(GWL_STYLE), uintptr(newStyle))
	logf("win32tweak", "SetWindowLongW(GWL_STYLE, 0x%X) -> prev=0x%X lastErr=%v", newStyle, r1, e2)

	// 2. Layered (needed for true window transparency/click-through) +
	// transparent (clicks pass through to whatever's underneath, since this
	// overlay is meant to be controlled via the HTTP API, not by clicking
	// it) + tool window (hides it from the taskbar and Alt-Tab switcher).
	exStyle, _, e3 := procGetWindowLong.Call(hwnd, uintptr(GWL_EXSTYLE))
	logf("win32tweak", "GetWindowLongW(GWL_EXSTYLE) -> 0x%X lastErr=%v", exStyle, e3)
	// Fyne/GLFW may set WS_EX_APPWINDOW, which overrides TOOLWINDOW and
	// explicitly asks Explorer for a taskbar button. Clearing it is therefore
	// just as important as adding WS_EX_TOOLWINDOW.
	newExStyle := (int32(exStyle) | WS_EX_LAYERED | WS_EX_TRANSPARENT | WS_EX_TOOLWINDOW) &^ WS_EX_APPWINDOW

	r2, _, e4 := procSetWindowLong.Call(hwnd, uintptr(GWL_EXSTYLE), uintptr(newExStyle))
	logf("win32tweak", "SetWindowLongW(GWL_EXSTYLE, 0x%X) -> prev=0x%X lastErr=%v", newExStyle, r2, e4)

	// Style changes don't take effect until you force a frame refresh.
	r3, _, e5 := procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, uintptr(SWP_NOMOVE|SWP_NOSIZE|SWP_FRAMECHANGED))
	logf("win32tweak", "SetWindowPos(frame refresh) -> ok=%v lastErr=%v", r3 != 0, e5)

	// Taskbar buttons are decided by Explorer at the moment a window is
	// first shown, and flipping WS_EX_TOOLWINDOW on an already-visible
	// window does NOT retroactively pull its existing taskbar button —
	// that's the actual cause of it still showing up there. Hiding and
	// re-showing the window forces Explorer to re-evaluate it with the new
	// ex-style already in place, which is what actually drops it.
	r4, _, e6 := procShowWindow.Call(hwnd, uintptr(SW_HIDE))
	logf("win32tweak", "ShowWindow(SW_HIDE) -> wasVisible=%v lastErr=%v", r4 != 0, e6)
	r5, _, e7 := procShowWindow.Call(hwnd, uintptr(SW_SHOW))
	logf("win32tweak", "ShowWindow(SW_SHOW) -> wasVisible=%v lastErr=%v", r5 != 0, e7)

	// WS_EX_LAYERED only marks the window as *capable* of being blended with
	// the desktop — it does nothing on its own. Windows still paints it fully
	// opaque until you explicitly hand it an alpha value. Reapplied here
	// (not just once earlier) because the hide/show cycle above can drop it.
	r6, _, e8 := procSetLayeredWindowAttributes.Call(hwnd, 0, uintptr(byte(WindowAlpha)), uintptr(LWA_ALPHA))
	logf("win32tweak", "SetLayeredWindowAttributes(alpha=%d) -> ok=%v lastErr=%v", WindowAlpha, r6 != 0, e8)

	// 3. Position at the top-left of the primary monitor. SetProcessDPIAware()
	// in main keeps these coordinates and dimensions in physical pixels.
	scale := float32(1.0)
	if overlayWindow != nil && overlayWindow.Canvas() != nil {
		scale = overlayWindow.Canvas().Scale()
	}
	pw := int(windowWidth * scale)
	ph := int(windowHeight * scale)
	posX := PaddingLeft
	posY := PaddingTop
	logf("win32tweak", "computed top-left position posX=%d posY=%d", posX, posY)

	// Strip SWP_NOSIZE so we explicitly resize the OS window down to match Fyne's client viewport.
	// This physically deletes the unrendered "grey border" that Windows leaves behind.
	r7, _, e10 := procSetWindowPos.Call(hwnd, HWND_TOPMOST, uintptr(posX), uintptr(posY), uintptr(pw), uintptr(ph), 0)
	logf("win32tweak", "SetWindowPos(topmost, %d, %d, w=%d, h=%d) -> ok=%v lastErr=%v", posX, posY, pw, ph, r7 != 0, e10)

	logf("win32tweak", "end, entering topmost-reassertion loop (every 500ms)")

	// 4. Keep re-asserting topmost so other apps can't steal the z-order.
	// Note: true fullscreen-exclusive games can still cover it — that's an
	// OS-level compositor limitation no always-on-top flag gets around —
	// but this handles borderless/windowed apps and everything else fine.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		n := 0
		for range ticker.C {
			n++
			// Reassert only the z-order. SWP_NOMOVE preserves API and drag positioning.
			r, _, e := procSetWindowPos.Call(hwnd, HWND_TOPMOST, 0, 0, 0, 0, uintptr(SWP_NOMOVE|SWP_NOSIZE))
			if n%20 == 1 { // log roughly every 10s, not every 500ms — avoid flooding the file
				logf("topmostloop", "tick #%d SetWindowPos -> ok=%v lastErr=%v", n, r != 0, e)
			}
		}
	}()
}

// --- BACKGROUND REST API ---
func readIntegerParameter(r *http.Request, name string) (int, error) {
	if value := r.URL.Query().Get(name); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		return parsed, nil
	}

	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("request must include %s in the query string or JSON body", name)
	}

	raw, ok := body[name]
	if !ok {
		return 0, fmt.Errorf("request must include %s", name)
	}

	var parsed int
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func startBackgroundAPI(listener net.Listener) {
	logf("http", "serving reserved listener on %s", listener.Addr())

	http.HandleFunc("/identity", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "GET required"})
			return
		}
		writeJSON(w, http.StatusOK, localNodeIdentity())
	})
	registerTimerCommands(http.DefaultServeMux, "", applyLocalAction)
	registerSpecialCommands(http.DefaultServeMux, "", appConfig.SpecialCommands, appConfig.CommandTimeoutSeconds,
		localRegistryPC(), &activityLog)
	registerOnBehalfHandlers(http.DefaultServeMux)

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		logf("http", "/status method=%s from=%s", r.Method, r.RemoteAddr)
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"status": "error", "message": "GET required"})
			return
		}

		status := localTimerStatus()
		writeJSON(w, http.StatusOK, struct {
			TimerStatus
			OnBehalfOf []TimerStatus `json:"on_behalf_of"`
		}{status, onBehalfStatuses()})
	})

	http.HandleFunc("/position", func(w http.ResponseWriter, r *http.Request) {
		logf("http", "/position method=%s from=%s", r.Method, r.RemoteAddr)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"status": "error", "message": "POST required"})
			return
		}

		var request struct {
			X *int `json:"x"`
			Y *int `json:"y"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.X == nil || request.Y == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": "JSON body must include integer x and y"})
			return
		}
		if err := moveOverlay(*request.X, *request.Y); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "x": *request.X, "y": *request.Y})
	})

	http.HandleFunc("/drag", func(w http.ResponseWriter, r *http.Request) {
		logf("http", "/drag method=%s from=%s", r.Method, r.RemoteAddr)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"status": "error", "message": "POST required"})
			return
		}

		var request struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Enabled == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": "JSON body must include boolean enabled"})
			return
		}
		if err := setDragEnabled(*request.Enabled); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "drag_enabled": *request.Enabled})
	})

	err := http.Serve(listener, cors(http.DefaultServeMux))
	logf("http", "Serve RETURNED (server stopped): %v", err)
}

func runClockTicker() {
	logf("ticker", "started")
	ticker := time.NewTicker(1 * time.Second)
	tick := 0
	for range ticker.C {
		tick++
		state.Lock()
		hitZero := state.tick()
		remaining := state.Remaining
		running := state.IsRunning
		state.Unlock()
		// Keep state unlocked before scheduling the render. This prevents UI
		// callbacks and HTTP/tray actions from deadlocking on the timer mutex.

		mins := remaining / 60
		secs := remaining % 60
		timeStr := fmt.Sprintf("%02d:%02d", mins, secs)

		// The ticker is a background goroutine. All Fyne object mutations must
		// be scheduled onto Fyne's UI thread; direct writes here can race the
		// renderer and are rejected by newer Fyne releases.
		fyne.Do(func() {
			if remaining == 0 {
				state.LabelObj.Text = "00:00"
				state.LabelObj.Color = color.RGBA{R: 255, G: 0, B: 0, A: 255}
			} else {
				state.LabelObj.Text = timeStr
				state.LabelObj.Color = color.RGBA{R: 0, G: 255, B: 0, A: 255}
			}
			state.LabelObj.Refresh()
			// Keep the window/background exactly around the rendered timer.
			if overlayWindow != nil {
				size := state.LabelObj.MinSize()
				state.LabelObj.Resize(size)
				if backgroundBox != nil {
					backgroundBox.Resize(size)
				}
				overlayWindow.Resize(size)

				// Keep the OS window completely tight to the Fyne viewport on every tick
				if overlayHwnd != 0 {
					scale := overlayWindow.Canvas().Scale()
					pw := int(size.Width * scale)
					ph := int(size.Height * scale)
					// 0x0002 = SWP_NOMOVE, 0x0004 = SWP_NOZORDER
					procSetWindowPos.Call(overlayHwnd, 0, 0, 0, uintptr(pw), uintptr(ph), uintptr(0x0002|0x0004))
				}
			}
		})

		if tick%5 == 1 { // don't flood the log every single second
			logf("ticker", "tick #%d remaining=%d running=%v hitZero=%v", tick, remaining, running, hitZero)
		}

		if hitZero {
			logf("ticker", "tick #%d hitZero=true, calling showTimeUp, begin", tick)
			showTimeUp()
			logf("ticker", "tick #%d showTimeUp returned", tick)
		}
	}
}

func applyLocalAction(action string, value int) int {
	state.Lock()
	state.apply(action, value)
	remaining := state.Remaining
	state.Unlock()
	logf("timer", "action=%s value=%d remaining=%d", action, value, remaining)
	activityLog.append("timer_command", PC{Name: configuredFriendlyName()}, action,
		fmt.Sprint(value), "success", http.StatusOK, fmt.Sprintf("remaining_seconds=%d", remaining))
	if action == "show" {
		showTimeUpForced()
	} else {
		hideTimeUp()
	}
	return remaining
}

func setRunning(running bool) {
	action := "pause"
	if running {
		action = "play"
	}
	applyLocalAction(action, 0)
}

func addTime(seconds int) int { return applyLocalAction("tweak", seconds) }
func resetTimer()             { applyLocalAction("reset", 0) }

func showTimeUpForced() { displayTimeUp(true) }
func showTimeUp()       { displayTimeUp(false) }

func displayTimeUp(forced bool) {
	state.Lock()
	if state.TimeUpVisible || (!forced && (state.Remaining != 0 || !state.IsRunning)) {
		state.Unlock()
		return
	}
	state.TimeUpVisible = true
	state.Unlock()
	if timeUpWindow == nil {
		return
	}
	if !forced {
		closeConfiguredAppsOnTimeUp()
	}
	minimizeAllWindows()
	fyne.Do(func() {
		// Show before fullscreen so Fyne creates and lays out the native window.
		timeUpWindow.Resize(fyne.NewSize(800, 600))
		timeUpWindow.Show()
		timeUpWindow.SetFullScreen(true)
		timeUpWindow.RequestFocus()
	})
}

// hideTimeUp dismisses the full-screen alert. Called both from the UI
// "Minimize" button and automatically from every action that changes timer
// state (add time, pause, play, reset) — per the requirement that any of
// those should pull the alert out of the way on their own.
func hideTimeUp() {
	logf("hideTimeUp", "begin, waiting for lock")
	state.Lock()
	logf("hideTimeUp", "lock acquired, TimeUpVisible=%v", state.TimeUpVisible)
	wasVisible := state.TimeUpVisible
	state.TimeUpVisible = false
	state.Unlock()
	logf("hideTimeUp", "lock released")

	if !wasVisible || timeUpWindow == nil {
		logf("hideTimeUp", "end (no-op) wasVisible=%v windowNil=%v", wasVisible, timeUpWindow == nil)
		return
	}
	// Restore while TIME UP still covers the desktop, then remove the alert.
	restoreMinimizedWindows()
	logf("hideTimeUp", "scheduling Hide on Fyne UI thread")
	fyne.Do(func() {
		timeUpWindow.SetFullScreen(false)
		timeUpWindow.Hide()
		logf("hideTimeUp", "Hide completed")
	})
}

// buildTimeUpWindow creates (but does not show) the full-screen "TIME UP"
// alert. It's a normal Fyne window, not the chrome-stripped overlay — when
// SetFullScreen(true) is applied Windows removes the titlebar anyway, so the
// in-UI Minimize button is what makes it dismissible.
func buildTimeUpWindow(myApp fyne.App) fyne.Window {
	logf("buildTimeUpWindow", "begin")
	w := myApp.NewWindow(TimeUpTitle)

	bg := canvas.NewRectangle(color.RGBA{R: 10, G: 10, B: 10, A: 255})

	label := canvas.NewText("TIME UP", color.RGBA{R: 255, G: 50, B: 50, A: 255})
	label.TextSize = 96
	label.TextStyle = fyne.TextStyle{Bold: true}
	label.Alignment = fyne.TextAlignCenter

	minimizeBtn := widget.NewButton("Minimize", func() {
		logf("timeUpWindow", "Minimize button clicked, begin")
		hideTimeUp()
		logf("timeUpWindow", "Minimize button - hideTimeUp returned")
	})

	// Keep both controls in one centered layout. The previous Border layout
	// split them between independently sized regions and could be initialized
	// incorrectly when the hidden window entered fullscreen for the first time.
	alert := container.NewVBox(label, container.NewCenter(minimizeBtn))
	w.SetContent(container.NewMax(bg, container.NewCenter(alert)))
	w.SetCloseIntercept(func() {
		logf("timeUpWindow", "close intercepted (Alt+F4/X), begin")
		hideTimeUp()
		logf("timeUpWindow", "close intercept - hideTimeUp returned")
	})
	logf("buildTimeUpWindow", "end")
	return w
}

// setupSystemTray adds a tray icon with Pause/Play/Add-time/Reset/Quit — the
// only way to control or close the app once it has no titlebar and no
// taskbar entry. desktop.App is an optional interface: on Windows/macOS/
// Linux desktop builds *fyne.App satisfies it, so this type assertion
// always succeeds here; it's just the idiomatic Fyne way to reach the tray
// APIs.
func setupSystemTray(myApp fyne.App) {
	logf("tray", "setupSystemTray begin")
	desk, ok := myApp.(desktop.App)
	if !ok {
		logf("tray", "ABORT: app does not implement desktop.App on this platform")
		return
	}

	pauseItem := fyne.NewMenuItem("Pause", func() {
		logf("tray", "Pause clicked, begin")
		setRunning(false)
		logf("tray", "Pause clicked, setRunning returned")
	})
	playItem := fyne.NewMenuItem("Play", func() {
		logf("tray", "Play clicked, begin")
		setRunning(true)
		logf("tray", "Play clicked, setRunning returned")
	})
	resetItem := fyne.NewMenuItem("Reset", func() {
		logf("tray", "Reset clicked, begin")
		resetTimer()
		logf("tray", "Reset clicked, resetTimer returned")
	})
	enableDragItem := fyne.NewMenuItem("Enable dragging", func() {
		if err := setDragEnabled(true); err != nil {
			logf("tray", "Enable dragging failed: %v", err)
		}
	})
	disableDragItem := fyne.NewMenuItem("Disable dragging (click-through)", func() {
		if err := setDragEnabled(false); err != nil {
			logf("tray", "Disable dragging failed: %v", err)
		}
	})
	quitItem := fyne.NewMenuItem("Quit", func() {
		logf("tray", "Quit clicked, scheduling app.Quit()")
		fyne.Do(func() { myApp.Quit() })
	})

	addTimeOption := func(label string, minutes int) *fyne.MenuItem {
		return fyne.NewMenuItem(label, func() {
			logf("tray", "%s clicked, begin", label)
			addTime(minutes * 60)
			logf("tray", "%s clicked, addTime returned", label)
		})
	}

	// Deliberately flat, not nested under an "Add Time" submenu. Fyne's
	// Windows systray backend has known reliability problems with nested
	// ChildMenu items, so flat top-level items sidestep that path entirely.
	menu := fyne.NewMenu(WindowTitle,
		pauseItem,
		playItem,
		fyne.NewMenuItemSeparator(),
		addTimeOption("Add 5 min", 5),
		addTimeOption("Add 15 min", 15),
		addTimeOption("Add 20 min", 20),
		addTimeOption("Add 30 min", 30),
		addTimeOption("Add 45 min", 45),
		addTimeOption("Add 60 min", 60),
		fyne.NewMenuItemSeparator(),
		resetItem,
		fyne.NewMenuItemSeparator(),
		enableDragItem,
		disableDragItem,
		fyne.NewMenuItemSeparator(),
		quitItem,
	)

	desk.SetSystemTrayMenu(menu)
	desk.SetSystemTrayIcon(theme.MediaPlayIcon())
	myApp.SetIcon(theme.MediaPlayIcon())
	logf("tray", "setupSystemTray end")
}

func main() {
	logPath := initLogging()
	if activeLogWriter != nil {
		defer activeLogWriter.Close()
	}
	logf("main", "=== overlay_timer starting, pid=%d, log file: %s ===", os.Getpid(), logPath)
	configPath := initConfig()
	logf("main", "config file: %s", configPath)
	apiListener, uiListener, ok := acquireStartupResources()
	if !ok {
		logf("main", "startup aborted; check port errors or another running instance")
		return
	}
	defer apiListener.Close()
	defer func() {
		if instanceMutexHandle != 0 {
			procCloseHandle.Call(instanceMutexHandle)
		}
	}()
	defer uiListener.Close()
	initializeOnBehalfTimers()
	go startManagementService(uiListener)

	// Must run before any GetSystemMetrics/positioning call, or the values
	// come back scaled on HiDPI monitors and the overlay lands in the wrong
	// spot.
	r, _, e := procSetProcessDPIAware.Call()
	logf("main", "SetProcessDPIAware() -> ok=%v lastErr=%v", r != 0, e)

	logf("main", "app.New() begin")
	myApp := app.New()
	logf("main", "app.New() done")

	setupSystemTray(myApp)
	timeUpWindow = buildTimeUpWindow(myApp)

	logf("main", "creating main overlay window")
	myWindow := myApp.NewWindow(WindowTitle)
	overlayWindow = myWindow

	textLabel := canvas.NewText("01:00", color.RGBA{R: 0, G: 255, B: 0, A: 255})
	textLabel.TextSize = TimerTextSize
	textLabel.TextStyle = fyne.TextStyle{Bold: true}
	textLabel.Alignment = fyne.TextAlignCenter
	state.LabelObj = textLabel

	// Drawn fully opaque on purpose: the actual see-through effect now comes
	// from the OS-level SetLayeredWindowAttributes call in
	// applyWindowsWindowTweaks, which dims the *whole* window (panel + text)
	// uniformly against the desktop. Giving this rectangle its own partial
	// alpha on top of that would just double-darken and muddy it.
	backgroundBox = canvas.NewRectangle(color.RGBA{R: 0, G: 0, B: 0, A: 255})

	// We revert to NewWithoutLayout because NewMax was cropping the text.
	// Fyne text rendering has built in ascender/descender padding that NewMax ignores.
	dragLayer := newDragSurface()
	contentLayout := container.NewWithoutLayout(backgroundBox, textLabel, dragLayer)
	textSize := textLabel.MinSize()
	backgroundBox.Resize(textSize)
	textLabel.Resize(textSize)
	dragLayer.Resize(textSize)
	backgroundBox.Move(fyne.NewPos(0, 0))
	textLabel.Move(fyne.NewPos(0, 0))
	dragLayer.Move(fyne.NewPos(0, 0))

	myWindow.SetContent(contentLayout)
	myWindow.SetPadded(false)
	timerSize := textLabel.MinSize()
	myWindow.Resize(timerSize)
	myWindow.SetFixedSize(true)

	// The overlay is the app's master window. Never let WM_CLOSE (including
	// "Close window" from a transient taskbar button or Alt+F4) destroy it;
	// Quit in the tray is the one intentional process-exit path.
	myWindow.SetCloseIntercept(func() {
		logf("mainWindow", "close intercepted; keeping overlay and process alive")
	})

	// Start polling immediately. The HWND only exists once ShowAndRun begins,
	// but avoiding a fixed delay minimizes any transient taskbar appearance.
	go applyWindowsWindowTweaks(timerSize.Width, timerSize.Height)

	go startBackgroundAPI(apiListener)
	go runClockTicker()

	logf("main", "entering myWindow.ShowAndRun() — main goroutine blocks here until quit")
	myWindow.ShowAndRun()
	logf("main", "ShowAndRun() returned — app is exiting")
}
