package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go_transcription/config"
	"go_transcription/database"
	"go_transcription/routes"
	"go_transcription/services"
	"go_transcription/utils"
)

// ─── Version ──────────────────────────────────────────────────────────────────

const (
	appVersion = "1.0.0"
)

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	printBanner()

	// ── Load config ──────────────────────────────────────────────────────────
	cfg := config.GetSettings()

	// ── Initialise database ──────────────────────────────────────────────────
	log.Println("[Main] Initialising database...")
	if err := database.InitDB(database.DBConfig{
		DBHost:     cfg.DBHost,
		DBServer:   cfg.DBServer,
		DBName:     cfg.DBName,
		DBUsername: cfg.DBUsername,
		DBPassword: cfg.DBPassword,
	}); err != nil {
		log.Fatalf("[Main] Database init failed: %v", err)
	}
	defer database.GetDB().Close()

	// ── Initialise Zoho config ───────────────────────────────────────────────
	zohoConfig := &utils.ZohoConfig{
		ClientID:       cfg.ZohoClientID,
		ClientSecret:   cfg.ZohoClientSecret,
		RedirectURI:    cfg.ZohoRedirectURI,
		AccountsURL:    cfg.ZohoAccountsURL,
		CreatorURL:     cfg.ZohoCreatorURL,
		OAuthScopes:    cfg.ZohoOAuthScopes,
		TokensFilePath: cfg.TokensDir,
	}

	// ── Initialise token manager ─────────────────────────────────────────────
	// Tokens are saved to / loaded from cfg.TokensDir/zoho_tokens.json
	// First-time token obtained via GET /zoho/auth/url → browser → Allow
	log.Printf("[Main] Token storage directory: %s", cfg.TokensDir)
	tokenManager := utils.NewTokenManager(zohoConfig, cfg.TokensDir)

	// ── Initialise services ──────────────────────────────────────────────────
	log.Println("[Main] Initialising services...")

	// Audio service
	audioService := services.NewAudioService(
		cfg.TimeoutSeconds,
		cfg.MaxAudioSizeMB,
	)

	// Transcription service
	transcriptionService := services.NewTranscriptionService(
		services.TranscriptionConfig{
			OpenRouterAPIKey: cfg.OpenRouterAPIKey,
			OpenRouterModel:  cfg.OpenRouterModel,
			WaitingPeriod:    cfg.WaitingPeriod,
		},
	)

	// Zoho Meeting Post service
	// Handles: get token → build payload → POST to Zoho Creator form
	// Owner / App / Form are constants inside zoho_meeting_post_service.go
	zohoMeetingPostService := services.NewZohoMeetingPostService(
		cfg.ZohoCreatorURL,
		tokenManager,
	)

	// Database repository
	meetingRepo := database.NewMeetingRecordingRepo()

	// Meeting service — wires everything together
	meetingService := services.NewMeetingService(
		meetingRepo,
		audioService,
		transcriptionService,
		zohoMeetingPostService, // ← ZohoMeetingPostService (not ZohoCreatorService)
		tokenManager,
		cfg.MaxAudioSizeMB,
		cfg.TimeoutSeconds,
	)

	// ── Initialise HTTP router ───────────────────────────────────────────────
	log.Println("[Main] Registering routes...")
	mux := http.NewServeMux()

	// Health check — GET /health
	routes.RegisterHealthRoute(mux, appVersion, cfg.OpenRouterModel)

	// Meeting recording — POST /meeting
	meetingHandler := routes.NewMeetingHandler(meetingService)
	routes.RegisterMeetingRoutes(mux, meetingHandler)

	// Zoho OAuth — /zoho/auth/url, /zoho/auth/generate-tokens,
	//              /zoho/token/status, /zoho/token/refresh
	zohoAuthHandler := routes.NewZohoAuthHandler(tokenManager, zohoConfig)
	routes.RegisterZohoAuthRoutes(mux, zohoAuthHandler)

	// ── Start HTTP server ────────────────────────────────────────────────────
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	server := &http.Server{
		Addr:         addr,
		Handler:      withLogging(withRecovery(mux)),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// ── Graceful shutdown ────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	go func() {
		printStartup(cfg, addr)
		log.Printf("[Main] Server listening on http://%s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Main] Server error: %v", err)
		}
	}()

	// Block until shutdown signal received
	<-quit
	log.Println("\n[Main] Shutdown signal received — stopping gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[Main] Server forced to shut down: %v", err)
	}

	log.Println("[Main] Server stopped cleanly.")
}

// ─── Middleware ───────────────────────────────────────────────────────────────

// withLogging logs every incoming request with method, path, and duration.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		log.Printf("[HTTP] %s %s %d %s",
			r.Method,
			r.URL.Path,
			rw.status,
			time.Since(start).Round(time.Millisecond),
		)
	})
}

// withRecovery catches panics and returns a 500 instead of crashing.
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[Main] PANIC recovered: %v", rec)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// ─── Banner ───────────────────────────────────────────────────────────────────

func printBanner() {
	log.Println("=" + strings.Repeat("=", 46))
	log.Println("   TRANSCRIPTION SERVER (Go)")
	log.Println("=" + strings.Repeat("=", 46))
}

func printStartup(cfg *config.Settings, addr string) {
	log.Println(strings.Repeat("=", 60))
	log.Println("  TRANSCRIPTION SERVER READY")
	log.Println(strings.Repeat("=", 60))
	log.Printf("  API       : http://%s", addr)
	log.Printf("  Version   : %s", appVersion)
	log.Printf("  Model     : %s", cfg.OpenRouterModel)
	log.Printf("  DB        : %s", cfg.DBName)
	log.Println("  " + strings.Repeat("─", 57))
	log.Println("  ENDPOINTS:")
	log.Printf("  GET  http://%s/health", addr)
	log.Printf("  POST http://%s/meeting", addr)
	log.Printf("  GET  http://%s/zoho/auth/url", addr)
	log.Printf("  GET  http://%s/zoho/auth/generate-tokens", addr)
	log.Printf("  GET  http://%s/zoho/token/status", addr)
	log.Printf("  POST http://%s/zoho/token/refresh", addr)
	log.Println("  " + strings.Repeat("─", 57))
	// log.Println("  ZOHO SETUP (first time only):")
	// log.Printf("  1. GET  http://%s/zoho/auth/url", addr)
	// log.Println("  2. Open the URL in your browser and click Allow")
	// log.Printf("  3. Tokens saved to: %s/zoho_tokens.json", cfg.TokensDir)
	log.Println(strings.Repeat("=", 60))
}