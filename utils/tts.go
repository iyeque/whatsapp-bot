package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// TextToVoice converts text to speech using either ElevenLabs or a local provider (e.g., NeuTTS-Air)
func TextToVoice(text string) ([]byte, error) {
	provider := os.Getenv("TTS_PROVIDER")
	if provider == "LOCAL" {
		return LocalTextToVoice(text)
	}
	return ElevenLabsTextToVoice(text)
}

// ElevenLabsTextToVoice converts text to speech using ElevenLabs
func ElevenLabsTextToVoice(text string) ([]byte, error) {
	apiKey := os.Getenv("ELEVENLABS_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ELEVENLABS_API_KEY not set")
	}

	voiceID := os.Getenv("ELEVENLABS_VOICE_ID")
	if voiceID == "" {
		voiceID = "pNInz6obpgDQGcFmaJgB" // Default: Adam voice
	}

	apiURL := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s", voiceID)

	reqBody := map[string]interface{}{
		"text":     text,
		"model_id": "eleven_monolingual_v1",
		"voice_settings": map[string]float64{
			"stability":        0.5,
			"similarity_boost": 0.5,
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ElevenLabs API failed (%d): %s", resp.StatusCode, string(body))
	}

	// Read audio bytes (likely MP3)
	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return ConvertToOggOpus(audioData, "mp3")
}

// LocalTextToVoice converts text to speech using a local provider (e.g., NeuTTS-Air)
func LocalTextToVoice(text string) ([]byte, error) {
	apiURL := os.Getenv("TTS_ENDPOINT")
	if apiURL == "" {
		// Default local endpoint for a potential FastAPI/Flask wrapper for NeuTTS-Air
		apiURL = "http://localhost:8000/tts"
	}

	// Voice Cloning Parameters for NeuTTS-Air
	refAudio := os.Getenv("TTS_REFERENCE_AUDIO")
	refText := os.Getenv("TTS_REFERENCE_TEXT")

	reqBody := map[string]interface{}{
		"text":           text,
		"reference_audio": refAudio, // Path to local wav file or base64
		"reference_text":  refText,  // Text spoken in the reference audio
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Use a longer timeout for local CPU generation
	timeout := 300 * time.Second
	if timeoutStr := os.Getenv("AI_TIMEOUT"); timeoutStr != "" {
		if t, err := strconv.Atoi(timeoutStr); err == nil && t > 0 {
			timeout = time.Duration(t) * time.Second
		}
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Local TTS API failed (%d): %s", resp.StatusCode, string(body))
	}

	// Read audio bytes (assuming WAV/MP3 from local server)
	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return ConvertToOggOpus(audioData, "wav")
}

// ConvertToOggOpus converts various audio formats to OGG Opus for WhatsApp using FFmpeg
func ConvertToOggOpus(audioData []byte, inputFormat string) ([]byte, error) {
	tmpDir := os.TempDir()
	inPath := filepath.Join(tmpDir, fmt.Sprintf("voice_in_%d.%s", time.Now().UnixNano(), inputFormat))
	outPath := filepath.Join(tmpDir, fmt.Sprintf("voice_out_%d.ogg", time.Now().UnixNano()))

	if err := os.WriteFile(inPath, audioData, 0644); err != nil {
		return nil, err
	}
	defer os.Remove(inPath)

	// Convert to OGG Opus using FFmpeg
	// WhatsApp expects OGG Opus for voice notes
	cmd := exec.Command("ffmpeg", "-i", inPath, "-c:a", "libopus", "-b:a", "64k", "-vbr", "on", "-compression_level", "10", "-y", outPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg conversion failed: %w; output: %s", err, string(output))
	}
	defer os.Remove(outPath)

	// Read OGG bytes
	return os.ReadFile(outPath)
}

// ConvertOggToWav converts OGG Opus (WhatsApp format) to 16kHz Mono WAV for local Whisper
func ConvertOggToWav(oggBytes []byte) ([]byte, error) {
	tmpDir := os.TempDir()
	oggPath := filepath.Join(tmpDir, fmt.Sprintf("in_%d.ogg", time.Now().UnixNano()))
	wavPath := filepath.Join(tmpDir, fmt.Sprintf("out_%d.wav", time.Now().UnixNano()))

	if err := os.WriteFile(oggPath, oggBytes, 0644); err != nil {
		return nil, err
	}
	defer os.Remove(oggPath)

	// Convert OGG to 16kHz Mono WAV
	cmd := exec.Command("ffmpeg", "-i", oggPath, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-y", wavPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg conversion to wav failed: %w; output: %s", err, string(output))
	}
	defer os.Remove(wavPath)

	return os.ReadFile(wavPath)
}

// ConvertOggToMp3 converts OGG Opus (WhatsApp format) to MP3 for Gemini
func ConvertOggToMp3(oggBytes []byte) ([]byte, error) {
	tmpDir := os.TempDir()
	oggPath := filepath.Join(tmpDir, fmt.Sprintf("in_%d.ogg", time.Now().UnixNano()))
	mp3Path := filepath.Join(tmpDir, fmt.Sprintf("out_%d.mp3", time.Now().UnixNano()))

	if err := os.WriteFile(oggPath, oggBytes, 0644); err != nil {
		return nil, err
	}
	defer os.Remove(oggPath)

	// Convert OGG to MP3 using FFmpeg
	cmd := exec.Command("ffmpeg", "-i", oggPath, "-acodec", "libmp3lame", "-y", mp3Path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg conversion to mp3 failed: %w; output: %s", err, string(output))
	}
	defer os.Remove(mp3Path)

	return os.ReadFile(mp3Path)
}
