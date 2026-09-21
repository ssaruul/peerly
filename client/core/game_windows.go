//go:build windows

package core

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

func launchCommand(launch string, wait bool) (*exec.Cmd, error) {
	launch = strings.TrimSpace(launch)
	starter := `cmd /C start "" `
	if wait {
		starter = `cmd /C start /WAIT "" `
	}
	commandLine := starter + launch
	unquoted := strings.Trim(launch, `"`)
	if IsLink(launch) {
		if strings.ContainsAny(launch, `"^%`) {
			return nil, errors.New("the launch link contains characters that are not allowed")
		}
		commandLine = starter + `"` + launch + `"`
	} else if info, err := os.Stat(unquoted); err == nil && !info.IsDir() {
		commandLine = starter + `/D "` + filepath.Dir(unquoted) + `" "` + unquoted + `"`
	}
	command := exec.Command("cmd")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow, CmdLine: commandLine}
	return command, nil
}

func normalizeProcessName(name string) string {
	name = strings.Trim(strings.TrimSpace(name), `"`)
	name = filepath.Base(strings.ReplaceAll(name, "/", `\`))
	if filepath.Ext(name) == "" {
		name += ".exe"
	}
	return name
}

func processRunning(name string) (bool, error) {
	wanted := normalizeProcessName(name)
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), wanted) {
			return true, nil
		}
		err = windows.Process32Next(snapshot, &entry)
	}
	if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return false, nil
	}
	return false, err
}
