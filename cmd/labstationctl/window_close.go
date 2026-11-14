package main

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var wmctrlBinary = "/usr/bin/wmctrl"
var xdotoolBinary = "/usr/bin/xdotool"

var windowIDPattern = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)

func requestApplicationWindowClose(processGroupIDs []int, deadline time.Time) bool {
	if len(processGroupIDs) == 0 || !time.Now().Before(deadline) {
		return false
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	output, err := exec.CommandContext(ctx, wmctrlBinary, "-lp").Output()
	if err != nil {
		return false
	}
	groups := make(map[int]struct{}, len(processGroupIDs))
	for _, pid := range processGroupIDs {
		if pid > 0 {
			groups[pid] = struct{}{}
		}
	}
	windowIDs := windowsForApplicationGroups(output, groups, syscall.Getpgid)
	if len(windowIDs) == 0 {
		return false
	}

	var requests sync.WaitGroup
	accepted := make(chan bool, len(windowIDs))
	for _, windowID := range windowIDs {
		windowID := windowID
		requests.Add(1)
		go func() {
			defer requests.Done()
			accepted <- exec.CommandContext(ctx, wmctrlBinary, "-ic", windowID).Run() == nil
		}()
	}
	requests.Wait()
	for range windowIDs {
		if <-accepted {
			return true
		}
	}
	return false
}

func windowsForApplicationGroups(output []byte, processGroups map[int]struct{}, processGroupForPID func(int) (int, error)) []string {
	seen := make(map[string]struct{})
	var windowIDs []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !windowIDPattern.MatchString(fields[0]) {
			continue
		}
		windowPID, err := strconv.Atoi(fields[2])
		if err != nil || windowPID <= 0 {
			continue
		}
		processGroup, err := processGroupForPID(windowPID)
		if err != nil {
			continue
		}
		if _, belongsToApplication := processGroups[processGroup]; !belongsToApplication {
			continue
		}
		if _, alreadyAdded := seen[fields[0]]; alreadyAdded {
			continue
		}
		seen[fields[0]] = struct{}{}
		windowIDs = append(windowIDs, fields[0])
	}
	return windowIDs
}

type applicationWindow struct {
	id     string
	width  int
	height int
}

type windowGeometry struct {
	x      int
	y      int
	width  int
	height int
}

type mouseLocation struct {
	x        int
	y        int
	windowID string
}

func clickPrimaryApplicationCloseCoordinates(processGroupID int, coordinates []int, deadline time.Time) bool {
	if processGroupID <= 0 || len(coordinates) != 2 || !time.Now().Before(deadline) {
		return false
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	output, err := exec.CommandContext(ctx, wmctrlBinary, "-lpG").Output()
	if err != nil {
		return false
	}
	windows := windowsForSingleApplicationGroup(output, processGroupID)
	if len(windows) != 1 {
		return false
	}
	window := windows[0]
	if !windowIDPattern.MatchString(window.id) || coordinates[0] < 0 || coordinates[1] < 0 || coordinates[0] >= window.width || coordinates[1] >= window.height {
		return false
	}

	activeWindow, err := activeWindowID(ctx)
	if err != nil || !sameWindowID(activeWindow, window.id) {
		return false
	}
	geometry, err := exec.CommandContext(ctx, xdotoolBinary, "getwindowgeometry", "--shell", window.id).Output()
	if err != nil {
		return false
	}
	windowBounds, ok := parseWindowGeometry(geometry)
	if !ok || coordinates[0] >= windowBounds.width || coordinates[1] >= windowBounds.height {
		return false
	}
	activeWindow, err = activeWindowID(ctx)
	if err != nil || !sameWindowID(activeWindow, window.id) {
		return false
	}
	moveArgs := []string{"mousemove", "--sync", "--window", window.id, strconv.Itoa(coordinates[0]), strconv.Itoa(coordinates[1])}
	if err := exec.CommandContext(ctx, xdotoolBinary, moveArgs...).Run(); err != nil {
		return false
	}
	activeWindow, err = activeWindowID(ctx)
	if err != nil || !sameWindowID(activeWindow, window.id) {
		return false
	}
	mouseLocation, err := exec.CommandContext(ctx, xdotoolBinary, "getmouselocation", "--shell").Output()
	if err != nil {
		return false
	}
	hit, ok := parseMouseLocation(mouseLocation)
	if !ok || hit.x != windowBounds.x+coordinates[0] || hit.y != windowBounds.y+coordinates[1] {
		return false
	}
	hitPIDOutput, err := exec.CommandContext(ctx, xdotoolBinary, "getwindowpid", hit.windowID).Output()
	if err != nil {
		return false
	}
	hitPID, err := strconv.Atoi(strings.TrimSpace(string(hitPIDOutput)))
	if err != nil || hitPID <= 0 {
		return false
	}
	hitProcessGroup, err := syscall.Getpgid(hitPID)
	if err != nil || hitProcessGroup != processGroupID {
		return false
	}
	return exec.CommandContext(ctx, xdotoolBinary, "click", "1").Run() == nil
}

func windowsForSingleApplicationGroup(output []byte, processGroupID int) []applicationWindow {
	var windows []applicationWindow
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || !windowIDPattern.MatchString(fields[0]) {
			continue
		}
		windowPID, err := strconv.Atoi(fields[2])
		if err != nil || windowPID <= 0 {
			continue
		}
		group, err := syscall.Getpgid(windowPID)
		if err != nil || group != processGroupID {
			continue
		}
		width, widthErr := strconv.Atoi(fields[5])
		height, heightErr := strconv.Atoi(fields[6])
		if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
			continue
		}
		windows = append(windows, applicationWindow{id: fields[0], width: width, height: height})
	}
	return windows
}

