package database

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	_ "github.com/denisenkom/go-mssqldb"
)

// ─── Config ───────────────────────────────────────────────────────────────────

// DBConfig holds all database connection settings.
// Populate from your config/env loader.
type DBConfig struct {
	DBHost     string
	DBServer   string
	DBName     string
	DBUsername string
	DBPassword string
}

// ─── Manager ──────────────────────────────────────────────────────────────────

// DatabaseManager is a singleton that owns the *sql.DB connection pool.
type DatabaseManager struct {
	db  *sql.DB
	cfg DBConfig
}

var (
	dbInstance *DatabaseManager
	dbOnce     sync.Once
)

// InitDB initialises the singleton database manager.
// Call once in main.go before starting the HTTP server.
func InitDB(cfg DBConfig) error {
	var initErr error


	
	
	dbOnce.Do(func() {
		dsn := fmt.Sprintf(
		"sqlserver://@%s?instance=%s&database=%s&trusted_connection=yes&TrustServerCertificate=true",
			cfg.DBHost,
			cfg.DBServer,
			cfg.DBName,
		)

		db, err := sql.Open("sqlserver", dsn)
		if err != nil {
			initErr = fmt.Errorf("sql.Open failed: %w", err)
			return
		}

		// Connection pool settings (mirrors Python SQLAlchemy config)
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(10)
		db.SetConnMaxLifetime(time.Duration(3600) * time.Second)
		db.SetConnMaxIdleTime(time.Duration(600) * time.Second)

		// Verify connectivity
		if err := db.Ping(); err != nil {
			initErr = fmt.Errorf("db.Ping failed: %w", err)
			return
		}

		dbInstance = &DatabaseManager{db: db, cfg: cfg}
		log.Printf("[Database] Connected to %s on %s", cfg.DBName, cfg.DBServer)

		// Create tables if they don't exist
		if err := dbInstance.initTables(); err != nil {
			initErr = fmt.Errorf("initTables failed: %w", err)
			return
		}
	})

	return initErr
}

// GetDB returns the singleton DatabaseManager.
// Panics if InitDB has not been called.
func GetDB() *DatabaseManager {
	if dbInstance == nil {
		panic("[Database] GetDB called before InitDB")
	}
	return dbInstance
}

// DB returns the raw *sql.DB for use in queries.
func (m *DatabaseManager) DB() *sql.DB {
	return m.db
}

// Close closes the database connection pool.
func (m *DatabaseManager) Close() error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}

// ─── Table Initialisation ─────────────────────────────────────────────────────

// initTables creates all required tables if they do not already exist.
func (m *DatabaseManager) initTables() error {
	if err := m.createMeetingRecordingsTable(); err != nil {
		return fmt.Errorf("create meeting_recordings table: %w", err)
	}
	log.Println("[Database] Tables verified/created")
	return nil
}

// createMeetingRecordingsTable creates the meeting_recordings table.
// Mirrors the Python WorkdriveMeetingRecording SQLAlchemy model.
func (m *DatabaseManager) createMeetingRecordingsTable() error {
	query := `
	IF NOT EXISTS (
		SELECT 1 FROM sysobjects
		WHERE name = 'meeting_recordings' AND xtype = 'U'
	)
	BEGIN
		CREATE TABLE meeting_recordings (
			id                      INT IDENTITY(1,1) PRIMARY KEY,
			file_id                 NVARCHAR(200)  NOT NULL,
			download_url            NVARCHAR(MAX)  NOT NULL,
			permalink               NVARCHAR(MAX)  NULL,
			zoho_created_time       NVARCHAR(100)  NULL,
			meeting_title           NVARCHAR(500)  NULL,
			meeting_transcription   NVARCHAR(MAX)  NULL,
			meeting_summary         NVARCHAR(MAX)  NULL,
			status                  NVARCHAR(50)   NOT NULL DEFAULT 'pending',
			error_message           NVARCHAR(MAX)  NULL,
			processing_time_seconds INT            NULL,
			file_size_bytes         BIGINT         NULL,
			audio_size_bytes        BIGINT         NULL,
			created_at              DATETIME2      DEFAULT GETUTCDATE(),
			updated_at              DATETIME2      DEFAULT GETUTCDATE()
		);

		CREATE UNIQUE INDEX UX_meeting_recordings_file_id
			ON meeting_recordings (file_id);
	END`

	_, err := m.db.Exec(query)
	return err
}

