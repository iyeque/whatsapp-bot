# WhatsApp GPT Bot (Maximus Edition)

An advanced, context-aware "Digital Twin" WhatsApp bot. Maximus integrates with local/cloud AI models to represent its creator (Max) with high fidelity, utilizing real-time web search, personality profiling, and seamless human-in-the-loop transitions.

## 🚀 Advanced Features

- 🧠 **3-Tier Life Spark**: Combines Identity, Personality, and a RAG-based "Soul" for deep, consistent character embodiment.
- 🎙️ **Automated Voice Loop**: Native integration with local Whisper (STT) and NeuTTS-Air (TTS) for seamless voice note conversations.
- 🌐 **Real-Time Web Search**: AI-driven search loop using DuckDuckGo to fetch current events, prices, and news dynamically.
- 📅 **AI-Powered Scheduling**: Natural language task scheduling (e.g., "schedule a poem for Wilma daily at 9am").
- 📄 **Document Intelligence**: Deep parsing and summarization of `.txt`, `.md`, `.pdf`, and `.docx` files.
- 🔄 **Self-Healing Infrastructure**: Automatic monitoring and 5-second restart policy for external AI services.
- ✋ **Auto-Pilot Back-off**: Automatically pauses when Max is manually messaging, preserving natural human-to-human flow.
- 📊 **Context Intelligence**: Splits history into "Recent" and "Archived" to maintain focus and prevent hallucinations.

## 🛠 Prerequisites

- Go 1.24+
- FFmpeg (for audio conversion)
- Local Whisper.cpp & NeuTTS-Air (optional for local voice)
- LM Studio / Gemini / Groq / Cerebras API Keys

## 📦 Installation & Setup

1. **Clone & Install**:
   ```bash
   git clone <repo-url>
   go mod download
   ```

2. **Configure `.env`**:
   ```env
   DB_PATH=./whatsapp.db
   GEMINI_API_KEY=your_key
   CEREBRAS_API_KEY=your_key
   AI_ENDPOINT=http://localhost:1234/v1/chat/completions
   HUMAN_ASSISTANT_JID=your_jid@s.whatsapp.net
   ```

3. **Run**:
   ```bash
   go run main.go
   ```

## 🎮 Commands

- `new`: Connect a new account via QR code.
- `list`: Show all active sessions.
- `remove <id>`: Disconnect a specific bot.
- `quit`: Shutdown everything safely.
- `!resume` or `!autopilot`: Instantly re-engage the bot, clear failure counters, and refresh all cron schedules.
- `!schedule [request]`: Create a new AI-powered scheduled task (Natural Language).
- `!unschedule [id]` or `!remove schedule [id]`: Manage and remove active scheduled tasks.
- `!fact [text]`: Add a priority fact to the bot's long-term memory (`truth.md`).
- `!correct [text]`: Correct the bot's misinformation and update `truth.md`.
- `!tasks`: List all active scheduled tasks.
- `!status`: Get real-time system metrics.
- `!logs`: View the most recent system logs in-chat.

## 🧠 Architecture: The "Digital Twin" Workflow

1. **Perception**: Bot receives message/reaction + current date/time context.
2. **Retrieval**: RAG system pulls relevant personality data and past history.
3. **Reasoning**: AI decides if it needs a **Web Search** `[SEARCH:]`, a **Reaction** `[REACT:]`, or a **Voice Note** `[VOICE]`.
4. **Execution**: Bot performs search (if requested), re-prompts the AI with results, and sends the final response.
5. **Back-off**: If Max replies from his phone, the bot mutes itself for 2 minutes to allow natural human conversation.

## Acknowledgment
Built with `go-whatsmeow` and `LM Studio`. Special focus on privacy and local-first AI.
