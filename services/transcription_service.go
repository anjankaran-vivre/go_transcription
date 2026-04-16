package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ─── Constants ────────────────────────────────────────────────────────────────

const (
	chunkDurationMS      = 20 * 60 * 1000 // 20 minutes in milliseconds
	compressedBitrate    = "48k"
	compressedSampleRate = 16000
	openRouterBaseURL    = "https://openrouter.ai/api/v1"
)

// ─── Config ───────────────────────────────────────────────────────────────────

// TranscriptionConfig holds all settings for the transcription service.
type TranscriptionConfig struct {
	OpenRouterAPIKey string
	OpenRouterModel  string
	WaitingPeriod    float64 // seconds
}

// ─── Types ────────────────────────────────────────────────────────────────────

// audioChunk represents a single piece of chunked audio.
type audioChunk struct {
	Data       []byte
	Format     string
	Index      int
	TotalCount int
}

// TranscribeResult holds the transcription output.
type TranscribeResult struct {
	FullConversation string
	Summary          string
}

// ─── OpenRouter Request/Response types ───────────────────────────────────────

type openRouterRequest struct {
	Model       string              `json:"model"`
	Messages    []openRouterMessage `json:"messages"`
	Temperature float64             `json:"temperature"`
	MaxTokens   int                 `json:"max_tokens"`
	TopP        float64             `json:"top_p,omitempty"`
}

type openRouterMessage struct {
	Role    string        `json:"role"`
	Content interface{}   `json:"content"`
}

type contentPart struct {
	Type       string      `json:"type"`
	Text       string      `json:"text,omitempty"`
	InputAudio *inputAudio `json:"input_audio,omitempty"`
}

type inputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type openRouterResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ─── Service ──────────────────────────────────────────────────────────────────

// TranscriptionService handles audio transcription via OpenRouter.
type TranscriptionService struct {
	config     TranscriptionConfig
	httpClient *http.Client
}

// NewTranscriptionService creates a new TranscriptionService.
func NewTranscriptionService(cfg TranscriptionConfig) *TranscriptionService {
	s := &TranscriptionService{
		config: cfg,
		httpClient: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}

	if cfg.OpenRouterAPIKey == "" {
		log.Println("[TranscriptionService] WARNING: OPENROUTER_API_KEY not set")
	} else {
		log.Println("[TranscriptionService] Initialized")
		log.Printf("[TranscriptionService]   Model      : %s", cfg.OpenRouterModel)
		log.Printf("[TranscriptionService]   Via        : OpenRouter API")
		log.Printf("[TranscriptionService]   Chunk size : %.0f minutes",
			float64(chunkDurationMS)/1000/60)
	}

	return s
}

// ─── Audio Processing ─────────────────────────────────────────────────────────

// getAudioDurationMS returns audio duration in milliseconds using ffprobe.
func getAudioDurationMS(inputBytes []byte, inputFormat string) (int, error) {
	tmpIn, err := os.CreateTemp("", "duration_*."+inputFormat)
	if err != nil {
		return 0, fmt.Errorf("create temp: %w", err)
	}
	defer os.Remove(tmpIn.Name())

	if _, err := tmpIn.Write(inputBytes); err != nil {
		tmpIn.Close()
		return 0, fmt.Errorf("write temp: %w", err)
	}
	tmpIn.Close()

	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		tmpIn.Name(),
	)

	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe failed: %w", err)
	}

	durationStr := strings.TrimSpace(string(out))
	var durationSec float64
	if _, err := fmt.Sscanf(durationStr, "%f", &durationSec); err != nil {
		return 0, fmt.Errorf("parse duration '%s': %w", durationStr, err)
	}

	return int(durationSec * 1000), nil
}

