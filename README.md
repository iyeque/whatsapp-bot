# WhatsApp GPT Bot

A powerful WhatsApp bot that integrates with local AI models (like LM Studio) to provide automated responses, handle media, and manage multiple WhatsApp accounts simultaneously. Built with Go and modern best practices.

## Features

- 🤖 Seamless integration with local AI models via OpenAI-compatible API
- 📱 Support for multiple WhatsApp accounts
- 💬 Full WhatsApp message support (text, images, documents)
- 🧠 Conversation history management with summarization
- ⚡ Rate limiting and throttling for stability
- 🔄 Automatic reconnection and session management
- 📊 Real-time performance dashboard with metrics
- 🚀 High performance with Go concurrency
- 🔒 Local data storage with SQLite
- 🔄 Automatic cache management
- ⏱️ Dynamic timeout adjustment

## Prerequisites

- Go 1.23.0 or later
- SQLite3
- Local AI model server (e.g., LM Studio) with OpenAI API compatibility
- WhatsApp account(s) for bot usage

## Installation

1. Clone the repository:
```bash
git clone https://github.com/yourusername/whatsapp-gpt-bot.git
cd whatsapp-gpt-bot
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

2. The bot now supports multiple WhatsApp accounts. Available commands:
   - `new` - Create and connect a new bot instance (scan QR code)
   - `list` - Show all active bot instances and their status
   - `remove <bot_id>` - Disconnect and remove a specific bot
   - `quit` - Safely shut down all bots and exit

3. Managing Multiple Accounts:
   - Start the bot and type `new` to add your first account
   - Scan the QR code with WhatsApp to connect
   - Repeat with `new` for additional accounts
   - Use `list` to see all connected accounts
   - Use `remove bot_1` to disconnect a specific account

4. Features per Account:
   - Independent conversation history
   - Separate message caching
   - Individual timeout management
   - Isolated rate limiting

## Architecture

The project is organized into several packages:

- `main.go`: Bot initialization and CLI interface
- `whatsapp/`: WhatsApp client and multi-account management
- `ai/`: AI client and interface implementations
- `utils/`: Common utilities and monitoring dashboard

## Performance Dashboard

Access the real-time performance dashboard at `http://localhost:8080/dashboard` to monitor:

- System health metrics
- Request performance
- Timeout statistics
- LM Studio performance
- Memory usage
- Active sessions

## Error Handling

The bot includes robust error handling:

- Automatic retries with exponential backoff
- Session persistence per account
- Graceful shutdown handling
- Comprehensive logging
- Dynamic timeout adjustment

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

### Multiple Accounts
- Each account needs a separate QR code scan
- Use `list` to check connection status
- If an account disconnects, use `remove` and add it again
- Check logs for account-specific issues

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