// Command worker walks running sheets through their checklist. It shares
// the database and the DATA_DIR volume with the API.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"listingboard/internal/store"
	"listingboard/internal/worker"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	pause, err := worker.ParsePause(os.Getenv("STEP_PAUSE"))
	if err != nil {
		log.Fatal(err)
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.OpenWithRetry(ctx, dsn, 30*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	log.Printf("worker started: step pause %s, data dir %s", pause, dataDir)
	w := &worker.Worker{Store: st, DataDir: dataDir, Pause: pause, Poll: 500 * time.Millisecond}
	if err := w.Run(ctx); err != nil {
		log.Fatal(err)
	}
	log.Print("worker stopped")
}
