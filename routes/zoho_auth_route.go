package routes

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"go_transcription/models"
	"go_transcription/utils"
)

// ─── Handler ──────────────────────────────────────────────────────────────────

// ZohoAuthHandler holds dependencies for Zoho OAuth routes.
type ZohoAuthHandler struct {
	tokenManager *utils.TokenManager
	zohoConfig   *utils.ZohoConfig
}

// NewZohoAuthHandler creates a new ZohoAuthHandler.
func NewZohoAuthHandler(
	tokenManager *utils.TokenManager,
	zohoConfig *utils.ZohoConfig,
) *ZohoAuthHandler {
	return &ZohoAuthHandler{
		tokenManager: tokenManager,
		zohoConfig:   zohoConfig,
	}
}

// ─── Routes ───────────────────────────────────────────────────────────────────

// RegisterZohoAuthRoutes registers all Zoho OAuth routes onto the mux.
func RegisterZohoAuthRoutes(mux *http.ServeMux, h *ZohoAuthHandler) {
	// GET  /zoho/auth/url              — get the authorization URL
	// GET  /zoho/auth/generate-tokens  — OAuth callback (Zoho redirects here)
	// GET  /zoho/token/status          — check current token status
	// POST /zoho/token/refresh         — manually refresh the token
	mux.HandleFunc("/zoho/auth/url", h.getAuthURL)
	mux.HandleFunc("/zoho/auth/generate-tokens", h.generateTokens)
	mux.HandleFunc("/zoho/token/status", h.tokenStatus)
	mux.HandleFunc("/zoho/token/refresh", h.refreshToken)
}

// ─── GET /zoho/auth/url ───────────────────────────────────────────────────────

// getAuthURL returns the Zoho OAuth authorization URL.
//
// Instructions for first-time setup:
//  1. Call GET /zoho/auth/url
//  2. Open the returned URL in your browser
//  3. Click Allow
//  4. Zoho redirects to /zoho/auth/generate-tokens with ?code=...
//  5. Tokens are saved automatically to secrets/zoho_tokens.json
//  6. Call GET /zoho/token/status to confirm
func (h *ZohoAuthHandler) getAuthURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}

	if err := h.zohoConfig.Validate(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}

	authURL := h.tokenManager.BuildAuthURL()

	log.Printf("[ZohoAuthRoute] Auth URL generated")

	writeJSON(w, http.StatusOK, models.ZohoAuthURLResponse{
		Success:          true,
		AuthorizationURL: authURL,
		Instructions: []string{
			"1. Copy the authorization_url above",
			"2. Open it in your browser",
			"3. Click 'Allow' to authorize with Zoho",
			"4. You will be redirected back automatically",
			"5. Tokens are saved to: " + h.tokenManager.TokensFilePath(),
			"6. Call GET /zoho/token/status to verify",
		},
	})
}

// ─── GET /zoho/auth/generate-tokens ──────────────────────────────────────────

// generateTokens handles the Zoho OAuth callback.
// Zoho redirects the browser here with ?code=... after the user clicks Allow.
//
// This endpoint:
//  1. Exchanges the authorization code for access + refresh tokens
//  2. Saves tokens to secrets/zoho_tokens.json
//  3. Returns a success/failure JSON response
//
// Set ZOHO_REDIRECT_URI in .env to:
//
//	http://your-server:5050/zoho/auth/generate-tokens
func (h *ZohoAuthHandler) generateTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}

	log.Println("[ZohoAuthRoute] OAuth callback received")

	query := r.URL.Query()

	// ── Handle error from Zoho ──
	if errParam := query.Get("error"); errParam != "" {
		log.Printf("[ZohoAuthRoute] OAuth error from Zoho: %s", errParam)
		writeJSON(w, http.StatusBadRequest, models.ZohoTokenResponse{
			Success: false,
			Message: "Authorization denied or failed: " + errParam,
		})
		return
	}

	// ── Validate code ──
	code := query.Get("code")
	if strings.TrimSpace(code) == "" {
		writeError(
			w,
			http.StatusBadRequest,
			"no authorization code provided",
			"Visit /zoho/auth/url first to start the OAuth flow",
		)
		return
	}

	log.Printf("[ZohoAuthRoute] Exchanging authorization code: %s...", truncate(code, 20))

	// ── Exchange code for tokens ──
	result, err := h.tokenManager.ExchangeCode(r.Context(), code)
	if err != nil {
		log.Printf("[ZohoAuthRoute] Token exchange failed: %v", err)
		writeError(
			w,
			http.StatusInternalServerError,
			"token exchange failed",
			err.Error(),
		)
		return
	}

	expiresIn := 3600
	if v, ok := result["expires_in"].(float64); ok {
		expiresIn = int(v)
	}

	log.Printf("[ZohoAuthRoute] OAuth completed successfully")
	log.Printf("[ZohoAuthRoute] Tokens saved to: %s", h.tokenManager.TokensFilePath())

	writeJSON(w, http.StatusOK, models.ZohoTokenResponse{
		Success:    true,
		Message:    "OAuth authorization completed successfully — tokens saved",
		ExpiresIn:  expiresIn,
		TokensFile: h.tokenManager.TokensFilePath(),
	})
}

// ─── GET /zoho/token/status ───────────────────────────────────────────────────

// tokenStatus returns the current token state and configuration summary.
func (h *ZohoAuthHandler) tokenStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}

	writeJSON(w, http.StatusOK, models.ZohoTokenStatusResponse{
		TokenStatus:      h.tokenManager.Status(),
		Config:           h.zohoConfig.GetConfigStatus(),
		TokensFile:       h.tokenManager.TokensFilePath(),
		TokensFileExists: h.tokenManager.TokensFileExists(),
	})
}

// ─── POST /zoho/token/refresh ─────────────────────────────────────────────────

// refreshToken manually triggers an access token refresh using the stored
// refresh token.
func (h *ZohoAuthHandler) refreshToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}

	log.Println("[ZohoAuthRoute] Manual token refresh triggered")

	if err := h.tokenManager.Refresh(r.Context()); err != nil {
		log.Printf("[ZohoAuthRoute] Token refresh failed: %v", err)
		writeError(
			w,
			http.StatusInternalServerError,
			"token refresh failed",
			err.Error(),
		)
		return
	}

	log.Println("[ZohoAuthRoute] Token refreshed successfully")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"message":   "Token refreshed successfully",
		"status":    h.tokenManager.Status(),
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

// ─── Health check ─────────────────────────────────────────────────────────────

// RegisterHealthRoute registers GET /health onto the mux.
func RegisterHealthRoute(mux *http.ServeMux, version, model string) {
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
			return
		}
		writeJSON(w, http.StatusOK, models.HealthResponse{
			Status:    "healthy",
			Version:   version,
			Model:     model,
			Timestamp: time.Now().Format(time.RFC3339),
		})
	})
}

// ─── Token file location note ─────────────────────────────────────────────────
//
// The tokens file is saved to the directory specified by TOKENS_DIR in .env
// Default location: <project_root>/secrets/zoho_tokens.json
//
// This file contains:
//   {
//     "access_token":  "...",
//     "refresh_token": "...",
//     "expires_at":    "2025-01-01T00:00:00Z"
//   }
//
// The file is created automatically when you complete the OAuth flow.
// Keep this file secure — treat it like a password.
// Add secrets/ to your .gitignore.
//
// ─────────────────────────────────────────────────────────────────────────────

// parseJSONBody decodes a JSON request body into v.
// Returns false and writes an error response if decoding fails.
func parseJSONBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", err.Error())
		return false
	}
	return true
}