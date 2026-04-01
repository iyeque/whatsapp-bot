# Session Summary & Technical Benchmarks (2026-03-30)

## 1. External Service Automation
- **Whisper STT:**
  - **Path:** `D:/software/whisper-bin-x64/Release/whisper-server.exe`
  - **Model:** `ggml-large-v3-turbo-q5_0.bin`
  - **Endpoint:** `http://localhost:9095/inference`
  - **Working Dir:** `D:/software/whisper-bin-x64/Release`
- **NeuTTS-Air (TTS):**
  - **Script:** `tts_wrapper.py`
  - **Endpoint:** `http://localhost:8000/tts`
  - **Working Dir:** `D:/software/local-tts/neutts-air`

## 2. Core Logic Enhancements
- **Path Handling (`main.go`):**
  - Implemented `cleanEnvValue` to fix Windows `.env` path mangling (auto-correcting `\n`, `\t`, etc.).
  - Added support for `WHISPER_DIR` and `TTS_DIR` to ensure external processes run in their native directories (fixing "File Not Found" errors).
- **Context Processing (`whatsapp/bot.go`):**
  - **History Splitting:** Messages are now separated into "Recent" (last 24h) and "Archived" (older).
  - **Hallucination Prevention:** The system prompt treats archived history as "FYI ONLY," instructing the AI to never bring up old topics unless specifically asked.

## 3. Environment Conventions
- **Forward Slashes:** Always use `/` in `.env` for Windows paths to avoid escaping issues.
- **Port 9095:** Default port for local Whisper to avoid conflicts.
