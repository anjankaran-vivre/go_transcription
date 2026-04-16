package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ─── Types ────────────────────────────────────────────────────────────────────

// AudioResult holds the downloaded/extracted audio data and metadata
type AudioResult struct {
	Data     []byte
	MimeType string
	Format   string // "mp3", "mp4", "wav", "webm", "ogg", "flac"
}

// AudioService handles downloading and processing audio files
type AudioService struct {
	timeoutSeconds int
	maxSizeBytes   int64
	httpClient     *http.Client
}

// ─── Constructor ──────────────────────────────────────────────────────────────

// NewAudioService creates a new AudioService using app config
func NewAudioService(timeoutSeconds int, maxAudioSizeMB int64) *AudioService {
	maxBytes := maxAudioSizeMB * 1024 * 1024

	return &AudioService{
		timeoutSeconds: timeoutSeconds,
		maxSizeBytes:   maxBytes,
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
}

// ─── Download ─────────────────────────────────────────────────────────────────

// DownloadAudio downloads audio from a URL using the provided OAuth token.
// Returns AudioResult containing raw bytes, MIME type, and format string.
func (s *AudioService) DownloadAudio(ctx context.Context, audioURL, accessToken string) (*AudioResult, error) {
	// Truncate URL for logging
	displayURL := audioURL
	if len(displayURL) > 100 {
		displayURL = displayURL[:100] + "..."
	}
	log.Printf("[AudioService] Downloading audio from: %s", displayURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, audioURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Zoho-oauthtoken "+accessToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// Distinguish timeout from other errors
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("[AudioService] Timeout downloading audio")
			return nil, fmt.Errorf("audio download timeout")
		}
		log.Printf("[AudioService] Error downloading audio: %v", err)
		return nil, fmt.Errorf("failed to download audio: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[AudioService] HTTP error downloading audio: %d", resp.StatusCode)
		return nil, fmt.Errorf("failed to download audio: HTTP %d", resp.StatusCode)
	}

	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read audio response body: %w", err)
	}

	log.Printf("[AudioService] Downloaded %d bytes", len(audioData))

	// ── Size validation ──
	if int64(len(audioData)) > s.maxSizeBytes {
		return nil, fmt.Errorf(
			"audio file too large: %d bytes (max: %d bytes)",
			len(audioData), s.maxSizeBytes,
		)
	}
	if len(audioData) < 100 {
		return nil, fmt.Errorf("audio file too small: %d bytes", len(audioData))
	}

	// ── Format detection ──
	mimeType, format := DetectAudioType(audioData)
	log.Printf("[AudioService] Detected format: %s (%s)", strings.ToUpper(format), mimeType)

	return &AudioResult{
		Data:     audioData,
		MimeType: mimeType,
		Format:   format,
	}, nil
}

// ─── Format Detection ─────────────────────────────────────────────────────────

