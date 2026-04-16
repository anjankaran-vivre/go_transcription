package routes

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"go_transcription/models"
	"go_transcription/services"
)

// ─── Handler ──────────────────────────────────────────────────────────────────

// MeetingHandler holds dependencies for the meeting recording route.
type MeetingHandler struct {
	meetingService *services.MeetingService
}

// NewMeetingHandler creates a new MeetingHandler.
func NewMeetingHandler(meetingService *services.MeetingService) *MeetingHandler {
	return &MeetingHandler{meetingService: meetingService}
}

// ─── Routes ───────────────────────────────────────────────────────────────────

// RegisterMeetingRoutes registers all meeting recording routes onto the mux.
func RegisterMeetingRoutes(mux *http.ServeMux, h *MeetingHandler) {
	mux.HandleFunc("/meeting", h.processMeeting)
}

// ─── POST /meeting ────────────────────────────────────────────────────────────

// processMeeting handles POST /meeting.
//
// Accepts JSON payload:
//
//	{
//	  "fileId":       "abc123",
//	  "downloadUrl":  "https://...",
//	  "permalink":    "https://...",
//	  "createdTime":  "Mar 17, 4:02 PM",
//	  "meetingTitle": "Weekly Sync"
//	}
//
// Returns 202 immediately and processes in background.
//
// TO ENABLE ZOHO POST-BACK: populate these optional fields in the payload:
//
//	"zohoRecordId"   : "ABC123"
//	"zohoOwnerName"  : "vivrepanelsprivatelimited"
//	"zohoAppName"    : "md-approval-application"
//	"zohoReportName" : "AI_Meeting_Transcription"
//
// Then uncomment the Zoho block in services/meeting_service.go → ProcessMeeting()
func (h *MeetingHandler) processMeeting(w http.ResponseWriter, r *http.Request) {
	// ── Method check ──
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}

	// ── Parse JSON body ──
	var payload models.MeetingRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload", err.Error())
		return
	}

	// ── Validate required fields ──
	if strings.TrimSpace(payload.FileID) == "" {
		writeError(w, http.StatusBadRequest, "fileId is required", "")
		return
	}
	if strings.TrimSpace(payload.DownloadURL) == "" {
		writeError(w, http.StatusBadRequest, "downloadUrl is required", "")
		return
	}

	log.Printf("[MeetingRoute] POST /meeting — fileId=%s createdTime=%s",
		payload.FileID, payload.CreatedTime)
	log.Printf("[MeetingRoute]   • Permalink   : %s", payload.Permalink)
	log.Printf("[MeetingRoute]   • Title       : %s", payload.MeetingTitle)
	log.Printf("[MeetingRoute]   • DownloadURL : %s", truncate(payload.DownloadURL, 80))

	// ── Resolve Zoho write-back fields ──
	// These are passed through to ProcessMeeting but not used
	// until you enable the Zoho block in meeting_service.go
	zohoRecordID := payload.ZohoRecordID
	if zohoRecordID == "" {
		zohoRecordID = payload.FileID // fallback to fileId
	}
	zohoOwnerName := payload.ZohoOwnerName
	zohoAppName := payload.ZohoAppName
	zohoReportName := payload.ZohoReportName

	// ── Return 202 immediately ──
	writeJSON(w, http.StatusAccepted, models.MeetingResponse{
		Success:   true,
		FileID:    payload.FileID,
		Message:   "Meeting recording received — processing in background",
		Permalink: payload.Permalink,
	})

	// ── Launch background processing ──
	go func() {
		// Use a fresh context with a long timeout for background work.
		// Adjust the timeout to suit your longest expected processing time.
		bgCtx, cancel := context.WithTimeout(
			context.Background(),
			60*time.Minute,
		)
		defer cancel()

		log.Printf("[MeetingRoute] Background processing started for fileId=%s", payload.FileID)

		result, errMsg := h.meetingService.ProcessMeeting(
			bgCtx,
			payload.FileID,
			payload.DownloadURL,
			payload.Permalink,
			payload.CreatedTime,
			payload.MeetingTitle,
			zohoOwnerName,
			zohoAppName,
			zohoReportName,
			zohoRecordID,
		)

		if errMsg != "" {
			log.Printf("[MeetingRoute] ERROR: Background processing failed for fileId=%s: %s",
				payload.FileID, errMsg)
			return
		}

		log.Printf("[MeetingRoute] Background processing completed for fileId=%s", payload.FileID)
		log.Printf("[MeetingRoute]   • Database ID   : %d", result.DatabaseID)
		log.Printf("[MeetingRoute]   • File size     : %d bytes", result.FileSizeBytes)
		log.Printf("[MeetingRoute]   • Audio size    : %d bytes", result.AudioSizeBytes)
		log.Printf("[MeetingRoute]   • Total time    : %.2fs", result.ProcessingTime)
		log.Printf("[MeetingRoute]   • Transcription : %d chars",
			len(result.MeetingTranscription))
		log.Printf("[MeetingRoute]   • Summary       : %d chars",
			len(result.MeetingSummary))
	}()
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[Routes] ERROR: failed to write JSON response: %v", err)
	}
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, errMsg, detail string) {
	writeJSON(w, status, models.ErrorResponse{
		Success:   false,
		Error:     errMsg,
		Detail:    detail,
		Timestamp: time.Now().Format(time.RFC3339),
	})
}

// truncate shortens a string to maxLen characters for safe logging.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}