// extractAudioSegment extracts a time range from audio using ffmpeg.
func extractAudioSegment(
	inputBytes []byte,
	inputFormat string,
	startMS, endMS int,
) ([]byte, error) {

	tmpIn, err := os.CreateTemp("", "segment_in_*."+inputFormat)
	if err != nil {
		return nil, fmt.Errorf("create temp input: %w", err)
	}
	defer os.Remove(tmpIn.Name())

	if _, err := tmpIn.Write(inputBytes); err != nil {
		tmpIn.Close()
		return nil, fmt.Errorf("write temp input: %w", err)
	}
	tmpIn.Close()

	tmpOut, err := os.CreateTemp("", "segment_out_*."+inputFormat)
	if err != nil {
		return nil, fmt.Errorf("create temp output: %w", err)
	}
	tmpOutPath := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpOutPath)

	startSec := float64(startMS) / 1000.0
	durationSec := float64(endMS-startMS) / 1000.0

	cmd := exec.Command(
		"ffmpeg",
		"-i", tmpIn.Name(),
		"-ss", fmt.Sprintf("%.3f", startSec),
		"-t", fmt.Sprintf("%.3f", durationSec),
		"-c", "copy",
		"-y",
		tmpOutPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf(
			"ffmpeg segment failed: %w\nstderr: %s",
			err, stderr.String(),
		)
	}

	return os.ReadFile(tmpOutPath)
}

// compressAudio converts audio to mono 16kHz MP3 at 48k bitrate.
func compressAudio(inputBytes []byte, inputFormat string) ([]byte, string, error) {
	tmpIn, err := os.CreateTemp("", "compress_in_*."+inputFormat)
	if err != nil {
		return nil, "", fmt.Errorf("create temp input: %w", err)
	}
	defer os.Remove(tmpIn.Name())

	if _, err := tmpIn.Write(inputBytes); err != nil {
		tmpIn.Close()
		return nil, "", fmt.Errorf("write temp input: %w", err)
	}
	tmpIn.Close()

	tmpOut, err := os.CreateTemp("", "compress_out_*.mp3")
	if err != nil {
		return nil, "", fmt.Errorf("create temp output: %w", err)
	}
	tmpOutPath := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpOutPath)

	cmd := exec.Command(
		"ffmpeg",
		"-i", tmpIn.Name(),
		"-ac", "1",
		"-ar", fmt.Sprintf("%d", compressedSampleRate),
		"-ab", compressedBitrate,
		"-f", "mp3",
		"-y",
		tmpOutPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf(
			"ffmpeg compress failed: %w\nstderr: %s",
			err, stderr.String(),
		)
	}

	outBytes, err := os.ReadFile(tmpOutPath)
	if err != nil {
		return nil, "", fmt.Errorf("read compressed output: %w", err)
	}

	return outBytes, "mp3", nil
}

// ─── Chunking ─────────────────────────────────────────────────────────────────

// chunkAudio splits audio into compressed chunks ready for API submission.
func (s *TranscriptionService) chunkAudio(
	audioBytes []byte,
	audioFormat string,
) ([]audioChunk, error) {

	totalMS, err := getAudioDurationMS(audioBytes, audioFormat)
	if err != nil {
		return nil, fmt.Errorf("get audio duration: %w", err)
	}

	totalMin := float64(totalMS) / 1000 / 60
	log.Printf("[TranscriptionService] Audio duration: %.2f minutes", totalMin)

	numChunks := totalMS / chunkDurationMS
	if totalMS%chunkDurationMS != 0 {
		numChunks++
	}
	if numChunks == 0 {
		numChunks = 1
	}

	if numChunks == 1 {
		log.Printf("[TranscriptionService] Single chunk (under %.0f min)",
			float64(chunkDurationMS)/1000/60)
	} else {
		log.Printf("[TranscriptionService] Splitting into %d chunks", numChunks)
	}

	chunks := make([]audioChunk, 0, numChunks)

	for i := 0; i < numChunks; i++ {
		startMS := i * chunkDurationMS
		endMS := startMS + chunkDurationMS
		if endMS > totalMS {
			endMS = totalMS
		}

		chunkDurMin := float64(endMS-startMS) / 1000 / 60

		segmentBytes, err := extractAudioSegment(audioBytes, audioFormat, startMS, endMS)
		if err != nil {
			return nil, fmt.Errorf("extract chunk %d: %w", i+1, err)
		}

		compressed, compFmt, err := compressAudio(segmentBytes, audioFormat)
		if err != nil {
			return nil, fmt.Errorf("compress chunk %d: %w", i+1, err)
		}

		chunks = append(chunks, audioChunk{
			Data:       compressed,
			Format:     compFmt,
			Index:      i + 1,
			TotalCount: numChunks,
		})

		log.Printf("[TranscriptionService]   Chunk %d/%d: %.1f min, %d bytes",
			i+1, numChunks, chunkDurMin, len(compressed))
	}

	return chunks, nil
}

