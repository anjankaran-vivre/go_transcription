package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ─── Config ───────────────────────────────────────────────────────────────────

// ZohoConfig holds all Zoho OAuth settings.
// Values are loaded from environment variables in config.go.
type ZohoConfig struct {
	ClientID       string
	ClientSecret   string
	RedirectURI    string
	AccountsURL    string // e.g. "https://accounts.zoho.in"
	CreatorURL     string // e.g. "https://creator.zoho.in"
	OAuthScopes    []string
	TokensFilePath string // absolute path where tokens.json is saved
}

// Validate checks that all required fields are set.
func (c *ZohoConfig) Validate() error {
	if c.ClientID == "" {
		return fmt.Errorf("ZOHO_CLIENT_ID is not set")
	}
	if c.ClientSecret == "" {
		return fmt.Errorf("ZOHO_CLIENT_SECRET is not set")
	}
	if c.RedirectURI == "" {
		return fmt.Errorf("ZOHO_REDIRECT_URI is not set")
	}
	return nil
}

// GetConfigStatus returns a safe (no secrets) summary for API responses.
func (c *ZohoConfig) GetConfigStatus() map[string]interface{} {
	return map[string]interface{}{
		"client_id_set":     c.ClientID != "",
		"client_secret_set": c.ClientSecret != "",
		"redirect_uri":      c.RedirectURI,
		"accounts_url":      c.AccountsURL,
		"creator_url":       c.CreatorURL,
		"scopes":            c.OAuthScopes,
		"tokens_file":       c.TokensFilePath,
	}
}

// ─── Stored Token ─────────────────────────────────────────────────────────────

// storedTokens is the structure saved to / loaded from the tokens JSON file.
type storedTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ─── Token Manager ────────────────────────────────────────────────────────────

// TokenManager manages Zoho OAuth access and refresh tokens.
// It is safe for concurrent use across goroutines.
type TokenManager struct {
	mu           sync.RWMutex
	cfg          *ZohoConfig
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	tokensFile   string // absolute path to tokens.json
}

// NewTokenManager creates a TokenManager, loads any existing tokens from disk,
// and returns it ready to use.
//
// tokensDir is the directory where tokens.json will be saved.
// Typically pass the project root or a dedicated secrets/ folder.
func NewTokenManager(cfg *ZohoConfig, tokensDir string) *TokenManager {
	// Ensure the directory exists
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		log.Printf("[ZohoAuth] WARNING: cannot create tokens dir %s: %v", tokensDir, err)
	}

	tm := &TokenManager{
		cfg:        cfg,
		tokensFile: filepath.Join(tokensDir, "zoho_tokens.json"),
	}

	// Load tokens saved from a previous run
	if err := tm.loadTokens(); err != nil {
		log.Printf("[ZohoAuth] No existing tokens loaded: %v", err)
	} else {
		log.Printf("[ZohoAuth] Tokens loaded from %s", tm.tokensFile)
	}

	return tm
}

// ─── Public API ───────────────────────────────────────────────────────────────

// GetToken returns a valid access token, refreshing automatically if needed.
func (tm *TokenManager) GetToken(ctx context.Context) (string, error) {
	tm.mu.RLock()
	token := tm.accessToken
	expiry := tm.expiresAt
	tm.mu.RUnlock()

	// Refresh if expired (with 60 s buffer)
	if token == "" || time.Now().Add(60*time.Second).After(expiry) {
		log.Println("[ZohoAuth] Token expired or missing — refreshing...")
		if err := tm.Refresh(ctx); err != nil {
			return "", fmt.Errorf("token refresh failed: %w", err)
		}

		tm.mu.RLock()
		token = tm.accessToken
		tm.mu.RUnlock()
	}

	return token, nil
}

// SetTokensFromOAuth stores tokens received from the OAuth callback flow.
// Call this from the /zoho/auth/generate-tokens handler.
func (tm *TokenManager) SetTokensFromOAuth(accessToken, refreshToken string, expiresIn int) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.accessToken = accessToken
	tm.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)

	if refreshToken != "" {
		tm.refreshToken = refreshToken
		log.Println("[ZohoAuth] Refresh token updated from OAuth flow")
	}

	return tm.saveTokens()
}

// Refresh forces a token refresh using the stored refresh token.
func (tm *TokenManager) Refresh(ctx context.Context) error {
	tm.mu.RLock()
	refreshToken := tm.refreshToken
	tm.mu.RUnlock()

	if refreshToken == "" {
		return fmt.Errorf(
			"no refresh token available — visit /zoho/auth/url to authorise",
		)
	}

	tokenURL := tm.cfg.AccountsURL + "/oauth/v2/token"

	formData := url.Values{}
	formData.Set("refresh_token", refreshToken)
	formData.Set("client_id", tm.cfg.ClientID)
	formData.Set("client_secret", tm.cfg.ClientSecret)
	formData.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tokenURL,
		strings.NewReader(formData.Encode()),
	)
	if err != nil {
		return fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("refresh HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse refresh response: %w", err)
	}

	if errMsg, ok := result["error"].(string); ok {
		return fmt.Errorf("zoho refresh error: %s", errMsg)
	}

	newToken, ok := result["access_token"].(string)
	if !ok || newToken == "" {
		return fmt.Errorf("no access_token in refresh response: %s", string(body))
	}

	expiresIn := 3600
	if v, ok := result["expires_in"].(float64); ok {
		expiresIn = int(v)
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.accessToken = newToken
	tm.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)

	// Zoho sometimes returns a new refresh token on refresh
	if newRefresh, ok := result["refresh_token"].(string); ok && newRefresh != "" {
		tm.refreshToken = newRefresh
		log.Println("[ZohoAuth] New refresh token received")
	}

	log.Printf("[ZohoAuth] Token refreshed — expires at %s", tm.expiresAt.Format(time.RFC3339))

	return tm.saveTokens()
}

