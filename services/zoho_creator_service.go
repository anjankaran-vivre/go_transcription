package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"go_transcription/utils"
)

// ─── Service ──────────────────────────────────────────────────────────────────

// ZohoCreatorService handles all Zoho Creator API interactions.
type ZohoCreatorService struct {
	apiBaseURL   string
	tokenManager *utils.TokenManager
	httpClient   *http.Client
}

// NewZohoCreatorService creates a new ZohoCreatorService.
func NewZohoCreatorService(apiBaseURL string, tokenManager *utils.TokenManager) *ZohoCreatorService {
	log.Println("[ZohoCreatorService] Initialized")
	return &ZohoCreatorService{
		apiBaseURL:   apiBaseURL,
		tokenManager: tokenManager,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ─── Create Form Record ───────────────────────────────────────────────────────

// CreateFormRecordResult holds the result of a form record creation.
type CreateFormRecordResult struct {
	Success  bool
	RecordID string
	Error    string
}

// CreateFormRecord creates a new record in a Zoho Creator form.
//
// Args:
//
//	appName   : Zoho Creator app name (e.g. "creator-transcription")
//	formName  : Form name (e.g. "AI_Meeting_Transcription")
//	ownerName : Zoho account owner (e.g. "vivrepanelsprivatelimited")
//	fields    : map of field names → values
//
// Returns (success, recordID, errorMessage)
func (s *ZohoCreatorService) CreateFormRecord(
	ctx context.Context,
	ownerName string,
	appName string,
	formName string,
	fields map[string]interface{},
) (bool, string, string) {

	accessToken, err := s.tokenManager.GetToken(ctx)
	if err != nil {
		errMsg := fmt.Sprintf("failed to get access token: %v", err)
		log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
		return false, "", errMsg
	}

	apiURL := fmt.Sprintf(
		"%s/api/v2/%s/%s/form/%s",
		s.apiBaseURL, ownerName, appName, formName,
	)

	log.Printf("[ZohoCreatorService] Creating record in form '%s'", formName)
	log.Printf("[ZohoCreatorService] Fields: %v", mapKeys(fields))

	payload := map[string]interface{}{"data": fields}

	body, err := json.Marshal(payload)
	if err != nil {
		errMsg := fmt.Sprintf("marshal payload: %v", err)
		log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
		return false, "", errMsg
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		errMsg := fmt.Sprintf("build request: %v", err)
		log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
		return false, "", errMsg
	}

	req.Header.Set("Authorization", "Zoho-oauthtoken "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		errMsg := fmt.Sprintf("HTTP request failed: %v", err)
		log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
		return false, "", errMsg
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	respStr := string(respBytes)

	log.Printf("[ZohoCreatorService] Response status: %d", resp.StatusCode)
	log.Printf("[ZohoCreatorService] Response body: %s", respStr)

	if resp.StatusCode == 200 || resp.StatusCode == 201 {
		var data map[string]interface{}
		if err := json.Unmarshal(respBytes, &data); err != nil {
			errMsg := fmt.Sprintf("parse response JSON: %v", err)
			log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
			return false, "", errMsg
		}

		recordID := extractZohoRecordID(data)
		if recordID != "" {
			log.Printf("[ZohoCreatorService] Record created successfully: %s", recordID)
			return true, recordID, ""
		}

		// Success status but no ID
		log.Printf("[ZohoCreatorService] WARNING: Record possibly created but no ID returned")
		return true, "", "record created but no ID in response"
	}

	// Non-success status
	errMsg := fmt.Sprintf("API error %d: %s", resp.StatusCode, respStr)
	log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
	return false, "", errMsg
}

// ─── Update Record Field ──────────────────────────────────────────────────────

// UpdateRecordField updates a single field on an existing Zoho Creator record.
//
// Args:
//
//	ownerName  : Zoho account owner
//	appName    : Zoho Creator app name
//	reportName : Report/view name (e.g. "All_Meetings")
//	recordID   : The record ID to update
//	fieldName  : Field API name to update
//	fieldValue : New value for the field
//
// Returns (success, errorMessage)
func (s *ZohoCreatorService) UpdateRecordField(
	ctx context.Context,
	ownerName string,
	appName string,
	reportName string,
	recordID string,
	fieldName string,
	fieldValue string,
) (bool, string) {

	accessToken, err := s.tokenManager.GetToken(ctx)
	if err != nil {
		errMsg := fmt.Sprintf("failed to get access token: %v", err)
		log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
		return false, errMsg
	}

	apiURL := fmt.Sprintf(
		"%s/api/v2/%s/%s/report/%s/%s",
		s.apiBaseURL, ownerName, appName, reportName, recordID,
	)

	log.Printf("[ZohoCreatorService] Updating field '%s' in report '%s' for record %s",
		fieldName, reportName, recordID)
	log.Printf("[ZohoCreatorService] Target: owner=%s | app=%s | report=%s",
		ownerName, appName, reportName)

	payload := map[string]interface{}{
		"data": map[string]interface{}{
			fieldName: fieldValue,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		errMsg := fmt.Sprintf("marshal payload: %v", err)
		log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
		return false, errMsg
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, apiURL, bytes.NewReader(body))
	if err != nil {
		errMsg := fmt.Sprintf("build request: %v", err)
		log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
		return false, errMsg
	}

	req.Header.Set("Authorization", "Zoho-oauthtoken "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		errMsg := fmt.Sprintf("HTTP request failed: %v", err)
		log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
		return false, errMsg
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 204 {
		log.Printf("[ZohoCreatorService] Field '%s' updated successfully for record %s",
			fieldName, recordID)
		return true, ""
	}

	errMsg := fmt.Sprintf("API error %d: %s", resp.StatusCode, string(respBytes))
	log.Printf("[ZohoCreatorService] ERROR: %s", errMsg)
	return false, errMsg
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// extractZohoRecordID tries multiple common Zoho response structures
// to find the record ID.
func extractZohoRecordID(data map[string]interface{}) string {
	idKeys := []string{"ID", "id", "RECORD_ID", "record_id"}

	// Structure 1: {"data": {"ID": "..."}}
	if inner, ok := data["data"].(map[string]interface{}); ok {
		for _, key := range idKeys {
			if v, ok := inner[key]; ok && v != nil {
				return fmt.Sprintf("%v", v)
			}
		}
	}

	// Structure 2: {"ID": "..."} at root
	for _, key := range idKeys {
		if v, ok := data[key]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
	}

	// Structure 3: {"code": 3000, "data": {...}}
	if code, ok := data["code"].(float64); ok && int(code) == 3000 {
		if inner, ok := data["data"].(map[string]interface{}); ok {
			for _, key := range idKeys {
				if v, ok := inner[key]; ok && v != nil {
					return fmt.Sprintf("%v", v)
				}
			}
		}
	}

	return ""
}

// mapKeys returns the keys of a map for logging purposes.
func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ─── Singleton ────────────────────────────────────────────────────────────────

// DefaultZohoCreatorService is the package-level singleton.
// Initialise in main.go with InitZohoCreatorService().
var DefaultZohoCreatorService *ZohoCreatorService

// InitZohoCreatorService creates the singleton.
// Call once during application startup.
func InitZohoCreatorService(apiBaseURL string, tokenManager *utils.TokenManager) {
	DefaultZohoCreatorService = NewZohoCreatorService(apiBaseURL, tokenManager)
}