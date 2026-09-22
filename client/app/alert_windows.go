//go:build windows

package main

import (
	"log"
	"os"

	"golang.org/x/sys/windows"
)

func fatal(message string) {
	log.Print(message)
	text, _ := windows.UTF16PtrFromString(message)
	caption, _ := windows.UTF16PtrFromString("peerly")
	windows.MessageBox(0, text, caption, windows.MB_OK|windows.MB_ICONERROR)
	os.Exit(1)
}