// Status returns a safe summary of the current token state.
func (tm *TokenManager) Status() map[string]interface{} {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	hasAccess := tm.accessToken != ""
	hasRefresh := tm.refreshToken != ""
	expired := time.Now().After(tm.expiresAt)

	return map[string]interface{}{
		"has_access_token":  hasAccess,
		"has_refresh_token": hasRefresh,
		"expired":           expired,
		"expires_at":        tm.expiresAt.Format(time.RFC3339),
		"tokens_file":       tm.tokensFile,
	}
}

// TokensFilePath returns the absolute path to the tokens file.
func (tm *TokenManager) TokensFilePath() string {
	return tm.tokensFile
}

// TokensFileExists returns true if the tokens file is present on disk.
func (tm *TokenManager) TokensFileExists() bool {
	_, err := os.Stat(tm.tokensFile)
	return err == nil
}

// ─── Persistence ──────────────────────────────────────────────────────────────

// saveTokens writes the current tokens to disk.
// Caller must hold tm.mu (write lock) or be in a locked context.
func (tm *TokenManager) saveTokens() error {
	data := storedTokens{
		AccessToken:  tm.accessToken,
		RefreshToken: tm.refreshToken,
		ExpiresAt:    tm.expiresAt,
	}

	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}

	// Write atomically: write to .tmp then rename
	tmpFile := tm.tokensFile + ".tmp"
	if err := os.WriteFile(tmpFile, b, 0600); err != nil {
		return fmt.Errorf("write tokens tmp file: %w", err)
	}
	if err := os.Rename(tmpFile, tm.tokensFile); err != nil {
		return fmt.Errorf("rename tokens file: %w", err)
	}

	log.Printf("[ZohoAuth] Tokens saved to %s", tm.tokensFile)
	return nil
}

// loadTokens reads tokens from disk into the manager.
func (tm *TokenManager) loadTokens() error {
	b, err := os.ReadFile(tm.tokensFile)
	if err != nil {
		return fmt.Errorf("read tokens file: %w", err)
	}

	var data storedTokens
	if err := json.Unmarshal(b, &data); err != nil {
		return fmt.Errorf("parse tokens file: %w", err)
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.accessToken = data.AccessToken
	tm.refreshToken = data.RefreshToken
	tm.expiresAt = data.ExpiresAt

	return nil
}

// ─── Auth URL Builder ─────────────────────────────────────────────────────────

// BuildAuthURL constructs the Zoho OAuth authorization URL.
func (tm *TokenManager) BuildAuthURL() string {
	scopes := strings.Join(tm.cfg.OAuthScopes, ",")

	params := url.Values{}
	params.Set("scope", scopes)
	params.Set("client_id", tm.cfg.ClientID)
	params.Set("response_type", "code")
	params.Set("access_type", "offline")
	params.Set("redirect_uri", tm.cfg.RedirectURI)

	return tm.cfg.AccountsURL + "/oauth/v2/auth?" + params.Encode()
}

// ExchangeCode exchanges an authorization code for access + refresh tokens.
// Call this from the OAuth callback handler.
func (tm *TokenManager) ExchangeCode(ctx context.Context, code string) (map[string]interface{}, error) {
	tokenURL := tm.cfg.AccountsURL + "/oauth/v2/token"

	formData := url.Values{}
	formData.Set("code", code)
	formData.Set("client_id", tm.cfg.ClientID)
	formData.Set("client_secret", tm.cfg.ClientSecret)
	formData.Set("redirect_uri", tm.cfg.RedirectURI)
	formData.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tokenURL,
		strings.NewReader(formData.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("build exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse exchange response: %w", err)
	}

	if errMsg, ok := result["error"].(string); ok {
		return nil, fmt.Errorf("zoho exchange error: %s", errMsg)
	}

	// Persist the new tokens
	accessToken, _ := result["access_token"].(string)
	refreshToken, _ := result["refresh_token"].(string)
	expiresIn := 3600
	if v, ok := result["expires_in"].(float64); ok {
		expiresIn = int(v)
	}

	if err := tm.SetTokensFromOAuth(accessToken, refreshToken, expiresIn); err != nil {
		log.Printf("[ZohoAuth] WARNING: failed to save tokens: %v", err)
	}

	return result, nil
}