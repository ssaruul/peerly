//go:build !windows

package core

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func launchCommand(launch string, wait bool) (*exec.Cmd, error) {
	launch = strings.TrimSpace(launch)
	if IsLink(launch) {
		return exec.Command("xdg-open", launch), nil
	}
	return exec.Command("sh", "-c", launch), nil
}

func normalizeProcessName(name string) string {
	name = strings.Trim(strings.TrimSpace(name), `"`)
	return filepath.Base(strings.ReplaceAll(name, `\`, "/"))
}

func processRunning(name string) (bool, error) {
	processDirs, err := filepath.Glob("/proc/[0-9]*")
	if err != nil {
		return false, err
	}
	wanted := strings.ToLower(normalizeProcessName(name))
	for _, processDir := range processDirs {
		if stat, err := os.ReadFile(filepath.Join(processDir, "stat")); err == nil {
			if _, afterName, found := bytes.Cut(stat, []byte(") ")); found && bytes.HasPrefix(afterName, []byte("Z")) {
				continue
			}
		}
		if comm, err := os.ReadFile(filepath.Join(processDir, "comm")); err == nil {
			if strings.ToLower(strings.TrimSpace(string(comm))) == wanted {
				return true, nil
			}
		}
		if cmdline, err := os.ReadFile(filepath.Join(processDir, "cmdline")); err == nil {
			executable, _, _ := bytes.Cut(cmdline, []byte{0})
			normalized := strings.ReplaceAll(string(executable), `\`, "/")
			if strings.ToLower(filepath.Base(normalized)) == wanted {
				return true, nil
			}
		}
	}
	return false, nil
}
