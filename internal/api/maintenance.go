package api

import (
	"context"
	"log"
	"time"

	"doriyamedia/internal/db"
)

const maintenanceInterval = 10 * time.Minute

// StartMaintenance launches the process-wide retention worker. The server has
// no graceful-shutdown context today, so the worker lives for the process.
func StartMaintenance(store *db.Store) {
	if store == nil {
		return
	}
	go runMaintenance(context.Background(), store, maintenanceInterval)
}

func runMaintenance(ctx context.Context, store *db.Store, interval time.Duration) {
	if store == nil {
		return
	}
	if interval <= 0 {
		interval = maintenanceInterval
	}
	runMaintenancePass(store)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runMaintenancePass(store)
		}
	}
}

func runMaintenancePass(store *db.Store) {
	now := time.Now().UTC()
	if _, err := store.CleanupPlayback(now); err != nil {
		// DB errors may contain a filesystem path; never log the raw error.
		log.Printf("periodic playback cleanup failed")
	}
	if err := store.CleanupDeviceAuth(now); err != nil {
		log.Printf("periodic device-auth cleanup failed")
	}
}