func activeWindowID(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, xdotoolBinary, "getactivewindow").Output()
	return strings.TrimSpace(string(output)), err
}

func parseMouseLocation(output []byte) (mouseLocation, bool) {
	var location mouseLocation
	var hasX, hasY bool
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "X":
			parsed, err := strconv.Atoi(value)
			location.x, hasX = parsed, err == nil
		case "Y":
			parsed, err := strconv.Atoi(value)
			location.y, hasY = parsed, err == nil
		case "WINDOW":
			location.windowID = value
		}
	}
	return location, hasX && hasY && validWindowID(location.windowID)
}

func sameWindowID(first, second string) bool {
	firstValue, firstErr := strconv.ParseUint(first, 0, 64)
	secondValue, secondErr := strconv.ParseUint(second, 0, 64)
	return firstErr == nil && secondErr == nil && firstValue == secondValue
}

func parseWindowGeometry(output []byte) (windowGeometry, bool) {
	var geometry windowGeometry
	var hasX, hasY, hasWidth, hasHeight bool
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "X":
			geometry.x, hasX = parsed, true
		case "Y":
			geometry.y, hasY = parsed, true
		case "WIDTH":
			geometry.width, hasWidth = parsed, true
		case "HEIGHT":
			geometry.height, hasHeight = parsed, true
		}
	}
	return geometry, hasX && hasY && hasWidth && hasHeight && geometry.width > 0 && geometry.height > 0
}

func validWindowID(value string) bool {
	if windowIDPattern.MatchString(value) {
		return true
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0
}

func closeApplicationProcessGroups(processGroupIDs []int, timeout time.Duration, primaryCloseCoordinates []int) {
	if len(processGroupIDs) == 0 {
		return
	}
	started := time.Now()
	if timeout <= 0 || timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	deadline := started.Add(timeout)
	termAt := started.Add(timeout * 2 / 3)
	coordinateDeadline := started.Add(timeout / 4)
	if coordinateDeadline.After(termAt) {
		coordinateDeadline = termAt
	}
	if len(primaryCloseCoordinates) == 2 && clickPrimaryApplicationCloseCoordinates(processGroupIDs[0], primaryCloseCoordinates, coordinateDeadline) {
		waitForProcessGroups(processGroupIDs[:1], coordinateDeadline)
	}
	requested := requestApplicationWindowClose(processGroupIDs, termAt)
	if requested {
		waitForProcessGroups(processGroupIDs, termAt)
	}

	for _, pid := range processGroupIDs {
		if processGroupExists(pid) {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
	}
	waitForProcessGroups(processGroupIDs, deadline)
	for _, pid := range processGroupIDs {
		if processGroupExists(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
}

func waitForProcessGroups(processGroupIDs []int, deadline time.Time) {
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range processGroupIDs {
			if processGroupExists(pid) {
				alive = true
				break
			}
		}
		if !alive {
			return
		}
		remaining := time.Until(deadline)
		if remaining > 50*time.Millisecond {
			remaining = 50 * time.Millisecond
		}
		time.Sleep(remaining)
	}
}

func processGroupExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || err == syscall.EPERM
}