// ─── Repository ───────────────────────────────────────────────────────────────

// MeetingRecordingRepo provides DB operations for the meeting_recordings table.
type MeetingRecordingRepo struct {
	db *sql.DB
}

// NewMeetingRecordingRepo creates a new repository using the shared pool.
func NewMeetingRecordingRepo() *MeetingRecordingRepo {
	return &MeetingRecordingRepo{db: GetDB().DB()}
}

// Upsert inserts or updates a meeting_recordings row.
// Returns the row id.
func (r *MeetingRecordingRepo) Upsert(rec *MeetingRecordingRow) (int64, error) {
	// Check if file_id already exists
	var existingID int64
	checkQ := `SELECT id FROM meeting_recordings WHERE file_id = @p1`
	err := r.db.QueryRow(checkQ, rec.FileID).Scan(&existingID)

	if err == sql.ErrNoRows {
		// ── INSERT ──
		insertQ := `
		INSERT INTO meeting_recordings (
			file_id, download_url, permalink, zoho_created_time,
			meeting_title, meeting_transcription, meeting_summary,
			status, error_message, processing_time_seconds,
			file_size_bytes, audio_size_bytes
		) VALUES (
			@p1, @p2, @p3, @p4,
			@p5, @p6, @p7,
			@p8, @p9, @p10,
			@p11, @p12
		);
		SELECT SCOPE_IDENTITY();`

		var newID int64
		err = r.db.QueryRow(
			insertQ,
			rec.FileID, rec.DownloadURL, rec.Permalink, rec.ZohoCreatedTime,
			rec.MeetingTitle, rec.MeetingTranscription, rec.MeetingSummary,
			rec.Status, rec.ErrorMessage, rec.ProcessingTimeSeconds,
			rec.FileSizeBytes, rec.AudioSizeBytes,
		).Scan(&newID)

		if err != nil {
			return 0, fmt.Errorf("insert meeting_recording: %w", err)
		}

		log.Printf("[Database] Inserted meeting_recording id=%d file_id=%s", newID, rec.FileID)
		return newID, nil
	}

	if err != nil {
		return 0, fmt.Errorf("check existing meeting_recording: %w", err)
	}

	// ── UPDATE ──
	updateQ := `
	UPDATE meeting_recordings SET
		download_url            = @p1,
		permalink               = @p2,
		zoho_created_time       = @p3,
		meeting_title           = @p4,
		meeting_transcription   = @p5,
		meeting_summary         = @p6,
		status                  = @p7,
		error_message           = @p8,
		processing_time_seconds = @p9,
		file_size_bytes         = @p10,
		audio_size_bytes        = @p11,
		updated_at              = GETUTCDATE()
	WHERE id = @p12`

	_, err = r.db.Exec(
		updateQ,
		rec.DownloadURL, rec.Permalink, rec.ZohoCreatedTime,
		rec.MeetingTitle, rec.MeetingTranscription, rec.MeetingSummary,
		rec.Status, rec.ErrorMessage, rec.ProcessingTimeSeconds,
		rec.FileSizeBytes, rec.AudioSizeBytes,
		existingID,
	)

	if err != nil {
		return 0, fmt.Errorf("update meeting_recording: %w", err)
	}

	log.Printf("[Database] Updated meeting_recording id=%d file_id=%s", existingID, rec.FileID)
	return existingID, nil
}

// ─── Row type (internal DB representation) ────────────────────────────────────

// MeetingRecordingRow is the flat struct used for DB read/write.
// Pointer fields are nullable columns.
type MeetingRecordingRow struct {
	ID                    int64
	FileID                string
	DownloadURL           string
	Permalink             string
	ZohoCreatedTime       string
	MeetingTitle          string
	MeetingTranscription  *string // nil → NULL
	MeetingSummary        *string // nil → NULL
	Status                string  // "pending" | "completed" | "failed"
	ErrorMessage          *string // nil → NULL
	ProcessingTimeSeconds int
	FileSizeBytes         int64
	AudioSizeBytes        int64
}