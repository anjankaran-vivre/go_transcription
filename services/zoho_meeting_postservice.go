package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"go_transcription/utils"
)

// ─── Zoho Creator Target Constants ────────────────────────────────────────────
// Update these if the owner / app / form names ever change.

const (
	meetingOwnerName = "vivrepanelsprivatelimited" // Zoho account owner
	meetingAppName   = "md-approval-application"   // Zoho Creator app name
	meetingFormName  = "AI_Meeting_Transcription"  // Form to create records in
)

// ─── Service ──────────────────────────────────────────────────────────────────

// ZohoMeetingPostService creates meeting transcription records in Zoho Creator.
//
// Responsibility chain:
//
//	ZohoMeetingPostService
//	  1. gets a valid OAuth token from TokenManager (zoho_auth.go)
//	  2. builds the form payload
//	  3. POST to Zoho Creator form
//	  4. extracts and returns the new record ID
type ZohoMeetingPostService struct {
	tokenManager   *utils.TokenManager
	creatorBaseURL string // e.g. "https://creator.zoho.in"
	httpClient     *http.Client
}

// NewZohoMeetingPostService constructs the service.
//
//	creatorBaseURL — Zoho Creator base URL, e.g. "https://creator.zoho.in"
//	tokenManager   — manages OAuth tokens (zoho_auth.go)
func NewZohoMeetingPostService(
	creatorBaseURL string,
	tokenManager *utils.TokenManager,
) *ZohoMeetingPostService {
	return &ZohoMeetingPostService{
		tokenManager:   tokenManager,
		creatorBaseURL: strings.TrimRight(creatorBaseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ─── Request type ─────────────────────────────────────────────────────────────

// CreateMeetingRecordRequest carries all meeting data needed to create
// a new record in the AI_Meeting_Transcription form.
//
// Owner / App / Form are handled via the package constants above —
// callers do not need to supply them.
type CreateMeetingRecordRequest struct {
	FileID        string // WorkDrive file ID       → Record_ID
	MeetingTitle  string // Meeting title            → Meeting_Title
	CreatedTime   string // WorkDrive timestamp      → Date_Time (auto-formatted)
	Permalink     string // WorkDrive permalink      → permaUrl
	Transcription string // Full transcription text  → Meeting_Transcription
	Summary       string // AI-generated summary     → Meeting_Summary
}

// zohoURLField is the JSON shape Zoho Creator expects for a URL field.
type zohoURLField struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Value string `json:"value"`
}

// ─── Public API ───────────────────────────────────────────────────────────────

// CreateRecord creates a new meeting transcription record in Zoho Creator.
//
// Returns:
//
//	recordID — Zoho record ID on success, empty string on failure
//	errMsg   — empty on success, description of problem on failure
func (z *ZohoMeetingPostService) CreateRecord(
	ctx context.Context,
	req *CreateMeetingRecordRequest,
) (recordID string, errMsg string) {

	log.Println(strings.Repeat("-", 60))
	log.Println("[ZohoMeetingPostService] Creating meeting record in Zoho Creator")
	log.Printf("[ZohoMeetingPostService]   Owner  : %s", meetingOwnerName)
	log.Printf("[ZohoMeetingPostService]   App    : %s", meetingAppName)
	log.Printf("[ZohoMeetingPostService]   Form   : %s", meetingFormName)
	log.Printf("[ZohoMeetingPostService]   FileID : %s", req.FileID)
	log.Printf("[ZohoMeetingPostService]   Title  : %s", req.MeetingTitle)

	// ── 1. Format date ────────────────────────────────────────────────────────
	// WorkDrive : "Mar 17, 5:32 PM"        (no year)
	// Zoho      : "17-Mar-2025 17:32:00"
	formattedDate := formatMeetingDate(req.CreatedTime)
	log.Printf("[ZohoMeetingPostService]   Date          : %q → %q", req.CreatedTime, formattedDate)
	log.Printf("[ZohoMeetingPostService]   Transcription : %d chars", len(req.Transcription))
	log.Printf("[ZohoMeetingPostService]   Summary       : %d chars", len(req.Summary))
	log.Printf("[ZohoMeetingPostService]   Permalink     : %s", req.Permalink)

	// ── 2. Build payload ──────────────────────────────────────────────────────
	// Zoho Creator expects: {"data": { ...fields... }}
	payload := map[string]interface{}{
		"data": map[string]interface{}{
			"Record_ID":             req.FileID,
			"Meeting_Title":         req.MeetingTitle,
			"Date_Time":             formattedDate,
			"Meeting_Transcription": req.Transcription,
			"Meeting_Summary":       req.Summary,
			"permaUrl": zohoURLField{
				URL:   req.Permalink,
				Title: "Workdrive url",
				Value: "Workdrive url",
			},
		},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		errMsg = fmt.Sprintf("marshal payload: %v", err)
		log.Printf("[ZohoMeetingPostService] ERROR: %s", errMsg)
		return "", errMsg
	}

	// ── 3. Build API URL ──────────────────────────────────────────────────────
	// POST {creatorBaseURL}/api/v2/{owner}/{app}/form/{form}
	apiURL := fmt.Sprintf(
		"%s/api/v2/%s/%s/form/%s",
		z.creatorBaseURL,
		meetingOwnerName,
		meetingAppName,
		meetingFormName,
	)
	log.Printf("[ZohoMeetingPostService]   POST → %s", apiURL)

	// ── 4. Get OAuth token ────────────────────────────────────────────────────
	// TokenManager auto-refreshes if the token is expired.
	// First-time token is obtained via the manual OAuth hit (/zoho/auth/url).
	token, err := z.tokenManager.GetToken(ctx)
	if err != nil {
		errMsg = fmt.Sprintf("get OAuth token: %v", err)
		log.Printf("[ZohoMeetingPostService] ERROR: %s", errMsg)
		return "", errMsg
	}

	// ── 5. Build and execute HTTP request ─────────────────────────────────────
	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, apiURL, bytes.NewReader(bodyBytes),
	)
	if err != nil {
		errMsg = fmt.Sprintf("build HTTP request: %v", err)
		log.Printf("[ZohoMeetingPostService] ERROR: %s", errMsg)
		return "", errMsg
	}
	httpReq.Header.Set("Authorization", "Zoho-oauthtoken "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := z.httpClient.Do(httpReq)
	if err != nil {
		errMsg = fmt.Sprintf("HTTP request failed: %v", err)
		log.Printf("[ZohoMeetingPostService] ERROR: %s", errMsg)
		return "", errMsg
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	log.Printf("[ZohoMeetingPostService]   Response status : %d", resp.StatusCode)
	log.Printf("[ZohoMeetingPostService]   Response body   : %s", string(respBytes))

	// ── 6. Check HTTP status ──────────────────────────────────────────────────
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errMsg = fmt.Sprintf("API error %d: %s", resp.StatusCode, string(respBytes))
		log.Printf("[ZohoMeetingPostService] ERROR: %s", errMsg)
		return "", errMsg
	}

	// ── 7. Extract record ID ──────────────────────────────────────────────────
	var respData map[string]interface{}
	if err := json.Unmarshal(respBytes, &respData); err != nil {
		errMsg = fmt.Sprintf("parse response JSON: %v", err)
		log.Printf("[ZohoMeetingPostService] ERROR: %s", errMsg)
		return "", errMsg
	}

	recordID = extractRecordID(respData)
	if recordID == "" {
		log.Printf("[ZohoMeetingPostService] WARNING: 2xx but no record ID in response body")
		return "", "record created but no ID returned in response"
	}

	log.Println(strings.Repeat("=", 60))
	log.Println("[ZohoMeetingPostService] RECORD CREATED SUCCESSFULLY")
	log.Printf("[ZohoMeetingPostService]   Zoho Record ID : %s", recordID)
	log.Printf("[ZohoMeetingPostService]   File ID        : %s", req.FileID)
	log.Println(strings.Repeat("=", 60))

	return recordID, ""
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// formatMeetingDate converts a WorkDrive date string to Zoho Creator format.
//
//	Input  : "Mar 17, 5:32 PM"        (WorkDrive — no year)
//	Output : "17-Mar-2025 17:32:00"   (Zoho Creator DateTime format)
//
// Falls back to the original string on parse error so the record is still
// submitted rather than dropped.
func formatMeetingDate(dateStr string) string {
	if dateStr == "" {
		return dateStr
	}

	t, err := time.Parse("Jan 2, 3:04 PM", dateStr)
	if err != nil {
		log.Printf("[ZohoMeetingPostService] WARNING: cannot parse date %q: %v — using original", dateStr, err)
		return dateStr
	}

	// WorkDrive omits the year — attach current year
	t = t.AddDate(time.Now().Year()-t.Year(), 0, 0)

	// Zoho Creator DateTime format
	return t.Format("02-Jan-2006 15:04:05")
}

// extractRecordID tries multiple common Zoho response shapes to find the ID.
//
// Zoho Creator can return the record ID under different keys:
//
//	{"data": {"ID": "..."}}           → most common
//	{"data": {"id": "..."}}
//	{"data": {"RECORD_ID": "..."}}
//	{"ID": "..."}                     → root level fallback
//	{"code": 3000, "data": {"ID": "..."}}
func extractRecordID(data map[string]interface{}) string {
	idKeys := []string{"ID", "id", "RECORD_ID", "record_id"}

	// Check inside "data" object first
	if inner, ok := data["data"].(map[string]interface{}); ok {
		for _, key := range idKeys {
			if v, ok := inner[key]; ok && v != nil {
				if s := fmt.Sprintf("%v", v); s != "" && s != "0" {
					return s
				}
			}
		}
	}

	// Fallback: root level
	for _, key := range idKeys {
		if v, ok := data[key]; ok && v != nil {
			if s := fmt.Sprintf("%v", v); s != "" && s != "0" {
				return s
			}
		}
	}

	return ""
}

// ─── Singleton ────────────────────────────────────────────────────────────────

// DefaultZohoMeetingPostService is the package-level singleton.
var DefaultZohoMeetingPostService *ZohoMeetingPostService

// InitZohoMeetingPostService creates the singleton.
//
// Call order in main.go:
//
//	tokenManager := utils.NewTokenManager(cfg, tokensDir)  // 1. zoho_auth
//	InitZohoMeetingPostService(creatorBaseURL, tokenManager)   // 2. this service
func InitZohoMeetingPostService(
	creatorBaseURL string,
	tokenManager *utils.TokenManager,
) {
	DefaultZohoMeetingPostService = NewZohoMeetingPostService(creatorBaseURL, tokenManager)
	log.Printf("[ZohoMeetingPostService] Initialized — Owner=%s | App=%s | Form=%s",
		meetingOwnerName, meetingAppName, meetingFormName)
}