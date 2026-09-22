package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"peerly/server/api"
	"peerly/server/blobs"
	"peerly/server/store"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8787", "listen address")
	dataDir := flag.String("data", "./data", "directory for the database and save blobs")
	maxUploadMB := flag.Int64("max-upload-mb", 2048, "largest accepted save upload in MiB")
	backupDir := flag.String("backup", "", "write a consistent copy of the database and saves into this directory and exit")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o750); err != nil {
		log.Fatal(err)
	}
	database, err := store.Open(filepath.Join(*dataDir, "peerly.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()
	if *backupDir != "" {
		if err := backup(database, *dataDir, *backupDir); err != nil {
			log.Fatal(err)
		}
		return
	}
	blobDir, err := blobs.Open(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	referenced, err := database.ReferencedBlobs(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	if removed, err := blobDir.Sweep(referenced); err != nil {
		log.Printf("blob sweep failed: %v", err)
	} else if removed > 0 {
		log.Printf("removed %d save files no revision refers to", removed)
	}
	adminKey := os.Getenv("PEERLY_ADMIN_KEY")
	if adminKey == "" {
		log.Print("PEERLY_ADMIN_KEY is not set: anyone who can reach this server can create groups")
	}
	server := &api.Server{Store: database, Blobs: blobDir, AdminKey: adminKey, MaxUpload: *maxUploadMB << 20}
	httpServer := &http.Server{
		Addr:              *address,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("peerly server listening on %s, data in %s", *address, *dataDir)
	log.Fatal(httpServer.ListenAndServe())
}

func backup(database *store.Store, dataDir string, target string) error {
	stamp := time.Now().Format("20060102-150405")
	destination := filepath.Join(target, "peerly-"+stamp)
	if err := os.MkdirAll(filepath.Join(destination, "blobs"), 0o750); err != nil {
		return err
	}
	if err := database.BackupTo(context.Background(), filepath.Join(destination, "peerly.db")); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "blobs"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	copied := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		source := filepath.Join(dataDir, "blobs", entry.Name())
		if err := copyFile(source, filepath.Join(destination, "blobs", entry.Name())); err != nil {
			return err
		}
		copied++
	}
	log.Printf("backup written to %s (%d save files)", destination, copied)
	return nil
}

func copyFile(source string, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}