// ─── Text Cleaning ────────────────────────────────────────────────────────────

// removeRepetitiveText removes duplicate sentences and repeating blocks.
func removeRepetitiveText(text string) string {
	if len(text) < 10 {
		return text
	}

	originalLen := len(text)

	text = removeRepeatingBlocks(text, 50)

	sentenceRe := regexp.MustCompile(`[.!?]\s+`)
	sentences := sentenceRe.Split(text, -1)

	deduped := make([]string, 0, len(sentences))
	var prev string

	for _, sentence := range sentences {
		normalized := strings.ToLower(strings.TrimSpace(sentence))
		if normalized == "" {
			continue
		}
		if normalized != prev {
			deduped = append(deduped, strings.TrimSpace(sentence))
			prev = normalized
		}
	}

	text = strings.Join(deduped, " ")
	spaceRe := regexp.MustCompile(`\s+`)
	text = strings.TrimSpace(spaceRe.ReplaceAllString(text, " "))

	if originalLen != len(text) {
		reduction := float64(originalLen-len(text)) / float64(originalLen) * 100
		log.Printf("[TranscriptionService] Repetition removed: %d → %d chars (%.1f%%)",
			originalLen, len(text), reduction)
	}

	return text
}

func removeRepetitiveTextPreserveLines(text string) string {
	if len(text) < 10 {
		return text
	}

	lines := strings.Split(text, "\n")
	deduped := make([]string, 0, len(lines))
	seen := make(map[string]bool)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		normalized := strings.ToLower(trimmed)

		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		deduped = append(deduped, trimmed)
	}

	return strings.Join(deduped, "\n")
}

// removeRepeatingBlocks detects and removes blocks that repeat 3+ times.
func removeRepeatingBlocks(text string, minBlockLen int) string {
	if len(text) < minBlockLen*3 {
		return text
	}

	for blockLen := 300; blockLen >= minBlockLen; blockLen -= 20 {
		if len(text) < blockLen*2 {
			continue
		}

		i := 0
		for i < len(text)-blockLen {
			block := text[i : i+blockLen]
			repeatCount := 0
			pos := i + blockLen

			for pos+blockLen <= len(text) {
				nextBlock := text[pos : pos+blockLen]
				if textSimilarity(block, nextBlock) > 0.9 {
					repeatCount++
					pos += blockLen
				} else {
					break
				}
			}

			if repeatCount >= 2 {
				log.Printf("[TranscriptionService] Removing repeating block (~%d chars) × %d",
					blockLen, repeatCount+1)
				text = text[:i+blockLen] + text[pos:]
				continue
			}

			i += blockLen / 2
		}
	}

	return text
}

// textSimilarity returns Jaccard similarity between two text blocks.
func textSimilarity(text1, text2 string) float64 {
	if text1 == "" || text2 == "" {
		return 0.0
	}

	words1 := toWordSet(strings.ToLower(text1))
	words2 := toWordSet(strings.ToLower(text2))

	if len(words1) == 0 || len(words2) == 0 {
		return 0.0
	}

	intersection := 0
	for w := range words1 {
		if _, ok := words2[w]; ok {
			intersection++
		}
	}

	union := make(map[string]struct{})
	for w := range words1 {
		union[w] = struct{}{}
	}
	for w := range words2 {
		union[w] = struct{}{}
	}

	return float64(intersection) / float64(len(union))
}

