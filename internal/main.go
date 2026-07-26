package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"chatgpt2api/internal/httpapi"
)

func main() {
	app, err := httpapi.NewApp()
	if err != nil {
		log.Fatalf("init app: %v", err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	logger := app.Logger()

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           limitConcurrentRequests(app.Handler(), 256),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		logger.Info("starting server", "addr", ":"+port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("server shutdown failed", "error", err)
	}
	app.Close()
}

func limitConcurrentRequests(next http.Handler, limit int) http.Handler {
	if limit < 1 {
		limit = 1
	}
	semaphore := make(chan struct{}, limit)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "server is busy", http.StatusServiceUnavailable)
		}
	})
}
