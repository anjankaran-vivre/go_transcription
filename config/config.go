package config

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"
)

// ─── Settings ─────────────────────────────────────────────────────────────────

// Settings holds all application configuration loaded from environment variables.
type Settings struct {
	// ── Server ──
	Host string
	Port int

	// ── Database (MSSQL) ──
	DBHost     string
	DBServer   string
	DBName     string
	DBUsername string
	DBPassword string

	// ── OpenRouter / Transcription ──
	OpenRouterAPIKey string
	OpenRouterModel  string
	WaitingPeriod    float64 // seconds

	// ── Audio ──
	TimeoutSeconds int
	MaxAudioSizeMB int64

	// ── Zoho OAuth ──
	ZohoClientID     string
	ZohoClientSecret string
	ZohoRedirectURI  string
	ZohoAccountsURL  string
	ZohoCreatorURL   string
	ZohoOAuthScopes  []string

	// ── Paths ──
	// TokensDir is the directory where zoho_tokens.json is stored.
	// Defaults to <project_root>/secrets/
	TokensDir string

	// LogsDir is where log files are written.
	LogsDir string

	// PIDFile path
	PIDFile string
}

// ─── Singleton ────────────────────────────────────────────────────────────────

var (
	instance *Settings
	once     sync.Once
)

// GetSettings returns the singleton Settings instance.
// It loads the .env file on first call.
func GetSettings() *Settings {
	once.Do(func() {
		instance = load()
	})
	return instance
}

// ─── Loader ───────────────────────────────────────────────────────────────────

// load reads environment variables (after loading .env) and returns Settings.
func load() *Settings {

	projectRoot, err := os.Getwd()
	if err != nil {
		log.Printf("[Config] WARNING: cannot get working directory: %v", err)
		projectRoot = "."
	}

	// Load .env from project root — ignore error if file doesn't exist
	envPath := filepath.Join(projectRoot, ".env")
	if err := godotenv.Load(envPath); err != nil {
		log.Printf("[Config] .env not found at %s — using system environment", envPath)
	} else {
		log.Printf("[Config] Loaded .env from %s", envPath)
	}

	s := &Settings{
		// ── Server ──
		Host: getEnv("HOST", "127.0.0.1"),
		Port: getEnvInt("PORT", 5050),

		// ── Database ──
		DBHost:     getEnv("DB_HOST", "localhost"),
		DBServer:   getEnv("DB_SERVER", ""),
		DBName:     getEnv("DB_NAME", "transcription_db"),
		DBUsername: getEnv("DB_USERNAME", ""),
		DBPassword: getEnv("DB_PASSWORD", ""),

		// ── OpenRouter ──
		OpenRouterAPIKey: getEnv("OPENROUTER_API_KEY", ""),
		OpenRouterModel:  getEnv("OPENROUTER_MODEL", "google/gemini-2.5-flash-preview"),
		WaitingPeriod:    getEnvFloat("WAITING_PERIOD", 25.0),

		// ── Audio ──
		TimeoutSeconds: getEnvInt("TIMEOUT_SECONDS", 300),
		MaxAudioSizeMB: getEnvInt64("MAX_AUDIO_SIZE_MB", 500),

		// ── Zoho OAuth ──
		ZohoClientID:     getEnv("ZOHO_CLIENT_ID", ""),
		ZohoClientSecret: getEnv("ZOHO_CLIENT_SECRET", ""),
		ZohoRedirectURI:  getEnv("ZOHO_REDIRECT_URI", ""),
		ZohoAccountsURL:  getEnv("ZOHO_ACCOUNTS_URL", "https://accounts.zoho.in"),
		ZohoCreatorURL:   getEnv("ZOHO_CREATOR_URL", "https://creator.zoho.in"),
		ZohoOAuthScopes:  getEnvSlice("ZOHO_OAUTH_SCOPES", []string{
			"ZohoCreator.report.UPDATE",
			"ZohoCreator.report.READ",
			"ZohoCreator.form.CREATE",
			"ZohoWorkDrive.files.READ",
		}),

		// ── Paths ──
		TokensDir: getEnv(
			"TOKENS_DIR",
			filepath.Join(projectRoot, "secrets"),
		),
		LogsDir: getEnv(
			"LOGS_DIR",
			filepath.Join(projectRoot, "logs"),
		),
		PIDFile: getEnv(
			"PID_FILE",
			filepath.Join(projectRoot, "server.pid"),
		),
	}

	// Ensure required directories exist
	ensureDir(s.TokensDir)
	ensureDir(s.LogsDir)

	logSettings(s)

	return s
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("[Config] WARNING: %s=%q is not a valid int, using default %d", key, v, defaultVal)
		return defaultVal
	}
	return i
}

func getEnvInt64(key string, defaultVal int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Printf("[Config] WARNING: %s=%q is not a valid int64, using default %d", key, v, defaultVal)
		return defaultVal
	}
	return i
}

func getEnvFloat(key string, defaultVal float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("[Config] WARNING: %s=%q is not a valid float, using default %f", key, v, defaultVal)
		return defaultVal
	}
	return f
}

// getEnvSlice reads a comma-separated env var into a string slice.
func getEnvSlice(key string, defaultVal []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	parts := strings.Split(v, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func ensureDir(path string) {
	if err := os.MkdirAll(path, 0755); err != nil {
		log.Printf("[Config] WARNING: could not create directory %s: %v", path, err)
	}
}

func logSettings(s *Settings) {
	log.Println("[Config] ─────────────────────────────────────")
	log.Printf("[Config] Host           : %s:%d", s.Host, s.Port)
	log.Printf("[Config] Database       : %s on %s", s.DBName, s.DBServer)
	log.Printf("[Config] OpenRouter     : model=%s", s.OpenRouterModel)
	log.Printf("[Config] WaitingPeriod  : %.1fs", s.WaitingPeriod)
	log.Printf("[Config] MaxAudioSize   : %d MB", s.MaxAudioSizeMB)
	log.Printf("[Config] TokensDir      : %s", s.TokensDir)
	log.Printf("[Config] LogsDir        : %s", s.LogsDir)
	log.Printf("[Config] ZohoAccountsURL: %s", s.ZohoAccountsURL)
	log.Printf("[Config] ZohoCreatorURL : %s", s.ZohoCreatorURL)
	log.Println("[Config] ─────────────────────────────────────")
}