// toWordSet splits text into a set of words.
func toWordSet(text string) map[string]struct{} {
	fields := strings.Fields(text)
	set := make(map[string]struct{}, len(fields))
	for _, w := range fields {
		set[w] = struct{}{}
	}
	return set
}

// ─── API Call ─────────────────────────────────────────────────────────────────

// callOpenRouter makes a direct HTTP call to OpenRouter with audio support.
func (s *TranscriptionService) callOpenRouter(
	ctx context.Context,
	systemPrompt string,
	userPrompt string,
	audioBase64 string,
	audioFormat string,
	temperature float64,
	maxTokens int,
) (content string, finishReason string, err error) {

	reqBody := openRouterRequest{
		Model: s.config.OpenRouterModel,
		Messages: []openRouterMessage{
			{
				Role:    "system",
				Content: systemPrompt,
			},
			{
				Role: "user",
				Content: []contentPart{
					{
						Type: "input_audio",
						InputAudio: &inputAudio{
							Data:   audioBase64,
							Format: audioFormat,
						},
					},
					{
						Type: "text",
						Text: userPrompt,
					},
				},
			},
		},
		Temperature: temperature,
		MaxTokens:   maxTokens,
		TopP:        0.95,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		openRouterBaseURL+"/chat/completions",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		return "", "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+s.config.OpenRouterAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf(
			"API error %d: %s",
			resp.StatusCode,
			string(respBytes),
		)
	}

	var result openRouterResponse
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return "", "", fmt.Errorf("parse response: %w", err)
	}

	if result.Error != nil {
		return "", "", fmt.Errorf("API error: %s", result.Error.Message)
	}

	if len(result.Choices) == 0 {
		return "", "", fmt.Errorf("no choices in response")
	}

	return result.Choices[0].Message.Content,
		result.Choices[0].FinishReason,
		nil
}

// ─── Transcribe Chunk ─────────────────────────────────────────────────────────

// transcribeChunk sends a single audio chunk to OpenRouter.
func (s *TranscriptionService) transcribeChunk(
	ctx context.Context,
	chunk audioChunk,
) (string, error) {

	audioBase64 := base64.StdEncoding.EncodeToString(chunk.Data)

	contextNote := ""
	if chunk.TotalCount > 1 {
		contextNote = fmt.Sprintf(
			" This is part %d of %d.",
			chunk.Index, chunk.TotalCount,
		)
	}

systemPrompt := `You are an expert meeting transcription specialist.
You MUST output ONLY in English language. This is mandatory.

Your task:
1. Listen to the audio carefully from start to finish
2. Transcribe exactly what is spoken — word for word
3. Translate everything into English (Hindi, Tamil, Telugu, or any other language)
4. Do NOT hallucinate or add words that were not spoken
5. Do NOT summarize or paraphrase — write exactly what was said

Output rules:
- Plain text only — no speaker labels, no timestamps, no formatting
- Every new sentence or thought on a new line
- Translate every single word into English
- If a word is a proper noun or name (like Rahul, Anan, Pritam, Zoho, SQL) keep it as-is
- Do not add commentary, descriptions, or notes
- Do not repeat sentences
- Do not make up content — only transcribe what you actually hear
- If audio is unclear or silent, skip it — do not guess`

	userPrompt := fmt.Sprintf(
		"IMPORTANT: Output in ENGLISH ONLY. "+
			"Transcribe this meeting recording exactly word for word into English. "+
			"Translate everything spoken — do not skip anything. "+
			"Do not add anything that was not spoken. "+
			"Keep proper nouns and names exactly as heard.%s",
		contextNote,
	)

	log.Printf("[TranscriptionService] Sending chunk %d/%d to OpenRouter...",
		chunk.Index, chunk.TotalCount)

	chunkStart := time.Now()

	content, finishReason, err := s.callOpenRouter(
		ctx,
		systemPrompt,
		userPrompt,
		audioBase64,
		chunk.Format,
		0.1,
		8192,
	)

	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "400") {
			log.Println("[TranscriptionService]   Possible cause: Audio format or size issue")
		} else if strings.Contains(errStr, "429") {
			log.Println("[TranscriptionService]   Possible cause: Rate limit hit")
		} else if strings.Contains(errStr, "500") || strings.Contains(errStr, "503") {
			log.Println("[TranscriptionService]   Possible cause: OpenRouter service issue")
		}
		return "", fmt.Errorf("chunk %d API call failed: %w", chunk.Index, err)
	}

	chunkTime := time.Since(chunkStart).Seconds()
	wordCount := len(strings.Fields(content))

	log.Printf("[TranscriptionService]   Chunk %d/%d done in %.2fs — %d chars, %d words",
		chunk.Index, chunk.TotalCount, chunkTime, len(content), wordCount)

	if finishReason == "length" || finishReason == "max_tokens" {
		log.Printf("[TranscriptionService]   WARNING: Chunk %d hit token limit — may be truncated",
			chunk.Index)
	}

	return strings.TrimSpace(content), nil
}

