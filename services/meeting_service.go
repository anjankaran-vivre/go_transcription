package services

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"go_transcription/database"
	"go_transcription/utils"
)

// ─── Service ──────────────────────────────────────────────────────────────────

// MeetingService orchestrates the full meeting recording pipeline.
type MeetingService struct {
	repo           *database.MeetingRecordingRepo
	audioService   *AudioService
	transcription  *TranscriptionService
	zohoService    *ZohoCreatorService
	tokenManager   *utils.TokenManager
	httpClient     *http.Client
	maxAudioSizeMB int64
	timeoutSeconds int
}

// NewMeetingService creates a new MeetingService with all dependencies.
func NewMeetingService(
	repo *database.MeetingRecordingRepo,
	audioService *AudioService,
	transcription *TranscriptionService,
	zohoService *ZohoCreatorService,
	tokenManager *utils.TokenManager,
	maxAudioSizeMB int64,
	timeoutSeconds int,
) *MeetingService {
	return &MeetingService{
		repo:           repo,
		audioService:   audioService,
		transcription:  transcription,
		zohoService:    zohoService,
		tokenManager:   tokenManager,
		maxAudioSizeMB: maxAudioSizeMB,
		timeoutSeconds: timeoutSeconds,
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
}

// ─── Result ───────────────────────────────────────────────────────────────────

// MeetingResult holds the full output after processing completes.
type MeetingResult struct {
	FileID               string
	MeetingTranscription string
	MeetingSummary       string
	DatabaseID           int64
	FileSizeBytes        int64
	AudioSizeBytes       int64
	ProcessingTime       float64

	// ZohoRecordID is populated only when Zoho post-back is enabled.
	// Currently empty — see TO ENABLE ZOHO comment below.
	ZohoRecordID string
}

// ─── Main Pipeline ────────────────────────────────────────────────────────────

// ProcessMeeting runs the full pipeline:
//  1. Download video/audio from the meeting link
//  2. Extract audio track (MP3) via ffmpeg
//  3. Transcribe + summarise via OpenRouter
//  4. Save result to database
//  5. (Optional) Post transcription to Zoho Creator
//
// Returns (result, errorMessage).
// On failure result is nil and errorMessage describes what went wrong.
func (s *MeetingService) ProcessMeeting(
	ctx context.Context,
	fileID string,
	downloadURL string,
	permalink string,
	createdTime string,
	meetingTitle string,

	// Zoho write-back params — passed through but not used yet.
	// TO ENABLE ZOHO POST: see Step 5 below.
	zohoOwnerName string,
	zohoAppName string,
	zohoReportName string,
	zohoRecordID string,
) (*MeetingResult, string) {

	startTime := time.Now()

	log.Println(strings.Repeat("=", 80))
	log.Println("[MeetingService] PROCESSING STARTED")
	log.Printf("[MeetingService] File ID     : %s", fileID)
	log.Printf("[MeetingService] Permalink   : %s", permalink)
	log.Printf("[MeetingService] Created     : %s", createdTime)
	log.Printf("[MeetingService] Title       : %s", meetingTitle)
	log.Printf("[MeetingService] Download URL: %s", downloadURL)
	log.Println(strings.Repeat("=", 80))

	// ── Step 1: Download video ─────────────────────────────────────────────
	log.Println("[MeetingService] Step 1/4: Downloading meeting file...")

	videoBytes, err := s.downloadFile(ctx, downloadURL)
	if err != nil {
		errMsg := fmt.Sprintf("Download failed: %v", err)
		log.Printf("[MeetingService] ERROR: %s", errMsg)
		s.saveRecord(ctx, &database.MeetingRecordingRow{
			FileID:          fileID,
			DownloadURL:     downloadURL,
			Permalink:       permalink,
			ZohoCreatedTime: createdTime,
			MeetingTitle:    meetingTitle,
			Status:          "failed",
			ErrorMessage:    strPtr(errMsg),
		})
		return nil, errMsg
	}

	fileSize := int64(len(videoBytes))
	log.Printf("[MeetingService] Downloaded %d bytes", fileSize)

	if fileSize == 0 {
		errMsg := "downloaded file is empty"
		log.Printf("[MeetingService] ERROR: %s", errMsg)
		return nil, errMsg
	}

	maxBytes := s.maxAudioSizeMB * 1024 * 1024
	if fileSize > maxBytes {
		errMsg := fmt.Sprintf("file too large: %d bytes (max %d MB)", fileSize, s.maxAudioSizeMB)
		log.Printf("[MeetingService] ERROR: %s", errMsg)
		return nil, errMsg
	}

	// ── Step 2: Extract audio ──────────────────────────────────────────────
	log.Println("[MeetingService] Step 2/4: Extracting audio from file...")

	audioResult, err := s.audioService.ExtractAudioFromVideo(videoBytes)
	if err != nil {
		errMsg := fmt.Sprintf("Audio extraction failed: %v", err)
		log.Printf("[MeetingService] ERROR: %s", errMsg)
		s.saveRecord(ctx, &database.MeetingRecordingRow{
			FileID:          fileID,
			DownloadURL:     downloadURL,
			Permalink:       permalink,
			ZohoCreatedTime: createdTime,
			MeetingTitle:    meetingTitle,
			Status:          "failed",
			ErrorMessage:    strPtr(errMsg),
			FileSizeBytes:   fileSize,
		})
		return nil, errMsg
	}

	audioSize := int64(len(audioResult.Data))
	log.Printf("[MeetingService] Audio extracted: %d bytes (%s)", audioSize, audioResult.Format)

	// ── Step 3: Transcribe ─────────────────────────────────────────────────
	log.Println("[MeetingService] Step 3/4: Transcribing audio...")

	transcribeResult, transcribeErr, transTime, _ := s.transcription.TranscribeAudio(
		ctx,
		audioResult.Data,
		fileID,
		audioResult.Format,
	)

	processingTime := time.Since(startTime).Seconds()

	if transcribeErr != nil {
		errMsg := fmt.Sprintf("Transcription failed: %v", transcribeErr)
		log.Printf("[MeetingService] ERROR: %s", errMsg)
		s.saveRecord(ctx, &database.MeetingRecordingRow{
			FileID:                fileID,
			DownloadURL:           downloadURL,
			Permalink:             permalink,
			ZohoCreatedTime:       createdTime,
			MeetingTitle:          meetingTitle,
			Status:                "failed",
			ErrorMessage:          strPtr(errMsg),
			ProcessingTimeSeconds: int(processingTime),
			FileSizeBytes:         fileSize,
			AudioSizeBytes:        audioSize,
		})
		return nil, errMsg
	}

	fullTranscription := transcribeResult.FullConversation
	summary := transcribeResult.Summary

	log.Printf("[MeetingService] Transcription done in %.2fs", transTime)
	log.Printf("[MeetingService]   • Transcription : %d chars", len(fullTranscription))
	log.Printf("[MeetingService]   • Summary       : %d chars", len(summary))

	// ── Step 4: Save to database ───────────────────────────────────────────
	log.Println("[MeetingService] Step 4/4: Saving to database...")

	dbID := s.saveRecord(ctx, &database.MeetingRecordingRow{
		FileID:                fileID,
		DownloadURL:           downloadURL,
		Permalink:             permalink,
		ZohoCreatedTime:       createdTime,
		MeetingTitle:          meetingTitle,
		MeetingTranscription:  strPtr(fullTranscription),
		MeetingSummary:        strPtr(summary),
		Status:                "completed",
		ProcessingTimeSeconds: int(processingTime),
		FileSizeBytes:         fileSize,
		AudioSizeBytes:        audioSize,
	})

	log.Printf("[MeetingService] Saved to database: id=%d", dbID)

	// ── Step 5: Post to Zoho Creator ───────────────────────────────────────
	//
	// TO ENABLE ZOHO POST-BACK:
	// 1. Uncomment the block below
	// 2. Make sure your MeetingRequest includes:
	//      zohoOwnerName, zohoAppName, zohoReportName, zohoRecordID
	// 3. The transcription will be posted to the Zoho Creator record
	//    using UpdateRecordField (PATCH) for each field.
	//
	// ─────────────────────────────────────────────────────────────────────
	// zohoRecordIDOut := ""
	//
	// if zohoOwnerName != "" && zohoAppName != "" && zohoReportName != "" && zohoRecordID != "" {
	// 	log.Println("[MeetingService] Posting transcription to Zoho Creator...")
	//
	// 	ok, errMsg := s.zohoService.UpdateRecordField(
	// 		ctx,
	// 		zohoOwnerName,
	// 		zohoAppName,
	// 		zohoReportName,
	// 		zohoRecordID,
	// 		"Meeting_Transcription",
	// 		fullTranscription,
	// 	)
	// 	if !ok {
	// 		log.Printf("[MeetingService] WARNING: Failed to update Meeting_Transcription: %s", errMsg)
	// 	}
	//
	// 	ok, errMsg = s.zohoService.UpdateRecordField(
	// 		ctx,
	// 		zohoOwnerName,
	// 		zohoAppName,
	// 		zohoReportName,
	// 		zohoRecordID,
	// 		"Meeting_Summary",
	// 		summary,
	// 	)
	// 	if !ok {
	// 		log.Printf("[MeetingService] WARNING: Failed to update Meeting_Summary: %s", errMsg)
	// 	}
	//
	// 	if ok {
	// 		zohoRecordIDOut = zohoRecordID
	// 		log.Printf("[MeetingService] Zoho record updated: %s", zohoRecordID)
	// 	}
	// }
	// ─────────────────────────────────────────────────────────────────────

	// Final summary
	log.Println(strings.Repeat("=", 80))
	log.Println("[MeetingService] PROCESSING COMPLETED SUCCESSFULLY")
	log.Printf("[MeetingService]   • Database ID   : %d", dbID)
	log.Printf("[MeetingService]   • File size     : %d bytes", fileSize)
	log.Printf("[MeetingService]   • Audio size    : %d bytes", audioSize)
	log.Printf("[MeetingService]   • Total time    : %.2fs", processingTime)
	log.Println(strings.Repeat("=", 80))

	return &MeetingResult{
		FileID:               fileID,
		MeetingTranscription: fullTranscription,
		MeetingSummary:       summary,
		DatabaseID:           dbID,
		FileSizeBytes:        fileSize,
		AudioSizeBytes:       audioSize,
		ProcessingTime:       processingTime,
		ZohoRecordID:         "", // empty until Zoho post-back is enabled
	}, ""
}

// ─── Private Helpers ──────────────────────────────────────────────────────────

// downloadFile downloads a file from a URL.
// It tries with OAuth token first; if the URL is already authenticated
// (pre-signed URL), it works without a token too.
func (s *MeetingService) downloadFile(ctx context.Context, downloadURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	// Attach OAuth token if available
	token, tokenErr := s.tokenManager.GetToken(ctx)
	if tokenErr == nil && token != "" {
		req.Header.Set("Authorization", "Zoho-oauthtoken "+token)
	} else {
		log.Printf("[MeetingService] WARNING: No OAuth token for download: %v", tokenErr)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d from download URL", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	return data, nil
}

// saveRecord persists a MeetingRecordingRow to the database.
// Returns the database ID (0 on failure — logged but not fatal).
func (s *MeetingService) saveRecord(
	ctx context.Context,
	rec *database.MeetingRecordingRow,
) int64 {
	id, err := s.repo.Upsert(rec)
	if err != nil {
		log.Printf("[MeetingService] ERROR: DB save failed: %v", err)
		return 0
	}
	return id
}

// strPtr returns a pointer to the string s.
// Used for nullable database fields.
func strPtr(s string) *string {
	return &s
}

// ─── Singleton ────────────────────────────────────────────────────────────────

// DefaultMeetingService is the package-level singleton.
var DefaultMeetingService *MeetingService

// InitMeetingService creates the singleton.
// Call once in main.go after all dependencies are ready.
func InitMeetingService(
	repo *database.MeetingRecordingRepo,
	audioService *AudioService,
	transcription *TranscriptionService,
	zohoService *ZohoCreatorService,
	tokenManager *utils.TokenManager,
	maxAudioSizeMB int64,
	timeoutSeconds int,
) {
	DefaultMeetingService = NewMeetingService(
		repo,
		audioService,
		transcription,
		zohoService,
		tokenManager,
		maxAudioSizeMB,
		timeoutSeconds,
	)
}