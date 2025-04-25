# WhatsApp GPT Bot

A powerful WhatsApp bot that integrates with local AI models (like LM Studio) to provide automated responses, handle media, and manage conversations. Built with Go and modern best practices.

## Features

- 🤖 Seamless integration with local AI models via OpenAI-compatible API
- 📱 Full WhatsApp message support (text, images, documents)
- 💬 Conversation history management with summarization
- ⚡ Rate limiting and throttling for stability
- 🔄 Automatic reconnection and session management
- 📊 Prometheus metrics for monitoring
- 🚀 High performance with Go concurrency
- 🔒 Local data storage with SQLite

## Prerequisites

- Go 1.23.0 or later
- SQLite3
- Local AI model server (e.g., LM Studio) with OpenAI API compatibility
- WhatsApp account for bot usage

## Installation

1. Clone the repository:
```bash
git clone https://github.com/yourusername/whatsapp-bot.git
cd whatsapp-bot
```

2. Install dependencies:
```bash
go mod download
go mod tidy
```

3. Copy the environment configuration:
```bash
cp .env.example .env
```

4. Configure your environment variables in `.env`:
```env
DB_PATH=./whatsapp.db              # SQLite database path
AI_ENDPOINT=http://localhost:1234/v1  # Local AI endpoint
AI_TIMEOUT=30                      # Timeout for AI requests in seconds
WHATSAPP_LOG_LEVEL=info           # Logging level (debug/info/warn/error)
MAX_WHATSAPP_CHARS=4096           # Maximum characters per message
MODEL_NAME=local-model            # Your AI model name
CONTEXT_WINDOW=4096               # Maximum context window size
SUMMARY_THRESHOLD=10              # Messages before summarization
RATE_LIMIT_PER_SECOND=0.5        # Rate limit for message processing
```

## Usage

1. Build and run the bot:
```bash
go build
./whatsapp-gpt-bot
```

2. On first run, you'll see a QR code in the terminal. Scan this with WhatsApp on your phone to link the device.

3. The bot will now:
   - Automatically respond to messages using your local AI model
   - Handle images and documents
   - Maintain conversation history
   - Show typing indicators
   - Auto-reconnect if disconnected

## Architecture

The project is organized into several packages:

- `main.go`: Bot initialization and core logic
- `whatsapp/`: WhatsApp client implementation
- `ai/`: AI client and interface implementations
- `utils/`: Common utilities for retry logic and message handling

## Error Handling

The bot includes robust error handling:

- Automatic retries with exponential backoff
- Session persistence
- Graceful shutdown handling
- Comprehensive logging

## Troubleshooting

### Database Issues
- Ensure SQLite3 is properly installed
- Check if DB_PATH location is writable
- Try deleting whatsapp.db and rescanning QR if session is invalid

### AI Model Connection
- Verify AI_ENDPOINT is accessible
- Ensure model server supports OpenAI API format
- Check model is loaded in your local AI server
- Try increasing AI_TIMEOUT for slower models

### Compilation Problems
- Ensure Go 1.23.0+ is installed: `go version`
- Run `go mod tidy` to fix dependencies
- Verify all required packages are available

## Monitoring

Prometheus metrics are available at `:9090/metrics`:
- `whatsapp_bot_requests_total`: Message processing counter
- `whatsapp_bot_request_duration_seconds`: Processing time histogram

## Contributing

1. Fork the repository
2. Create your feature branch: `git checkout -b feature/my-feature`
3. Commit your changes: `git commit -am 'Add my feature'`
4. Push to the branch: `git push origin feature/my-feature`
5. Submit a pull request

## License

MIT License - feel free to use this project for your own purposes.

## Acknowledgments

- [go-whatsmeow](https://github.com/tulir/whatsmeow) for the WhatsApp client
- [LM Studio](https://lmstudio.ai/) for local AI model serving
- The Go community for excellent libraries and tools