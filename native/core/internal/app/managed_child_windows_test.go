//go:build windows

package app

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestManagedChildJobKillsDescendantsWhenOwnerExits(t *testing.T) {
	owner := exec.Command(os.Args[0], "-test.run=^TestManagedChildJobHelperProcess$")
	owner.Env = append(os.Environ(), "MUSICOPS_MANAGED_CHILD_HELPER=owner")
	stdout, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	owner.Stderr = os.Stderr
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if owner.Process != nil {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read helper pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid helper pid %q: %v", line, err)
	}
	if !processAlive(pid) {
		t.Fatalf("managed child %d was not alive", pid)
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("managed child %d survived owner exit", pid)
	}
}

func TestManagedChildJobHelperProcess(t *testing.T) {
	if os.Getenv("MUSICOPS_MANAGED_CHILD_HELPER") != "owner" {
		return
	}
	if err := prepareManagedChild(); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestWindowsInstallerHelperProcess$")
	child.Env = append(os.Environ(), "MUSICOPS_INSTALLER_SLEEP_HELPER=1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := attachManagedChild(child.Process); err != nil {
		t.Fatal(err)
	}
	_, _ = os.Stdout.WriteString(strconv.Itoa(child.Process.Pid) + "\n")
	for {
		time.Sleep(time.Hour)
	}
}

func processAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == 259
}