// DetectAudioType inspects magic bytes and returns (mimeType, format).
// Exported so it can be used independently (e.g. in tests).
func DetectAudioType(data []byte) (mimeType, format string) {
	if len(data) < 4 {
		return "audio/mp4", "mp4"
	}

	switch {
	// MP3 — sync word or ID3 tag
	case bytes.HasPrefix(data, []byte{0xff, 0xfb}),
		bytes.HasPrefix(data, []byte{0xff, 0xf3}),
		bytes.HasPrefix(data, []byte("ID3")):
		return "audio/mpeg", "mp3"

	// M4A / MP4 — 'ftyp' box appears within first 20 bytes
	case bytes.Contains(data[:min(20, len(data))], []byte("ftyp")):
		return "audio/mp4", "mp4"

	// WAV — RIFF header
	case bytes.HasPrefix(data, []byte("RIFF")):
		return "audio/wav", "wav"

	// OGG
	case bytes.HasPrefix(data, []byte("OggS")):
		return "audio/ogg", "ogg"

	// FLAC
	case bytes.HasPrefix(data, []byte("fLaC")):
		return "audio/flac", "flac"

	// WebM — EBML magic
	case bytes.HasPrefix(data, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return "audio/webm", "webm"

	default:
		log.Printf("[AudioService] Unknown audio format, defaulting to MP4")
		return "audio/mp4", "mp4"
	}
}

// ─── Audio Extraction ─────────────────────────────────────────────────────────

// ExtractAudioFromVideo extracts an MP3 audio track from raw video bytes using ffmpeg.
// ffmpeg must be installed and available on PATH.
func (s *AudioService) ExtractAudioFromVideo(videoBytes []byte) (*AudioResult, error) {
	log.Printf("[AudioService] Extracting audio from video (%d bytes)...", len(videoBytes))

	result, err := s.extractWithFFmpeg(videoBytes)
	if err != nil {
		return nil, fmt.Errorf(
			"audio extraction failed: %w\nEnsure ffmpeg is installed and available in PATH",
			err,
		)
	}
	return result, nil
}

// extractWithFFmpeg writes video to a temp file, runs ffmpeg, reads back the MP3.
func (s *AudioService) extractWithFFmpeg(videoBytes []byte) (*AudioResult, error) {
	log.Printf("[AudioService] Using ffmpeg for audio extraction...")

	// ── Write video to temp file ──
	tmpVideo, err := os.CreateTemp("", "video_input_*.mp4")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp video file: %w", err)
	}
	tmpVideoPath := tmpVideo.Name()
	defer safeRemove(tmpVideoPath)

	if _, err := tmpVideo.Write(videoBytes); err != nil {
		tmpVideo.Close()
		return nil, fmt.Errorf("failed to write temp video file: %w", err)
	}
	tmpVideo.Close()

	// ── Prepare output path ──
	tmpAudioPath := strings.TrimSuffix(tmpVideoPath, filepath.Ext(tmpVideoPath)) + "_audio.mp3"
	defer safeRemove(tmpAudioPath)

	// ── Run ffmpeg ──
	//   -i  input file
	//   -vn  no video
	//   -acodec libmp3lame  MP3 encoder
	//   -ab 128k  bitrate
	//   -ac 1  mono
	//   -y  overwrite output
	cmd := exec.Command(
		"ffmpeg",
		"-i", tmpVideoPath,
		"-vn",
		"-acodec", "libmp3lame",
		"-ab", "128k",
		"-ac", "1",
		"-y",
		tmpAudioPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf(
			"ffmpeg error: %w\nstderr: %s",
			err, stderr.String(),
		)
	}

	// ── Read extracted audio ──
	audioBytes, err := os.ReadFile(tmpAudioPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read extracted audio: %w", err)
	}

	log.Printf(
		"[AudioService] Audio extracted: %d bytes (video) → %d bytes (mp3)",
		len(videoBytes), len(audioBytes),
	)

	return &AudioResult{
		Data:     audioBytes,
		MimeType: "audio/mpeg",
		Format:   "mp3",
	}, nil
}

// ─── Field Detection ──────────────────────────────────────────────────────────

// DetectAudioField returns "Audio1" if the URL contains "/Audio1/", else "Audio".
func DetectAudioField(audioURL string) string {
	if strings.Contains(audioURL, "/Audio1/") {
		return "Audio1"
	}
	return "Audio"
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// safeRemove deletes a file, ignoring errors (used in deferred cleanup).
func safeRemove(path string) {
	if path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[AudioService] Warning: failed to remove temp file %s: %v", path, err)
		}
	}
}

// min returns the smaller of two ints (stdlib min is only available in Go 1.21+).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ─── Singleton ────────────────────────────────────────────────────────────────

// Global default instance — mirrors Python's `audio_service = AudioService()`
// Initialise this in main.go or your DI setup with real config values.
var DefaultAudioService *AudioService

// InitAudioService initialises the package-level singleton.
// Call once during application startup.
//
//	services.InitAudioService(cfg.TimeoutSeconds, cfg.MaxAudioSizeMB)
func InitAudioService(timeoutSeconds int, maxAudioSizeMB int64) {
	DefaultAudioService = NewAudioService(timeoutSeconds, maxAudioSizeMB)
}