package main

import (
	"context"
	"flag"
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
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o750); err != nil {
		log.Fatal(err)
	}
	database, err := store.Open(filepath.Join(*dataDir, "peerly.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()
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
