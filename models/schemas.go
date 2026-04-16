package models

import "time"

// ─── Transcription ────────────────────────────────────────────────────────────

type TranscriptionRequest struct {
	AudioURL    string `json:"audio_url" form:"audio_url"`
	AccessToken string `json:"access_token" form:"access_token"`
	RecordID    string `json:"record_id" form:"record_id"`
}

type TranscriptionResult struct {
	FullConversation string `json:"full_conversation"`
	Summary          string `json:"summary"`
}

type TranscriptionResponse struct {
	Success        bool                `json:"success"`
	RecordID       string              `json:"record_id"`
	AudioField     string              `json:"audio_field"`
	Transcription  TranscriptionResult `json:"transcription"`
	AudioFormat    string              `json:"audio_format"`
	AudioSizeBytes int64               `json:"audio_size_bytes"`
	ProcessingTime float64             `json:"processing_time"`
	Cached         bool                `json:"cached"`
	PostToZoho     bool                `json:"post_to_zoho"`
}

// ─── Meeting Recording ────────────────────────────────────────────────────────

// MeetingRequest is the JSON payload sent to trigger meeting recording processing.
// Send this from Zoho Deluge or any HTTP client.
type MeetingRequest struct {
	FileID       string `json:"fileId"`        // WorkDrive / meeting file ID
	DownloadURL  string `json:"downloadUrl"`   // Direct download URL for the MP4
	Permalink    string `json:"permalink"`     // Permanent link to the file
	CreatedTime  string `json:"createdTime"`   // e.g. "Mar 17, 4:02 PM"
	MeetingTitle string `json:"meetingTitle"`  // Optional meeting title

	// ── Zoho Creator write-back (set these when you want to post results to Zoho) ──
	// TO ENABLE ZOHO POST: populate these fields in your request payload.
	// Then uncomment the Zoho call in services/meeting_service.go → ProcessMeeting()
	ZohoRecordID   string `json:"zohoRecordId"`   // Zoho Creator record ID
	ZohoOwnerName  string `json:"zohoOwnerName"`  // e.g. "vivrepanelsprivatelimited"
	ZohoAppName    string `json:"zohoAppName"`    // e.g. "md-approval-application"
	ZohoReportName string `json:"zohoReportName"` // e.g. "AI_Meeting_Transcription"
}

// MeetingResponse is returned immediately (202 Accepted) before background processing.
type MeetingResponse struct {
	Success    bool   `json:"success"`
	FileID     string `json:"file_id"`
	Message    string `json:"message"`
	Permalink  string `json:"permalink"`
	DatabaseID int64  `json:"database_id"`
}

// MeetingProcessResult holds the full output after background processing completes.
type MeetingProcessResult struct {
	FileID               string  `json:"file_id"`
	MeetingTranscription string  `json:"meeting_transcription"`
	MeetingSummary       string  `json:"meeting_summary"`
	DatabaseID           int64   `json:"database_id"`
	ZohoRecordID         string  `json:"zoho_record_id,omitempty"`
	FileSizeBytes        int64   `json:"file_size_bytes"`
	AudioSizeBytes       int64   `json:"audio_size_bytes"`
	ProcessingTime       float64 `json:"processing_time"`
}

// ─── Error ────────────────────────────────────────────────────────────────────

type ErrorResponse struct {
	Success   bool   `json:"success"`
	Error     string `json:"error"`
	Detail    string `json:"detail,omitempty"`
	Timestamp string `json:"timestamp"`
}

// ─── Health ───────────────────────────────────────────────────────────────────

type HealthResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Model     string `json:"model"`
	Timestamp string `json:"timestamp"`
}

// ─── Zoho Auth ────────────────────────────────────────────────────────────────

type ZohoAuthURLResponse struct {
	Success          bool     `json:"success"`
	AuthorizationURL string   `json:"authorization_url"`
	Instructions     []string `json:"instructions"`
}

type ZohoTokenResponse struct {
	Success      bool   `json:"success"`
	Message      string `json:"message"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	TokensFile   string `json:"tokens_file,omitempty"`
}

type ZohoTokenStatusResponse struct {
	TokenStatus      map[string]interface{} `json:"token_status"`
	Config           map[string]interface{} `json:"config"`
	TokensFile       string                 `json:"tokens_file"`
	TokensFileExists bool                   `json:"tokens_file_exists"`
}

// ─── Database Model ───────────────────────────────────────────────────────────

// MeetingRecording is the database row model.
type MeetingRecording struct {
	ID                    int64      `db:"id"`
	FileID                string     `db:"file_id"`
	DownloadURL           string     `db:"download_url"`
	Permalink             string     `db:"permalink"`
	ZohoCreatedTime       string     `db:"zoho_created_time"`
	MeetingTitle          string     `db:"meeting_title"`
	MeetingTranscription  *string    `db:"meeting_transcription"`
	MeetingSummary        *string    `db:"meeting_summary"`
	Status                string     `db:"status"`
	ErrorMessage          *string    `db:"error_message"`
	ProcessingTimeSeconds int        `db:"processing_time_seconds"`
	FileSizeBytes         int64      `db:"file_size_bytes"`
	AudioSizeBytes        int64      `db:"audio_size_bytes"`
	CreatedAt             *time.Time `db:"created_at"`
	UpdatedAt             *time.Time `db:"updated_at"`
}