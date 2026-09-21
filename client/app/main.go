//go:build windows

package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"peerly/client/core"
	"peerly/client/ui"
)

func main() {
	configDir, err := core.DefaultConfigDir()
	if err != nil {
		log.Fatal(err)
	}
	config, err := core.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
	}
	if logFile, err := os.OpenFile(filepath.Join(configDir, "peerly.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		defer logFile.Close()
		log.SetOutput(logFile)
	}
	if err := config.CleanTemp(); err != nil {
		log.Printf("could not clean temporary files: %v", err)
	}
	app := ui.New(config)
	var window context.Context
	err = wails.Run(&options.App{
		Title:       "peerly",
		Width:       920,
		Height:      780,
		MinWidth:    520,
		MinHeight:   480,
		AssetServer: &assetserver.Options{Handler: app.Handler()},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "peerly-desktop-7c1f4e0a-52b9-4d8e-9a3b-6f2d1c8e5b70",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				if window != nil {
					wailsruntime.WindowUnminimise(window)
					wailsruntime.Show(window)
				}
			},
		},
		OnStartup: func(ctx context.Context) {
			window = ctx
		},
		OnBeforeClose: func(ctx context.Context) bool {
			if !app.HostingActive() {
				return false
			}
			wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
				Type:    wailsruntime.WarningDialog,
				Title:   "You are still hosting",
				Message: "Close the game and wait until peerly says the world is free, or press Stop hosting. Then close this window.\n\nIf peerly closes now, your friends do not get your progress.",
			})
			return true
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
