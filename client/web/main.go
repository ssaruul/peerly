package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"peerly/client/core"
	"peerly/client/ui"
)

func openBrowser(url string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		command = exec.Command("open", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if err := command.Start(); err != nil {
		log.Printf("open %s in your browser", url)
		return
	}
	go command.Wait()
}

func alreadyRunning(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(url + "/api/ping")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	answer := map[string]string{}
	return json.NewDecoder(response.Body).Decode(&answer) == nil && answer["app"] == "peerly"
}

func main() {
	port := flag.Int("port", 47800, "local port for the interface")
	noBrowser := flag.Bool("no-browser", false, "do not open the browser automatically")
	flag.Parse()

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
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	}

	keyPath := filepath.Join(configDir, "ui-key")
	fixedURL := fmt.Sprintf("http://127.0.0.1:%d", *port)
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		if alreadyRunning(fixedURL) {
			log.Printf("peerly is already running, opening %s", fixedURL)
			runningKey, _ := os.ReadFile(keyPath)
			if !*noBrowser {
				openBrowser(fixedURL + "/#key=" + string(runningKey))
			}
			return
		}
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
	}
	keyBytes := make([]byte, 24)
	if _, err := rand.Read(keyBytes); err != nil {
		log.Fatal(err)
	}
	accessKey := hex.EncodeToString(keyBytes)
	if err := os.WriteFile(keyPath, []byte(accessKey), 0o600); err != nil {
		log.Fatal(err)
	}
	unlock, err := core.LockDir(configDir)
	if err != nil {
		listener.Close()
		log.Fatal(err)
	}
	defer unlock()
	if err := config.CleanTemp(); err != nil {
		log.Printf("could not clean temporary files: %v", err)
	}
	app := ui.New(config)
	app.AccessKey = accessKey
	url := "http://" + listener.Addr().String() + "/#key=" + accessKey
	server := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	log.Printf("peerly is running at %s, keep this window open while you host", url)
	if !*noBrowser {
		openBrowser(url)
	}

	interrupted, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-interrupted.Done()
	if app.HostingActive() {
		log.Print("closing: uploading the current save first, press Ctrl+C again to skip")
		stop()
		second := make(chan os.Signal, 1)
		signal.Notify(second, os.Interrupt, syscall.SIGTERM)
		finished := make(chan struct{})
		go func() {
			app.StopAndWait(2 * time.Hour)
			close(finished)
		}()
		select {
		case <-finished:
		case <-second:
			app.StopAndWait(15 * time.Second)
		}
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(shutdownContext)
}