// ─── Generate Summary ─────────────────────────────────────────────────────────

// generateSummary creates a concise summary of the transcription.
func (s *TranscriptionService) generateSummary(
	ctx context.Context,
	transcription string,
) string {

	wordCount := len(strings.Fields(transcription))
	if wordCount < 15 {
		return transcription
	}

	log.Printf("[TranscriptionService] Generating summary from %d words...", wordCount)

	inputText := transcription
	if len(inputText) > 25000 {
		inputText = inputText[:25000]
	}

		systemPrompt := `You are a professional meeting summarizer.
		Create a structured, clear summary of the meeting transcript.

		Format your response EXACTLY like this:

		*Meeting Overview:
		[2-3 sentences describing the overall purpose and outcome of the meeting]

		*Key Discussion Points:
		• [Main topic discussed]
		• [Another topic discussed]
		• [Another topic discussed]

		*Decisions Made:
		• [Decision 1]
		• [Decision 2]

		*Action Items:
		• [Action item with responsible person if mentioned]
		• [Action item with responsible person if mentioned]

		*Next Steps:
		• [What happens next]
		• [Follow up items]

		Rules:
		- Only include sections that have content from the transcript
		- Do not invent or assume anything not in the transcript
		- Keep each bullet point concise and clear
		- If no decisions or action items were mentioned, skip those sections`

		userPrompt := fmt.Sprintf(
			"Create a structured meeting summary with bullet points from this transcript:\n\n%s",
			inputText,
		)
	// For summary we send text only — no audio
	reqBody := openRouterRequest{
		Model: s.config.OpenRouterModel,
		Messages: []openRouterMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature: 0.3,
		MaxTokens:   400,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		log.Printf("[TranscriptionService] ERROR: marshal summary request: %v", err)
		return fallbackSummary(transcription)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		openRouterBaseURL+"/chat/completions",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		log.Printf("[TranscriptionService] ERROR: build summary request: %v", err)
		return fallbackSummary(transcription)
	}

	req.Header.Set("Authorization", "Bearer "+s.config.OpenRouterAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		log.Printf("[TranscriptionService] ERROR: summary HTTP request: %v", err)
		return fallbackSummary(transcription)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	var result openRouterResponse
	if err := json.Unmarshal(respBytes, &result); err != nil {
		log.Printf("[TranscriptionService] ERROR: parse summary response: %v", err)
		return fallbackSummary(transcription)
	}

	if len(result.Choices) == 0 {
		return fallbackSummary(transcription)
	}

	summary := strings.TrimSpace(result.Choices[0].Message.Content)
	log.Printf("[TranscriptionService] Summary generated: %d chars", len(summary))

	if len(summary) > len(transcription) {
		log.Println("[TranscriptionService] WARNING: Summary longer than transcript — using fallback")
		return fallbackSummary(transcription)
	}

	return summary
}

// fallbackSummary returns the first 3 sentences of the transcription.
func fallbackSummary(transcription string) string {
	re := regexp.MustCompile(`[.!?]\s+`)
	sentences := re.Split(transcription, -1)
	if len(sentences) > 3 {
		return strings.Join(sentences[:3], " ") + "..."
	}
	return transcription
}

// ─── Main Entry Point ─────────────────────────────────────────────────────────

// TranscribeAudio is the main transcription workflow.
//
// Returns:
//   - result         : transcription + summary (nil on total failure)
//   - err            : error (nil on success)
//   - elapsed        : processing time in seconds
//   - postToZoho     : true if processing exceeded WaitingPeriod
func (s *TranscriptionService) TranscribeAudio(
	ctx context.Context,
	audioBytes []byte,
	recordID string,
	audioFormat string,
) (result *TranscribeResult, err error, elapsed float64, postToZoho bool) {

	startTime := time.Now()

	log.Println(strings.Repeat("=", 70))
	log.Printf("[TranscriptionService] TRANSCRIPTION START: %s", recordID)
	log.Printf("[TranscriptionService] Input  : %d bytes | Format: %s",
		len(audioBytes), audioFormat)
	log.Printf("[TranscriptionService] Model  : %s", s.config.OpenRouterModel)
	log.Println(strings.Repeat("=", 70))

	// ── Chunk audio ──
	chunks, chunkErr := s.chunkAudio(audioBytes, audioFormat)
	if chunkErr != nil {
		log.Printf("[TranscriptionService] ERROR: Audio chunking failed: %v", chunkErr)
		return nil, fmt.Errorf("audio chunking failed: %w", chunkErr), 0, false
	}

	// ── Transcribe each chunk ──
	transcriptions := make([]string, 0, len(chunks))
	var failedChunks []int

	for _, chunk := range chunks {
		log.Printf("[TranscriptionService] Processing chunk %d/%d...",
			chunk.Index, chunk.TotalCount)

		text, tErr := s.transcribeChunk(ctx, chunk)
		if tErr != nil {
			failedChunks = append(failedChunks, chunk.Index)
			log.Printf("[TranscriptionService] ERROR: Chunk %d failed: %v",
				chunk.Index, tErr)
			continue
		}

		cleaned := removeRepetitiveText(text)
		transcriptions = append(transcriptions, cleaned)
		log.Printf("[TranscriptionService]   After cleaning: %d chars, %d words",
			len(cleaned), len(strings.Fields(cleaned)))
	}

	// ── Check results ──
	if len(transcriptions) == 0 {
		elapsed = time.Since(startTime).Seconds()
		errMsg := fmt.Sprintf("all %d chunk(s) failed: %v", len(chunks), failedChunks)
		log.Printf("[TranscriptionService] ERROR: %s", errMsg)
		return nil, fmt.Errorf("%s", errMsg), elapsed, false
	}

	
	// ── Combine ──
	log.Printf("[TranscriptionService] Combining %d transcription(s)...",
	len(transcriptions))

	fullTranscription := strings.Join(transcriptions, "\n")
	fullTranscription = removeRepetitiveTextPreserveLines(fullTranscription)

	wordCount := len(strings.Fields(fullTranscription))
	charCount := len(fullTranscription)

	log.Printf("[TranscriptionService] Combined: %d chars, %d words", charCount, wordCount)

	// ── Handle very short result ──
	if charCount < 50 {
		elapsed = time.Since(startTime).Seconds()
		log.Println("[TranscriptionService] WARNING: Very short result — likely silence")

		text := fullTranscription
		if text == "" {
			text = "No speech detected in audio."
		}

		return &TranscribeResult{
			FullConversation: text,
			Summary:          "Recording contains minimal or no speech content.",
		}, nil, elapsed, true
	}

	// ── Generate summary ──
	summary := s.generateSummary(ctx, fullTranscription)

	// ── Final metrics ──
	elapsed = math.Round(time.Since(startTime).Seconds()*100) / 100
	postToZohoFlag := elapsed > s.config.WaitingPeriod

	log.Println(strings.Repeat("=", 70))
	log.Println("[TranscriptionService] TRANSCRIPTION COMPLETE")
	log.Printf("[TranscriptionService]   Chunks     : %d/%d processed",
		len(transcriptions), len(chunks))
	if len(failedChunks) > 0 {
		log.Printf("[TranscriptionService]   Failed     : %v", failedChunks)
	}
	log.Printf("[TranscriptionService]   Words      : %d", wordCount)
	log.Printf("[TranscriptionService]   Chars      : %d", charCount)
	log.Printf("[TranscriptionService]   Time       : %.2fs", elapsed)
	log.Printf("[TranscriptionService]   PostToZoho : %v", postToZohoFlag)
	log.Println(strings.Repeat("=", 70))

	finalResult := &TranscribeResult{
		FullConversation: fullTranscription,
		Summary:          summary,
	}
	s.logResultToFile(recordID, finalResult)

	return finalResult, nil, elapsed, postToZohoFlag
}
//------------------saving log in json file --------------

// logResultToFile saves transcription result as formatted JSON to logs/transcription_results.json
	func (s *TranscriptionService) logResultToFile(
		recordID string,
		result *TranscribeResult,
	) {
		os.MkdirAll("logs", 0755)
		logFilePath := filepath.Join("logs", "transcription_results.json")

		f, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf("[TranscriptionService] ERROR: open log file: %v", err)
			return
		}
		defer f.Close()

		// ── Write formatted entry manually ──
		// This keeps newlines readable instead of \n escape sequences
		entry := fmt.Sprintf(
			"{\n"+
				"  \"id\": %s,\n"+
				"  \"transcription\": %s,\n"+
				"  \"summary\": %s\n"+
				"}\n"+
				"---\n",
			jsonString(recordID),
			jsonMultiline(result.FullConversation),
			jsonMultiline(result.Summary),
		)

		if _, err := f.WriteString(entry); err != nil {
			log.Printf("[TranscriptionService] ERROR: write log: %v", err)
			return
		}

		log.Printf("[TranscriptionService] Result saved to %s", logFilePath)
	}

	// jsonString encodes a plain string value as a JSON string.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// jsonMultiline writes a string as a JSON array of lines.
// Each line becomes a separate element so the file is human-readable.
//
// Example output in file:
//
//	[
//	  "Speaker 1: Hello everyone.",
//	  "Speaker 2: Hi, good morning.",
//	  "Speaker 1: Let us begin the meeting."
//	]
func jsonMultiline(s string) string {
	lines := strings.Split(s, "\n")

	// Remove empty trailing lines
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) == 0 {
		return `""`
	}

	var sb strings.Builder
	sb.WriteString("[\n")
	for i, line := range lines {
		b, _ := json.Marshal(strings.TrimRight(line, "\r"))
		sb.WriteString("    ")
		sb.Write(b)
		if i < len(lines)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("  ]")
	return sb.String()
}
// ─── Singleton ────────────────────────────────────────────────────────────────

// DefaultTranscriptionService is the package-level singleton.
var DefaultTranscriptionService *TranscriptionService

// InitTranscriptionService creates the singleton.
func InitTranscriptionService(cfg TranscriptionConfig) {
	DefaultTranscriptionService = NewTranscriptionService(cfg)
}