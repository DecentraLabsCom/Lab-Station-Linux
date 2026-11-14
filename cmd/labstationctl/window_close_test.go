package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWindowsForApplicationGroupsSelectsOnlyMatchingWindowPIDs(t *testing.T) {
	output := []byte("0x010 0 101 host first app\n0x020 0 202 host other app\n0x030 0 303 host descendant\n")
	groups := map[int]struct{}{101: {}, 303: {}}
	got := windowsForApplicationGroups(output, groups, func(pid int) (int, error) {
		return pid, nil
	})
	if strings.Join(got, ",") != "0x010,0x030" {
		t.Fatalf("selected windows = %v, want only the two application windows", got)
	}
}

func TestRequestApplicationWindowCloseUsesWindowManagerCloseRequest(t *testing.T) {
	child := exec.Command("sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Skipf("cannot start isolated process group: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		_ = child.Wait()
	})

	directory := t.TempDir()
	marker := filepath.Join(directory, "close-requested")
	fakeWMctrl := filepath.Join(directory, "wmctrl")
	contents := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-lp\" ]; then printf '0x010 0 %s host target\\n' \"$APP_PID\"; exit 0; fi\n" +
		"if [ \"$1\" = \"-ic\" ] && [ \"$2\" = \"0x010\" ]; then touch \"$CLOSE_MARKER\"; kill \"$APP_PID\"; exit 0; fi\n" +
		"exit 1\n"
	if err := os.WriteFile(fakeWMctrl, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	oldWMctrl := wmctrlBinary
	wmctrlBinary = fakeWMctrl
	t.Cleanup(func() { wmctrlBinary = oldWMctrl })
	t.Setenv("APP_PID", strconv.Itoa(child.Process.Pid))
	t.Setenv("CLOSE_MARKER", marker)

	requestApplicationWindowClose([]int{child.Process.Pid}, time.Now().Add(2*time.Second))
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("window manager did not receive a close request: %v", err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("fake window manager did not close the selected application")
	}
}

func TestClickPrimaryApplicationCloseCoordinatesRequiresUniqueActiveAppWindow(t *testing.T) {
	child := exec.Command("sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Skipf("cannot start isolated process group: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		_ = child.Wait()
	})

	directory := t.TempDir()
	marker := filepath.Join(directory, "coordinate-click")
	fakeWMctrl := filepath.Join(directory, "wmctrl")
	if err := os.WriteFile(fakeWMctrl, []byte("#!/bin/sh\n[ \"$1\" = \"-lpG\" ] || exit 1\nprintf '0x010 0 %s 0 0 800 600 host target\\n' \"$APP_PID\"\nif [ \"$WINDOW_COUNT\" = \"2\" ]; then printf '0x011 0 %s 0 0 800 600 host second\\n' \"$APP_PID\"; fi\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeXdotool := filepath.Join(directory, "xdotool")
	if err := os.WriteFile(fakeXdotool, []byte("#!/bin/sh\nif [ \"$1\" = \"getactivewindow\" ]; then echo \"$ACTIVE_WINDOW\"; exit 0; fi\nif [ \"$1\" = \"getwindowgeometry\" ]; then printf 'WINDOW=0x010\\nX=0\\nY=0\\nWIDTH=800\\nHEIGHT=600\\nSCREEN=0\\n'; exit 0; fi\nif [ \"$1\" = \"getmouselocation\" ]; then printf 'X=394\\nY=395\\nSCREEN=0\\nWINDOW=16\\n'; exit 0; fi\nif [ \"$1\" = \"getwindowpid\" ]; then echo \"$POINTER_PID\"; exit 0; fi\nprintf '%s\\n' \"$*\" >> \"$CLICK_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldWMctrl, oldXdotool := wmctrlBinary, xdotoolBinary
	wmctrlBinary, xdotoolBinary = fakeWMctrl, fakeXdotool
	t.Cleanup(func() { wmctrlBinary, xdotoolBinary = oldWMctrl, oldXdotool })
	t.Setenv("APP_PID", strconv.Itoa(child.Process.Pid))
	t.Setenv("POINTER_PID", strconv.Itoa(child.Process.Pid))
	t.Setenv("ACTIVE_WINDOW", "0x010")
	t.Setenv("CLICK_MARKER", marker)
	t.Setenv("WINDOW_COUNT", "1")

	if !clickPrimaryApplicationCloseCoordinates(child.Process.Pid, []int{394, 395}, time.Now().Add(time.Second)) {
		t.Fatal("active primary application window did not receive its configured click")
	}
	contents, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("coordinate click command was not run: %v", err)
	}
	if got := strings.TrimSpace(string(contents)); got != "mousemove --sync --window 0x010 394 395\nclick 1" {
		t.Fatalf("xdotool actions = %q", got)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	coveredWindow := exec.Command("sleep", "30")
	coveredWindow.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := coveredWindow.Start(); err != nil {
		t.Skipf("cannot start second isolated process group: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-coveredWindow.Process.Pid, syscall.SIGKILL)
		_ = coveredWindow.Wait()
	})
	t.Setenv("POINTER_PID", strconv.Itoa(coveredWindow.Process.Pid))
	if clickPrimaryApplicationCloseCoordinates(child.Process.Pid, []int{394, 395}, time.Now().Add(time.Second)) {
		t.Fatal("coordinate click was sent when another app process was under the pointer")
	}
	contents, err = os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(contents)) != "mousemove --sync --window 0x010 394 395" {
		t.Fatalf("covered application was clicked: actions = %q, error = %v", contents, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POINTER_PID", strconv.Itoa(child.Process.Pid))
	t.Setenv("ACTIVE_WINDOW", "0x020")
	if clickPrimaryApplicationCloseCoordinates(child.Process.Pid, []int{394, 395}, time.Now().Add(time.Second)) {
		t.Fatal("coordinate click was attempted while another window was active")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("inactive primary app received a click: %v", err)
	}

	t.Setenv("ACTIVE_WINDOW", "0x010")
	t.Setenv("WINDOW_COUNT", "2")
	if clickPrimaryApplicationCloseCoordinates(child.Process.Pid, []int{394, 395}, time.Now().Add(time.Second)) {
		t.Fatal("coordinate click was attempted when the app process group owned multiple windows")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("ambiguous app window received a click: %v", err)
	}
}

func TestParseMouseLocationRequiresCompleteAbsolutePositionAndWindow(t *testing.T) {
	location, ok := parseMouseLocation([]byte("X=434\nY=215\nSCREEN=0\nWINDOW=16777250\n"))
	if !ok || location.x != 434 || location.y != 215 || location.windowID != "16777250" {
		t.Fatalf("mouse location = %#v, valid = %t", location, ok)
	}
	if _, ok := parseMouseLocation([]byte("X=434\nY=215\nSCREEN=0\nWINDOW=bad\n")); ok {
		t.Fatal("mouse location without a valid X window ID was accepted")
	}
}

func TestApplicationCloseTimeoutHasTenSecondMaximum(t *testing.T) {
	if got := applicationCloseTimeout(15); got != 10*time.Second {
		t.Fatalf("configured timeout was not capped: %s", got)
	}
	if got := applicationCloseTimeout(4); got != 4*time.Second {
		t.Fatalf("short configured timeout changed: %s", got)
	}
}
