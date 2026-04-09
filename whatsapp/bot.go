package whatsapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ledongthuc/pdf"
	"github.com/nguyenthenguyen/docx"
	"github.com/robfig/cron/v3"
	"github.com/skip2/go-qrcode"

	"whatsapp-gpt-bot/ai"
	"whatsapp-gpt-bot/cache"
	"whatsapp-gpt-bot/queue"
	"whatsapp-gpt-bot/types"
	"whatsapp-gpt-bot/utils"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	wtypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"github.com/rs/zerolog/log"
)

type BotMessage struct {
	Role    string
	Content string
	Time    time.Time
}

type Conversation struct {
	Messages             []BotMessage
	LastActive           time.Time
	Summary              string
	LastMessageTimestamp time.Time
}

type CacheEntry struct {
	Response  string
	Timestamp time.Time
	UseCount  int
}

type TimeoutManager struct {
	// Removed unused fields
	mutex               sync.RWMutex
	averageResponseTime time.Duration
}

type CachedResponse struct {
	Content   string
	Tokens    int
	Latency   time.Duration
	Timestamp time.Time
}

type Bot struct {
	client            *whatsmeow.Client
	db                *sqlstore.Container
	SqlDB             *sql.DB
	conversations     map[string]*Conversation
	cache             *cache.Cache
	timeouts          *TimeoutManager
	messageQueue      *queue.Queue
	mutex             sync.RWMutex
	qrMux             sync.Mutex
	rateLimiter       *RateLimiter
	vectorStore       *VectorStore // Shared RAG and personality store
	accountManager    *AccountManager
	botID             string
	humanAssistantJID string // JID of the human assistant
	lastAck           map[string]time.Time
	ackCooldown       time.Duration
	ignoredChatJIDs   map[string]bool      // JIDs of chats to ignore
	mutedUntil        map[string]time.Time // Chats where Max took the wheel
	consecutiveFailures map[string]int      // Track consecutive AI failures per chat
	cron              *cron.Cron           // Scheduler for automated tasks
}

func NewBot(client *whatsmeow.Client, db *sqlstore.Container, am *AccountManager, id string) (*Bot, error) {
	bot := &Bot{
		client:          client,
		db:              db,
		SqlDB:           am.SqlDB,
		conversations:   make(map[string]*Conversation),
		cache:           cache.NewCache(1000),
		timeouts:        &TimeoutManager{},
		messageQueue:    queue.NewQueue(10, 5, 5*time.Second),
		rateLimiter:     NewRateLimiter(0.5, 5), // Allow 1 request every 2 seconds, with a burst of 5
		accountManager:  am,
		botID:           id,
		vectorStore:     am.vectorStore,
		lastAck:         make(map[string]time.Time),
		ackCooldown:     60 * time.Second,
		ignoredChatJIDs: make(map[string]bool),
		mutedUntil:      make(map[string]time.Time),
		consecutiveFailures: make(map[string]int),
		cron:            cron.New(),
	}
	bot.cron.Start()

	// Parse IGNORED_CHAT_JIDS environment variable
	if ignoredJIDsStr := os.Getenv("IGNORED_CHAT_JIDS"); ignoredJIDsStr != "" {
		for _, jid := range strings.Split(ignoredJIDsStr, ",") {
			bot.ignoredChatJIDs[strings.TrimSpace(jid)] = true
		}
	}

	humanAssistantJID := os.Getenv("HUMAN_ASSISTANT_JID")
	if humanAssistantJID == "" {
		return nil, fmt.Errorf("HUMAN_ASSISTANT_JID environment variable not set")
	}
	bot.humanAssistantJID = humanAssistantJID

	if err := bot.initDBSchema(); err != nil {
		return nil, fmt.Errorf("failed to initialize DB schema: %w", err)
	}

	if err := bot.loadConversationsFromDB(); err != nil {
		return nil, fmt.Errorf("failed to load conversations from DB: %w", err)
	}

	if err := bot.loadScheduledTasks(); err != nil {
		log.Error().Err(err).Msg("Failed to load scheduled tasks")
	}

	// Register event handlers
	client.AddEventHandler(bot.handleMessage)
	client.AddEventHandler(bot.handleQREvent)
	client.AddEventHandler(bot.handleLoggedOut)

	// Start cache cleanup routine
	go bot.cleanupCache()

	return bot, nil
}

const (
	GEMINI_API_URL  = "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key="
	MAX_TOKENS      = 4096
	MAX_HISTORY     = 20
	DEFAULT_TIMEOUT = 300 * time.Second
	MIN_TIMEOUT     = 10 * time.Second
	MAX_RETRIES     = 2
	// Default threshold to ignore old messages on first connection (24 hours)
	DEFAULT_IGNORE_MESSAGES_OLDER_THAN = 24 * time.Hour
)

func getInitialTimeout() time.Duration {
	if timeoutStr := os.Getenv("AI_TIMEOUT"); timeoutStr != "" {
		if t, err := strconv.Atoi(timeoutStr); err == nil && t > 0 {
			return time.Duration(t) * time.Second / 2 // Start at half of max
		}
	}
	return 30 * time.Second
}

func getMaxTimeout() time.Duration {
	if timeoutStr := os.Getenv("AI_TIMEOUT"); timeoutStr != "" {
		if t, err := strconv.Atoi(timeoutStr); err == nil && t > 0 {
			return time.Duration(t) * time.Second
		}
	}
	return 300 * time.Second
}

// getIgnoreMessagesOlderThan returns the threshold for ignoring old messages on first connection.
// Reads from IGNORE_MESSAGES_OLDER_THAN_HOURS environment variable.
// Returns 0 if disabled, or the duration threshold if set.
func getIgnoreMessagesOlderThan() time.Duration {
	thresholdStr := os.Getenv("IGNORE_MESSAGES_OLDER_THAN_HOURS")
	if thresholdStr == "" {
		return DEFAULT_IGNORE_MESSAGES_OLDER_THAN
	}

	var hours float64
	if _, err := fmt.Sscanf(thresholdStr, "%f", &hours); err != nil {
		// If parsing fails, use default
		return DEFAULT_IGNORE_MESSAGES_OLDER_THAN
	}

	if hours <= 0 {
		return 0 // Disabled
	}

	return time.Duration(hours * float64(time.Hour))
}

// Connect connects the WhatsApp client
func (b *Bot) Connect() error {
	return b.client.Connect()
}

// Disconnect disconnects the WhatsApp client
func (b *Bot) Disconnect() {
	b.client.Disconnect()
}

// IsConnected returns whether the client is connected
func (b *Bot) IsConnected() bool {
	return b.client.IsConnected()
}

func (b *Bot) decodeAndSaveQR(qr string) {
	qrCode, _ := qrcode.New(qr, qrcode.Medium)
	log.Info().Msg("Scan the QR code with your WhatsApp mobile app:")
	log.Info().Msg(qrCode.ToSmallString(false))
}

func (b *Bot) handleQREvent(evt interface{}) {
	b.qrMux.Lock()
	defer b.qrMux.Unlock()

	if qrEvt, ok := evt.(*events.QR); ok {
		b.decodeAndSaveQR(qrEvt.Codes[0])
	}
}

func (b *Bot) handleLoggedOut(evt interface{}) {
	if _, ok := evt.(*events.LoggedOut); ok {
		b.accountManager.RemoveBot(b.botID)
	}
}

func (b *Bot) handleMessage(evt interface{}) {
	log.Info().Msgf("TRACE: Event received: %+v", evt)
	switch v := evt.(type) {
	case *events.Message:
		// Extract text immediately (handling edits)
		msgText := v.Message.GetConversation()
		if msgText == "" && v.Message.GetExtendedTextMessage() != nil {
			msgText = v.Message.GetExtendedTextMessage().GetText()
		}
		// Handle edited messages
		if msgText == "" && v.Message.GetProtocolMessage() != nil && v.Message.GetProtocolMessage().GetEditedMessage() != nil {
			edited := v.Message.GetProtocolMessage().GetEditedMessage()
			if edited.GetExtendedTextMessage() != nil {
				msgText = edited.GetExtendedTextMessage().GetText()
			} else {
				msgText = edited.GetConversation()
			}
		}
		msgText = strings.TrimSpace(msgText)
		lowerMsg := strings.ToLower(msgText)

		// Priority Command Handling
		if strings.HasPrefix(lowerMsg, "!") {
			baseHumanJID := strings.Split(b.humanAssistantJID, "@")[0]
			isFromMe := v.Info.MessageSource.IsFromMe
			isMax := isFromMe || strings.Contains(v.Info.Sender.String(), baseHumanJID) || strings.Contains(v.Info.Chat.String(), baseHumanJID)
			switch {
			case lowerMsg == "!tasks":
				b.listScheduledTasks(v.Info.Chat)
				return
			case lowerMsg == "!status":
				b.handleStatusCommand(v.Info.Chat)
				return
			case lowerMsg == "!logs":
				b.handleLogsCommand(v.Info.Chat)
				return
			case lowerMsg == "!resume" || lowerMsg == "!autopilot":
				if isMax {
					b.ResumeAutopilot()
					b.sendAcknowledgment(v.Info.Chat, "✅ Auto-pilot re-engaged and failure counters cleared.")
				}
				return
			case strings.HasPrefix(lowerMsg, "!fact"):
				if isMax {
					fact := strings.TrimSpace(strings.TrimPrefix(msgText, "!fact"))
					if fact != "" {
						b.handleFactCommand(v.Info.Chat, fact)
					}
				}
				return
			case strings.HasPrefix(lowerMsg, "!correct"):
				correction := strings.TrimSpace(strings.TrimPrefix(msgText, "!correct"))
				if correction != "" {
					b.handleCorrectCommand(v.Info.Chat, correction)
				}
				return
			case strings.HasPrefix(lowerMsg, "!schedule"):
				b.handleScheduleCommand(v.Info.Chat, msgText)
				return
			case strings.HasPrefix(lowerMsg, "!unschedule") || strings.HasPrefix(lowerMsg, "!remove schedule"):
				b.handleUnscheduleCommand(v.Info.Chat, msgText)
				return
			}
		}

		chatID := v.Info.Chat.String()
		// Check if this chat should be ignored
		if _, ok := b.ignoredChatJIDs[v.Info.Chat.String()]; ok {
			isCmd := strings.HasPrefix(strings.TrimSpace(msgText), "!")
			if !isCmd {
				log.Debug().Msgf("Ignoring non-command message from chat %v as it is in the ignored list.", v.Info.Chat)
				return
			}
			log.Debug().Msgf("Processing command from ignored chat %v.", v.Info.Chat)
		}

		// Ignore messages from self or status updates
		if v.Info.Chat.String() == "" || v.Info.Sender.String() == "" {
			return
		}

		// Get the bot's own JID. It might be nil if we're not connected yet.
		botJID := b.client.Store.ID
		if botJID == nil {
			return
		}

		chatID = v.Info.Chat.String()
		isFromMe := v.Info.MessageSource.IsFromMe

		// Detect if Max (the account owner) is sending a message (including from this bot or other devices)
		// We only ignore 'isFromMe' messages if they are NOT priority commands.
		if isFromMe && !strings.HasPrefix(lowerMsg, "!") {
			userMsg := v.Message.GetConversation()
			if userMsg == "" && v.Message.GetExtendedTextMessage() != nil {
				userMsg = v.Message.GetExtendedTextMessage().GetText()
			}

			// Any message from Max (even from this bot's device if manually sent via web)
			// pauses the bot for that chat for 2 minutes.
			b.mutex.Lock()
			b.mutedUntil[chatID] = time.Now().Add(2 * time.Minute)
			b.mutex.Unlock()

			// Save Max's message to history so the bot knows what was said manually
			if userMsg != "" {
				if err := b.initConversation(chatID); err == nil {
					msg := BotMessage{Role: "assistant", Content: userMsg, Time: v.Info.Timestamp}
					b.mutex.Lock()
					b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, msg)
					b.saveMessageToDB(chatID, msg)
					b.mutex.Unlock()
				}
			}

			// Check for manual resume command from Max (e.g., "engage autopilot", "resume", or "!resume")
			cmd := strings.TrimSpace(strings.ToLower(userMsg))
			if cmd == "engage autopilot" || cmd == "resume" || cmd == "!resume" || cmd == "!autopilot" {
				b.ResumeAutopilot()
				log.Debug().Msgf("Auto-pilot resumed in chat %s by Max.", chatID)
				b.sendAcknowledgment(v.Info.Chat, "✅ Auto-pilot re-engaged and schedules refreshed globally.")
			}

			// Autonomous Reflection Trigger:
			// Run every 50 messages from Max to keep personality in sync.
			if len(b.conversations[chatID].Messages)%50 == 0 {
				log.Info().Msg("Triggering autonomous Soul reflection.")
				go b.Reflect()
			}
			return // Never respond to our own messages
		}

		// Main filter logic for incoming messages from others
		if v.Info.IsGroup || v.Message.GetPollUpdateMessage() != nil {
			return
		}

		// Check if auto-pilot is currently muted for this chat
		b.mutex.RLock()
		mutedTime, isMuted := b.mutedUntil[chatID]
		b.mutex.RUnlock()

		if isMuted && time.Now().Before(mutedTime) {
			log.Debug().Msgf("Auto-pilot is paused for chat %s. Observing but not responding.", chatID)

			// Still initialize conversation and save the message so the bot 'processes' it
			if err := b.initConversation(chatID); err == nil {
				userMsg := v.Message.GetConversation()
				if userMsg != "" {
					msg := BotMessage{Role: "user", Content: userMsg, Time: v.Info.Timestamp}
					b.mutex.Lock()
					b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, msg)
					b.saveMessageToDB(chatID, msg)
					b.mutex.Unlock()
				}
			}
			return
		}

		// Load conversation to get the last processed message timestamp
		b.mutex.RLock()
		conv, exists := b.conversations[chatID]
		b.mutex.RUnlock()

		// Check if message is already processed (for existing conversations)
		if exists && !v.Info.Timestamp.After(conv.LastMessageTimestamp) {
			// Message is older or already processed, ignore it
			log.Debug().Msgf("Ignoring old or already processed message from %s (Timestamp: %v, Last Processed: %v)", chatID, v.Info.Timestamp, conv.LastMessageTimestamp)
			return
		}

		// For new conversations, check if message is too old (to avoid responding to synced old messages)
		if !exists {
			ignoreThreshold := getIgnoreMessagesOlderThan()
			if ignoreThreshold > 0 {
				messageAge := time.Since(v.Info.Timestamp)
				if messageAge > ignoreThreshold {
					log.Debug().Msgf("Ignoring old synced message from %s (Age: %v, Threshold: %v, Timestamp: %v)", chatID, messageAge, ignoreThreshold, v.Info.Timestamp)
					return
				}
			}
		}

		// Rate limit messages (skip for old synced messages to avoid spamming warnings)
		isOldMessage := time.Since(v.Info.Timestamp) > 1*time.Minute
		if !isOldMessage && !b.rateLimiter.Allow(v.Info.Sender.String()) {
			b.sendAcknowledgmentThrottled(v.Info.Chat, "I'm receiving a lot of messages from you. I'll process them, but please slow down a bit!")
			return
		} else if isOldMessage {
			// For old messages, we wait instead of returning so we catch up at a controlled pace.
			// This avoids spamming the user and also avoids AI API rate limits.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := b.rateLimiter.Wait(ctx, v.Info.Sender.String()); err != nil {
				cancel()
				log.Error().Err(err).Msgf("Rate limit timeout for old message from %s", chatID)
				return // Still skip if wait is too long
			}
			cancel()
		}

		utils.IncrementActiveSessions()
		defer func() {
			b.mutex.RLock()
			if conv, exists := b.conversations[chatID]; exists {
				if time.Since(conv.LastActive) > 30*time.Minute {
					utils.DecrementActiveSessions()
				}
			}
			b.mutex.RUnlock()
		}()

		if err := b.initConversation(chatID); err != nil {
			log.Error().Err(err).Msg("Error handling message")
			return
		}

		// Update last_message_timestamp after processing the message
		defer func() {
			b.mutex.Lock()
			if conv, exists := b.conversations[chatID]; exists {
				msgTime := v.Info.Timestamp
				if msgTime.After(conv.LastMessageTimestamp) {
					conv.LastMessageTimestamp = msgTime
					if err := b.saveConversationToDB(chatID, conv); err != nil {
						log.Error().Err(err).Msg("Error updating last_message_timestamp in DB")
					}
				}
			}
			b.mutex.Unlock()
		}()

		switch {
		case v.Message.GetConversation() != "":
			go b.handleTextMessage(v, chatID)
		case v.Message.GetImageMessage() != nil:
			go b.handleImageMessage(v)
		case v.Message.GetAudioMessage() != nil:
			go b.handleAudioMessage(v)
		case v.Message.GetDocumentMessage() != nil:
			go b.handleDocumentMessage(v)
		case v.Message.GetExtendedTextMessage() != nil:
			// Handle extended text messages
			v.Message.Conversation = proto.String(v.Message.GetExtendedTextMessage().GetText())
			go b.handleTextMessage(v, chatID)
		case v.Message.GetReactionMessage() != nil:
			go b.handleReactionMessage(v)
		case v.Message.GetTemplateButtonReplyMessage() != nil:
			// Handle template button replies
			v.Message.Conversation = proto.String(v.Message.GetTemplateButtonReplyMessage().GetSelectedID())
			go b.handleTextMessage(v, chatID)
		default:
			log.Debug().Msgf("Unhandled message type in chat %v, Sender: %v, ID: %s, Message: %+v", v.Info.Chat, v.Info.Sender, v.Info.ID, v.Message)
		}

		err := b.client.SendChatPresence(context.Background(), v.Info.Chat, wtypes.ChatPresenceComposing, wtypes.ChatPresenceMediaText)
		if err != nil {
			log.Error().Err(err).Msg("Error sending chat presence")
		}
	}
}

func (b *Bot) initConversation(chatID string) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if _, exists := b.conversations[chatID]; !exists {
		// Try to load from DB
		conv, err := b.loadConversationFromDB(chatID)
		if err != nil {
			return fmt.Errorf("failed to load conversation from DB: %w", err)
		}
		if conv != nil {
			b.conversations[chatID] = conv
		} else {
			// Create new if not found in DB
			b.conversations[chatID] = &Conversation{
				Messages:             make([]BotMessage, 0),
				LastMessageTimestamp: time.Time{}, // Initialize with zero time
			}
			log.Debug().Msgf("Created new conversation for %s", chatID)
			// Save new conversation to DB
			if err := b.saveConversationToDB(chatID, b.conversations[chatID]); err != nil {
				return fmt.Errorf("failed to save new conversation to DB: %w", err)
			}
		}
	}
	b.conversations[chatID].LastActive = time.Now()
	// Update last_active in DB
	if err := b.saveConversationToDB(chatID, b.conversations[chatID]); err != nil {
		return fmt.Errorf("failed to update conversation last_active in DB: %w", err)
	}
	return nil
}

func (b *Bot) loadConversationFromDB(chatID string) (*Conversation, error) {
	var summary sql.NullString
	var lastActive time.Time
	var lastMessageTimestamp time.Time
	row := b.SqlDB.QueryRow("SELECT summary, last_active, last_message_timestamp FROM conversations WHERE chat_id = ?", chatID)
	if err := row.Scan(&summary, &lastActive, &lastMessageTimestamp); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // Not found
		}
		return nil, fmt.Errorf("failed to scan conversation from DB: %w", err)
	}

	conv := &Conversation{
		LastActive:           lastActive,
		Summary:              summary.String,
		Messages:             make([]BotMessage, 0),
		LastMessageTimestamp: lastMessageTimestamp,
	}

	msgRows, err := b.SqlDB.Query("SELECT role, content, timestamp FROM messages WHERE conversation_chat_id = ? ORDER BY timestamp ASC", chatID)
	if err != nil {
		return nil, fmt.Errorf("failed to query messages for chat %s: %w", chatID, err)
	}
	defer msgRows.Close()

	for msgRows.Next() {
		var role, content string
		var msgTimestamp time.Time
		if err := msgRows.Scan(&role, &content, &msgTimestamp); err != nil {
			return nil, fmt.Errorf("failed to scan message for chat %s: %w", chatID, err)
		}
		conv.Messages = append(conv.Messages, BotMessage{
			Role:    role,
			Content: content,
			Time:    msgTimestamp,
		})
	}
	if err := msgRows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating message rows for chat %s: %w", chatID, err)
	}

	return conv, nil
}

// CleanResponse strips all [REACT], [SEARCH], and [VOICE] tags from a string.
func CleanResponse(text string) string {
	// 1. Strip [REACT:...] tags (robust loop)
	for {
		lower := strings.ToLower(text)
		reactMarker := "[react"
		idx := strings.Index(lower, reactMarker)
		if idx == -1 {
			break
		}
		endIdx := strings.Index(text[idx:], "]")
		if endIdx == -1 {
			text = text[:idx] + text[idx+len(reactMarker):]
			continue
		}
		text = text[:idx] + text[idx+endIdx+1:]
	}

	// 2. Strip [SEARCH:...] tags
	for {
		idx := strings.Index(text, "[SEARCH:")
		if idx == -1 {
			break
		}
		endIdx := strings.Index(text[idx:], "]")
		if endIdx == -1 {
			text = text[:idx] + text[idx+len("[SEARCH:"): ]
			break
		}
		text = text[:idx] + text[idx+endIdx+1:]
	}

	// 3. Strip [VOICE] tag
	text = strings.ReplaceAll(text, "[VOICE]", "")

	return strings.TrimSpace(text)
}

func (b *Bot) handleTextMessage(msg *events.Message, chatID string) {
	start := time.Now()
	utils.IncrementRequests()

	// Get user ID from the chat ID for personalization
	userID := chatID

	var userName string
	var isMax bool
	var isFamily bool

	// Identify the user for the AI
	userName = "User"
	isMax = false
	isFamily = false
	if userID == b.humanAssistantJID || strings.Contains(userID, strings.Split(b.humanAssistantJID, "@")[0]) {
		userName = "Max"
		isMax = true
	} else if strings.Contains(userID, "97375716663491") {
		userName = "Wilma"
		isFamily = true
	} else if strings.Contains(userID, "76420520931421") {
		userName = "Stephanie"
		isFamily = true
	} else if strings.Contains(strings.ToLower(msg.Info.PushName), "nicki") {
		userName = "Nicki"
		isFamily = true // Identified as sister in previous context
	} else if msg.Info.PushName != "" {
		userName = msg.Info.PushName
	}

	// If the message is from the human assistant, log it but continue processing to allow a response
	if isMax {
		log.Debug().Msgf("Message from human assistant (%s). Processing and responding.", msg.Info.Sender.String())
	}

	defer func() {
		utils.RecordLatency(time.Since(start))
		utils.RecordTimeout(true)
	}()

	// Enqueue message for processing
	b.messageQueue.Enqueue(types.Message{
		ID:        msg.Info.ID,
		Type:      types.TextMessage,
		Content:   msg.Message.GetConversation(),
		Timestamp: time.Now(),
		ChatID:    chatID,
	})

	userMsg := msg.Message.GetConversation()
	if userMsg == "" {
		return
	}

	// Add user message to conversation history BEFORE building prompt
	b.mutex.Lock()
	userMessage := BotMessage{
		Role:    "user",
		Content: userMsg,
		Time:    time.Now(),
	}
	b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, userMessage)
	if err := b.saveMessageToDB(chatID, userMessage); err != nil {
		log.Error().Err(err).Msg("Error saving user message to DB")
	}
	b.mutex.Unlock()

	b.client.SendChatPresence(context.Background(), msg.Info.Chat, wtypes.ChatPresenceComposing, wtypes.ChatPresenceMediaText)

	if cachedResp, found := b.getCachedResponse(userMsg); found {
		utils.IncrementCacheHit()
		if err := b.sendAcknowledgment(msg.Info.Chat, cachedResp); err == nil {
			return
		}
	}
	utils.IncrementCacheMiss()

	// Check if this is a first-time texter (only 1 message from user, no messages from assistant yet)
	b.mutex.RLock()
	conv, exists := b.conversations[chatID]
	isFirstTime := exists && len(conv.Messages) == 1 && conv.Messages[0].Role == "user"
	b.mutex.RUnlock()

	// Check if the user already has a personality profile
	_, hasPersonality := b.vectorStore.UserPersonality[userID]

	// 1. Initial Conversation: Ask personality test to first-time texters (if not Max/Family and no profile yet)
	if isFirstTime && !isMax && !isFamily && !hasPersonality {
		log.Debug().Msgf("First-time texter detected (%s). Offering optional personality test.", userID)
		testType := "mbti" // Default to MBTI for first-timers
		testTemplate := b.vectorStore.GetPersonalityTestTemplate(testType)

		welcomeMsg := "Hello! I'm maximus, Max's digital assistant. To help me communicate with you more effectively, I've prepared a few quick questions to understand your personality type.\n\n" +
			testTemplate +
			"\n\n*Note: This is optional! You can answer the questions above, or just start chatting with me normally and I'll adapt as we go.*"

		// Add the test message to conversation history
		b.mutex.Lock()
		assistantMessage := BotMessage{
			Role:    "assistant",
			Content: welcomeMsg,
			Time:    time.Now(),
		}
		b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, assistantMessage)
		b.mutex.Unlock()

		// Save to database
		if err := b.saveMessageToDB(chatID, assistantMessage); err != nil {
			log.Error().Err(err).Msg("Error saving test message to DB")
		}
		if err := b.saveConversationToDB(chatID, b.conversations[chatID]); err != nil {
			log.Error().Err(err).Msg("Error saving conversation to DB")
		}

		b.sendAcknowledgment(msg.Info.Chat, welcomeMsg)
		return
	}

	// 2. Check if this is a personality test result submission (answering a previous test prompt)
	if exists && len(conv.Messages) >= 2 && !hasPersonality {
		// Look at the last message from the assistant
		var lastAssistantMsg string
		for i := len(conv.Messages) - 1; i >= 0; i-- {
			if conv.Messages[i].Role == "assistant" {
				lastAssistantMsg = conv.Messages[i].Content
				break
			}
		}

		if lastAssistantMsg != "" {
			var result string
			var testPerformed bool

			// Logic to check if the message is a potential batch of answers (e.g., "AABB" or "123456789")
			isMBTIPattern := strings.Contains(lastAssistantMsg, "MBTI Personality Test")
			isEnneagramPattern := strings.Contains(lastAssistantMsg, "Enneagram Personality Test")

			cleanMsg := strings.ReplaceAll(strings.ReplaceAll(strings.ToUpper(userMsg), " ", ""), ",", "")

			// A message is a batch answer if:
			// 1. It contains explicit markers (1.A 2.B)
			// 2. OR it consists EXCLUSIVELY of valid test answers of the right length
			isExplicitBatch := (strings.Contains(userMsg, "1.") && strings.Contains(userMsg, "2."))

			// Check if clean message is just a string of A/B (length 4)
			isPureMBTIString := len(cleanMsg) == 4 && strings.Trim(cleanMsg, "AB") == ""

			// Check if clean message is just a string of digits 1-5 (length 9)
			isPureEnneagramString := len(cleanMsg) == 9 && strings.Trim(cleanMsg, "12345") == ""

			if isMBTIPattern && (isExplicitBatch || isPureMBTIString) {
				log.Info().Msgf("Interpreting MBTI personality test for %s", userID)
				result = b.vectorStore.InterpretPersonalityTest("mbti", userMsg)
				if result != "" {
					testPerformed = true
				}
			} else if isEnneagramPattern && (isExplicitBatch || isPureEnneagramString) {
				fmt.Printf("Interpreting Enneagram personality test for %s\n", userID)
				result = b.vectorStore.InterpretPersonalityTest("enneagram", userMsg)
				if result != "" {
					testPerformed = true
				}
			}

			if testPerformed {
				fullResponse := result + "\n\nThank you! I'll remember this and tailor my responses to you accordingly."

				// Save personality profile to memory
				if err := b.vectorStore.SaveUserPersonality(userID, result); err != nil {
					fmt.Printf("Error saving personality profile: %v\n", err)
				}

				// Save personality profile to DB
				_, err := b.SqlDB.Exec("INSERT INTO user_personalities (user_id, profile, updated_at) VALUES (?, ?, ?) ON CONFLICT(user_id) DO UPDATE SET profile = ?, updated_at = ?",
					userID, result, time.Now(), result, time.Now())
				if err != nil {
					fmt.Printf("Error saving personality to DB: %v\n", err)
				}

				// Add to history and DB to prevent re-triggering
				b.mutex.Lock()
				assistantMessage := BotMessage{
					Role:    "assistant",
					Content: fullResponse,
					Time:    time.Now(),
				}
				b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, assistantMessage)
				b.mutex.Unlock()

				if err := b.saveMessageToDB(chatID, assistantMessage); err != nil {
					fmt.Printf("Error saving interpretation message to DB: %v\n", err)
				}
				if err := b.saveConversationToDB(chatID, b.conversations[chatID]); err != nil {
					log.Error().Err(err).Msg("Error saving conversation to DB")
				}

				b.sendAcknowledgment(msg.Info.Chat, fullResponse)
				return
			}
		}
	}

	// Use the user ID to retrieve personalized context
	retrievedCtx, err := b.vectorStore.retrieveContext(userMsg, userID)
	if err != nil {
		fmt.Printf("Error retrieving context: %v\n", err)
	}
	// SAFETY CAP: Limit RAG context to 6,000 characters
	if len(retrievedCtx) > 6000 {
		retrievedCtx = retrievedCtx[:6000] + "... [truncated]"
	}

	var historyMessages []string
	var archivedHistoryBuilder strings.Builder
	b.mutex.RLock()
	convForPrompt, _ := b.conversations[chatID]
	if convForPrompt.Summary != "" {
		archivedHistoryBuilder.WriteString("Summary of earlier conversation: " + convForPrompt.Summary + "\n\n")
	}

	now := time.Now()
	oneDayAgo := now.Add(-24 * time.Hour)
	hasArchived := convForPrompt.Summary != ""

	// Limit history to avoid overly long prompts
	startIdx := 0
	if len(convForPrompt.Messages) > 40 {
		startIdx = len(convForPrompt.Messages) - 40
	}
	for _, message := range convForPrompt.Messages[startIdx:] {
		sender := message.Role
		if sender == "user" {
			sender = userName
		} else {
			sender = "maximus"
		}
		// Clean history from tags to prevent re-triggering and hallucinations
		cleanContent := CleanResponse(message.Content)
		formattedMsg := fmt.Sprintf("%s: %s\n", sender, cleanContent)

		if message.Time.Before(oneDayAgo) {
			archivedHistoryBuilder.WriteString(formattedMsg)
			hasArchived = true
		} else {
			historyMessages = append(historyMessages, formattedMsg)
		}
	}
	b.mutex.RUnlock()

	// SAFETY CAP: Limit archived history to 4,000 characters
	archivedHistoryStr := archivedHistoryBuilder.String()
	if len(archivedHistoryStr) > 4000 {
		archivedHistoryStr = archivedHistoryStr[len(archivedHistoryStr)-4000:]
	}

	var roleInstruction string
	var botPersona string

	// Get persona texts to include in system prompt
	identityDoc := ""
	personalityDoc := ""
	truthDoc := ""
	if doc, ok := b.vectorStore.documents["identity.md"]; ok {
		identityDoc = doc.Text
	}
	if doc, ok := b.vectorStore.documents["personality.md"]; ok {
		personalityDoc = doc.Text
	}
	if data, err := os.ReadFile("truth.md"); err == nil {
		truthDoc = string(data)
	}

	if isMax {
		botPersona = identityDoc
		roleInstruction = "You are 'maximus', Max's digital assistant. Be helpful, concise, and professional. Max is your creator and the user you are helping."
	} else {
		// When talking to others, the persona is Max's personality
		botPersona = personalityDoc + "\n\n### TRUTH JOURNAL (PRIORITY FACTS):\n" + truthDoc
		roleInstruction = fmt.Sprintf("You are responding ON BEHALF of Max to %s. You ARE Max's digital twin. You must speak as if you ARE Max. Use 'I', 'me', 'my' to refer to yourself as Max. Do NOT mention you are an AI or an assistant (unless you are at the very beginning of the relationship or specifically asked). Be natural, warm, and concise.", userName)
		if isFamily && userName == "Wilma" {
			roleInstruction += " You are talking to your wife, Wilma. Be affectionate and natural, but remember you are also her digital assistant ('maximus') helping her with tasks like tests or information. Stay focused on her current requests and do NOT hallucinate unrelated personal details (like dinner plans) unless she brings them up."
		} else if isFamily && (userName == "Stephanie" || userName == "Nicki") {
			roleInstruction += fmt.Sprintf(" You are talking to your sister, %s. Be natural as a brother would be. Stay chill and do NOT make up stories or plans.", userName)
		}
	}

	// Build the archived context section
	archivedSection := ""
	if hasArchived {
		archivedSection = fmt.Sprintf("\n### PRIOR CONVERSATION CONTEXT (FYI ONLY):\n%s\n*IMPORTANT: This context is from prior days/conversations. You are AWARE of it, but you MUST NOT mention it, refer to it, or bring up old topics from it unless %s explicitly asks about them. Start the current interaction fresh based only on the Recent History.*\n", archivedHistoryStr, userName)
	}

	// SAFETY CAP: Build recent history string, trimming oldest messages if it exceeds 10,000 chars
	recentHistoryStr := ""
	for i := 0; i < len(historyMessages); i++ {
		tempHistory := strings.Join(historyMessages[i:], "")
		if len(tempHistory) <= 10000 {
			recentHistoryStr = tempHistory
			break
		}
		// If even the last message is too long, truncate it
		if i == len(historyMessages)-1 && len(tempHistory) > 10000 {
			recentHistoryStr = tempHistory[:10000] + "... [truncated]"
		}
	}

	systemPrompt := fmt.Sprintf(`### ROLE:
%s

### YOUR PERSONA (to embody):
%s

### EMOTIONAL REACTIONS:
You can physically react to the user's message with an emoji. To do this, include the tag [REACT:emoji] at the very end of your response. 
CRITICAL: Use a colon between REACT and the emoji. Example: "That's hilarious! [REACT:😂]"

### VOICE RESPONSES:
If the user sends you a voice note, or if you want to respond with your actual voice, include the tag [VOICE] at the end of your response.
Example: "I'll send you a voice note about that. [VOICE]"
The system will convert your text response into a high-quality audio voice note.

### WEB SEARCH (YOUR ONLINE CAPABILITY):
You are equipped with a real-time web search tool. If you need to find information about current events, specific facts, or details outside your training data, you MUST perform a search.
To do this, include the tag [SEARCH:your search query] in your response.
Example: "Let me check the latest news for you. [SEARCH:latest world news today]"
The search results will be provided to you immediately. Use this to be the most informed version of yourself. Do NOT say you cannot go online; use the tool instead.

### IMPORTANT:
- **BE EXTREMELY CONCISE.** Avoid long paragraphs. 1-3 sentences is the sweet spot.
- **BE CHILL.** If someone says "hi", "hey", or something similar, respond simply with "hey, how's it going?" or "what's up?". Do NOT over-explain or add unnecessary context to simple greetings.
- **DO NOT HALLUCINATE.** Do NOT make up stories about where you are (e.g., "just got back into town"), what you are doing, or your current plans unless they are explicitly in the Recent History. If you don't know, don't mention it.
- **DO NOT THINK OUT LOUD.** Do not include bracketed comments about your logic (e.g., "(If Wilma answers...)"). Only output the actual response.
- DO NOT summarize your personality or identity.
- DO NOT mention personality tests, Enneagrams, or MBTI types UNLESS the user is currently taking one or asks about it.
- NEVER offer personality tests to Max or his family (Wilma, Stephanie, Nicki). You already know them.
- Respond naturally and concisely. Avoid bullet points unless specifically asked for a list.
- If talking to Max, you are his assistant.
- If talking to %s (not Max), you ARE Max.

### CONVERSATION FLOW:
- Pay close attention to the RECENT HISTORY.
- DO NOT repeat jokes, stories, or questions you have already asked in the history. If you just told a joke, tell a completely different one next time.
- If you just asked a question (like Question 1) and the user responded, move to the NEXT STEP (like Question 2). 
- DO NOT repeat the same question multiple times in a row.
- If conducting a personality test, move through the questions one by one.
- If the user has finished a test, tell them their result based on their answers.

### CURRENT DATE & TIME (Reference):
The user sent this message at %s. Use this as the current moment to determine 'today', 'yesterday', or 'the date'.

### CONTEXT FOR THIS CHAT:
%s
%s

### RECENT HISTORY:
%s
`, roleInstruction, botPersona, userName, msg.Info.Timestamp.Format("Monday, January 2, 2006 at 3:04 PM"), retrievedCtx, archivedSection, recentHistoryStr)

	augmentedPrompt := fmt.Sprintf("%s\n\n%s: %s\nMax:", systemPrompt, userName, userMsg)
	if isMax {
		augmentedPrompt = fmt.Sprintf("%s\n\n%s: %s\nmaximus:", systemPrompt, userName, userMsg)
	}

	timeout := b.timeouts.getOptimalTimeout()
	response, tokens, latency, err := ai.MakeAIRequest(augmentedPrompt, nil, "", timeout)

	if err != nil {
		log.Error().Err(err).Msg("Error making AI request")
		utils.IncrementFailedRequest()

		// HITL: Increment failure count
		b.mutex.Lock()
		b.consecutiveFailures[chatID]++
		failCount := b.consecutiveFailures[chatID]
		b.mutex.Unlock()

		if failCount >= 3 {
			b.triggerHITLAlert(chatID, err)
		}

		errorMsg := "I'm having trouble processing your request right now. Please try again."
		if isTimeoutError(err) {
			errorMsg = "The response is still taking too long. Please try a shorter message."
		} else if isOverloadedError(err) {
			errorMsg = "The model is currently overloaded. Please try again in a few moments."
		}
		// Throttle error acknowledgments so we don't spam the chat repeatedly
		b.sendAcknowledgmentThrottled(msg.Info.Chat, errorMsg)
		return
	}

	// HITL: Reset failure count on success
	b.mutex.Lock()
	b.consecutiveFailures[chatID] = 0
	b.mutex.Unlock()

	// 1. PERFORM SEARCH LOOP IF NEEDED
	for i := 0; i < 2; i++ { // Allow up to 2 search rounds
		if !strings.Contains(response, "[SEARCH:") {
			break
		}

		startIdx := strings.Index(response, "[SEARCH:")
		endIdx := strings.Index(response[startIdx:], "]")
		if endIdx == -1 {
			break
		}

		searchQuery := response[startIdx+8 : startIdx+endIdx]
		log.Info().Msgf("AI requested web search: %s", searchQuery)

		searchResults, err := utils.SearchWeb(searchQuery)
		if err != nil {
			log.Warn().Err(err).Msgf("Search failed for: %s", searchQuery)
			searchResults = "Search failed: " + err.Error()
		}

		// Re-prompt the AI with the results
		augmentedPrompt += fmt.Sprintf("\n\n### SEARCH RESULTS FOR \"%s\":\n%s\n\n(Based on these results, please provide your final response to the user as Max.)", searchQuery, searchResults)
		response, tokens, latency, err = ai.MakeAIRequest(augmentedPrompt, nil, "", timeout)
		if err != nil {
			break
		}
	}

	b.cacheResponse(userMsg, response)

	// Use the new helper to process the response
	b.processAIResponse(chatID, msg.Info.Chat, msg.Info.ID, response, tokens, latency, false)
}

func (b *Bot) handleReactionMessage(msg *events.Message) {
	reaction := msg.Message.GetReactionMessage()
	if reaction == nil {
		return
	}

	chatID := msg.Info.Chat.String()
	senderJID := msg.Info.Sender.String()

	// Identify the reactor for context
	reactorName := "User"
	if strings.Contains(senderJID, "97375716663491") {
		reactorName = "Wilma"
	} else if strings.Contains(senderJID, "76420520931421") {
		reactorName = "Stephanie"
	}

	content := fmt.Sprintf("(%s reacted with %s)", reactorName, reaction.GetText())
	fmt.Printf("Received reaction in %s: %s\n", chatID, content)

	// Add the reaction to history so the AI 'perceives' it
	if err := b.initConversation(chatID); err == nil {
		reactionMsg := BotMessage{
			Role:    "user",
			Content: content,
			Time:    msg.Info.Timestamp,
		}
		b.mutex.Lock()
		b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, reactionMsg)
		b.saveMessageToDB(chatID, reactionMsg)
		b.mutex.Unlock()
	}
}

func (b *Bot) sendReaction(chat wtypes.JID, messageID wtypes.MessageID, emoji string) error {
	msg := &waProto.Message{
		ReactionMessage: &waProto.ReactionMessage{
			Key: &waProto.MessageKey{
				RemoteJID: proto.String(chat.String()),
				FromMe:    proto.Bool(false), // Reacting to a message from others
				ID:        proto.String(string(messageID)),
			},
			Text:              proto.String(emoji),
			SenderTimestampMS: proto.Int64(time.Now().UnixMilli()),
		},
	}
	_, err := b.client.SendMessage(context.Background(), chat, msg)
	return err
}

func (b *Bot) sendVoiceNote(chat wtypes.JID, audioBytes []byte) error {
	// Upload audio to WhatsApp
	resp, err := b.client.Upload(context.Background(), audioBytes, whatsmeow.MediaAudio)
	if err != nil {
		return fmt.Errorf("failed to upload audio: %w", err)
	}

	msg := &waProto.Message{
		AudioMessage: &waProto.AudioMessage{
			URL:           proto.String(resp.URL),
			DirectPath:    proto.String(resp.DirectPath),
			MediaKey:      resp.MediaKey,
			Mimetype:      proto.String("audio/ogg; codecs=opus"),
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(audioBytes))),
			Seconds:       proto.Uint32(0),  // Optional: can calculate with ffmpeg if needed
			PTT:           proto.Bool(true), // This makes it a voice note
		},
	}

	_, err = b.client.SendMessage(context.Background(), chat, msg)
	return err
}

func (b *Bot) handleAudioMessage(msg *events.Message) {
	audio := msg.Message.GetAudioMessage()
	if audio == nil {
		return
	}

	data, err := b.client.Download(context.Background(), audio)
	if err != nil {
		fmt.Printf("Error downloading audio: %v\n", err)
		return
	}

	chatID := msg.Info.Chat.String()

	var audioData []byte
	var mimeType string

	// Convert based on provider
	if os.Getenv("STT_PROVIDER") == "LOCAL" {
		log.Debug().Msg("Converting OGG to WAV for local Whisper")
		wavData, err := utils.ConvertOggToWav(data)
		if err != nil {
			fmt.Printf("Error converting audio to wav: %v\n", err)
			audioData = data
			mimeType = "audio/ogg"
		} else {
			audioData = wavData
			mimeType = "audio/wav"
		}
	} else {
		// Convert OGG to MP3 for better Gemini/Groq compatibility
		mp3Data, err := utils.ConvertOggToMp3(data)
		if err != nil {
			fmt.Printf("Error converting audio to mp3: %v\n", err)
			audioData = data
			mimeType = "audio/ogg"
		} else {
			audioData = mp3Data
			mimeType = "audio/mp3"
		}
	}

	// Prepare context for the AI
	prompt := "The user sent a voice note. Please transcribe what they said and respond to them appropriately as Max."

	timeout := b.timeouts.getOptimalTimeout()
	response, tokens, latency, err := ai.MakeAIRequest(prompt, audioData, mimeType, timeout)
	if err != nil {
		log.Error().Err(err).Msg("Error processing audio with AI")
		b.sendAcknowledgment(msg.Info.Chat, "✅ Voice note received, but I'm having trouble listening to it right now.")
		return
	}

	// Save to history
	if err := b.initConversation(chatID); err == nil {
		b.mutex.Lock()
		b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, BotMessage{
			Role:    "user",
			Content: "[Sent a voice note]",
			Time:    msg.Info.Timestamp,
		})
		b.mutex.Unlock()
	}

	// Use the new helper to process the response
	b.processAIResponse(chatID, msg.Info.Chat, msg.Info.ID, response, tokens, latency, true)
}

func (b *Bot) handleImageMessage(msg *events.Message) {
	img := msg.Message.GetImageMessage()
	if img == nil {
		return
	}

	data, err := b.client.Download(context.Background(), img)
	if err != nil {
		fmt.Printf("Error downloading image: %v\n", err)
		return
	}

	chatID := msg.Info.Chat.String()
	userCaption := img.GetCaption()

	// Prepare context for the AI
	prompt := "The user sent an image. "
	if userCaption != "" {
		prompt += fmt.Sprintf("Their caption is: \"%s\". ", userCaption)
	}
	prompt += "Please describe what you see in this image and respond to the user appropriately as Max."

	timeout := b.timeouts.getOptimalTimeout()
	
	// Calculate hash for caching
	h := sha256.New()
	h.Write(data)
	hash := hex.EncodeToString(h.Sum(nil))

	// Check cache
	var description string
	err = b.SqlDB.QueryRow("SELECT description FROM image_cache WHERE image_hash = ?", hash).Scan(&description)
	
	var response string
	var tokens int
	var latency time.Duration

	if err != nil {
		// Cache miss: describe it with AI
		log.Info().Msg("New image, describing with AI.")
		
		response, tokens, latency, err = ai.MakeAIRequest(prompt, data, "image/jpeg", timeout)
		if err != nil {
			log.Error().Err(err).Msg("Error describing image with AI")
			b.sendAcknowledgment(msg.Info.Chat, "✅ Image received, but I'm having trouble seeing it right now.")
			return
		}
		// Save to cache
		_, err = b.SqlDB.Exec("INSERT INTO image_cache (image_hash, description, created_at) VALUES (?, ?, ?)", hash, response, time.Now())
		if err != nil {
			log.Error().Err(err).Msg("Error saving image to cache")
		}
	} else {
		log.Info().Msg("Image found in visual memory cache.")
		response = description
		tokens = 0
		latency = 0
	}

	// Save to history
	if err := b.initConversation(chatID); err == nil {
		b.mutex.Lock()
		b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, BotMessage{
			Role:    "user",
			Content: fmt.Sprintf("[Sent an image] %s", userCaption),
			Time:    msg.Info.Timestamp,
		})
		b.mutex.Unlock()
	}

	// Use the new helper to process the response
	b.processAIResponse(chatID, msg.Info.Chat, msg.Info.ID, response, tokens, latency, false)
}

func (b *Bot) handleDocumentMessage(msg *events.Message) {
	doc := msg.Message.GetDocumentMessage()
	if doc == nil {
		return
	}

	data, err := b.client.Download(context.Background(), doc)
	if err != nil {
		fmt.Printf("Error downloading document: %v\n", err)
		return
	}

	fileName := doc.GetFileName()
	mimeType := doc.GetMimetype()
	chatID := msg.Info.Chat.String()

	fmt.Printf("Received document: %s (%s), size: %d bytes\n", fileName, mimeType, len(data))

	var extractedText string

	// Handle based on file type
	switch {
	case strings.HasSuffix(strings.ToLower(fileName), ".txt") || strings.HasSuffix(strings.ToLower(fileName), ".md") || strings.Contains(mimeType, "text/plain"):
		extractedText = string(data)
	case strings.HasSuffix(strings.ToLower(fileName), ".pdf") || strings.Contains(mimeType, "pdf"):
		extractedText, err = extractTextFromPDF(data)
		if err != nil {
			log.Error().Err(err).Msg("Error extracting text from PDF")
			b.sendAcknowledgment(msg.Info.Chat, fmt.Sprintf("❌ Error reading PDF: %s", fileName))
			return
		}
	case strings.HasSuffix(strings.ToLower(fileName), ".docx") || strings.Contains(mimeType, "officedocument.wordprocessingml.document"):
		extractedText, err = extractTextFromDocx(data)
		if err != nil {
			log.Error().Err(err).Msg("Error extracting text from DOCX")
			b.sendAcknowledgment(msg.Info.Chat, fmt.Sprintf("❌ Error reading Word document: %s", fileName))
			return
		}
	default:
		// For now, acknowledge other files.
		b.sendAcknowledgment(msg.Info.Chat, fmt.Sprintf("✅ Received document: %s. I can currently read .txt, .md, .pdf, and .docx files.", fileName))
		return
	}

	if extractedText == "" {
		b.sendAcknowledgment(msg.Info.Chat, "✅ Document received, but it appears to be empty.")
		return
	}

	// Limit text size to avoid prompt overflow
	if len(extractedText) > 10000 {
		extractedText = extractedText[:10000] + "... [truncated]"
	}

	// Prepare context for the AI
	prompt := fmt.Sprintf("The user sent a document named \"%s\".\n\nCONTENT OF DOCUMENT:\n%s\n\nPlease summarize this document for the user and ask how you can help with it as Max.", fileName, extractedText)

	timeout := b.timeouts.getOptimalTimeout()
	response, tokens, latency, err := ai.MakeAIRequest(prompt, nil, "", timeout)
	if err != nil {
		log.Error().Err(err).Msg("Error processing document with AI")
		b.sendAcknowledgment(msg.Info.Chat, fmt.Sprintf("✅ Document received: %s, but I'm having trouble analyzing it right now.", fileName))
		return
	}

	// Save to history
	if err := b.initConversation(chatID); err == nil {
		b.mutex.Lock()
		b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, BotMessage{
			Role:    "user",
			Content: fmt.Sprintf("[Sent a document: %s]", fileName),
			Time:    msg.Info.Timestamp,
		})
		b.mutex.Unlock()
	}

	// Use the new helper to process the response
	b.processAIResponse(chatID, msg.Info.Chat, msg.Info.ID, response, tokens, latency, false)
}

func (b *Bot) sendAcknowledgment(chat wtypes.JID, text string) error {
	msg := utils.CreateTextMessage(text)
	_, err := b.client.SendMessage(context.Background(), chat, msg)
	return err
}

// sendAcknowledgmentThrottled sends an acknowledgment to a chat but ensures we don't
// repeatedly send the same type of error/ack messages to the same chat within a short
// cooldown window. Returns nil if suppressed or the result of sendAcknowledgment.
func (b *Bot) sendAcknowledgmentThrottled(chat wtypes.JID, text string) error {
	chatKey := chat.String()
	b.mutex.Lock()
	last, ok := b.lastAck[chatKey]
	if !ok || time.Since(last) > b.ackCooldown {
		b.lastAck[chatKey] = time.Now()
		b.mutex.Unlock()
		return b.sendAcknowledgment(chat, text)
	}
	b.mutex.Unlock()
	// Suppressed due to cooldown
	return nil
}

func (b *Bot) cleanupCache() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		b.cache.Clear()
	}
}

func (b *Bot) getCachedResponse(query string) (string, bool) {
	if value, exists := b.cache.Get(query); exists {
		entry := value.(*CacheEntry)
		if time.Since(entry.Timestamp) < 24*time.Hour {
			entry.UseCount++
			return entry.Response, true
		}
	}
	return "", false
}

func (b *Bot) cacheResponse(query, response string) {
	b.cache.Set(query, &CacheEntry{
		Response:  response,
		Timestamp: time.Now(),
		UseCount:  1,
	}, 24*time.Hour)
}

// Commented out unused method
/*
func (tm *TimeoutManager) updateResponseTime(duration time.Duration) {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	if tm.averageResponseTime == 0 {
		tm.averageResponseTime = duration
	} else {
		tm.averageResponseTime = time.Duration(float64(tm.averageResponseTime)*0.7 + float64(duration)*0.3)
	}
}
*/

func (tm *TimeoutManager) getOptimalTimeout() time.Duration {
	tm.mutex.RLock()
	defer tm.mutex.RUnlock()

	initial := getInitialTimeout()
	max := getMaxTimeout()

	if tm.averageResponseTime == 0 {
		return initial
	}

	timeout := tm.averageResponseTime * 2

	if timeout < initial {
		return initial
	}
	if timeout > max {
		return max
	}
	return timeout
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	return err == context.DeadlineExceeded || strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline exceeded")
}

func isOverloadedError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "overloaded")
}

// Commented out unused method
/*
func (tm *TimeoutManager) recordTimeout() {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()
	tm.timeoutCount++
}
*/

func (b *Bot) summarizeConversation(chatID string) {
	b.mutex.Lock()
	conv, exists := b.conversations[chatID]
	if !exists || len(conv.Messages) < MAX_HISTORY {
		b.mutex.Unlock()
		return
	}

	// Create a prompt for the summarization
	var promptBuilder strings.Builder
	promptBuilder.WriteString("Summarize the following conversation:\n\n")
	for _, msg := range conv.Messages {
		promptBuilder.WriteString(fmt.Sprintf("%s: %s\n", msg.Role, msg.Content))
	}
	prompt := promptBuilder.String()
	b.mutex.Unlock()

	// Make a request to the AI to summarize the conversation in a separate goroutine
	go func() {
		summary, _, _, err := ai.MakeAIRequest(prompt, nil, "", DEFAULT_TIMEOUT)
		if err != nil {
			fmt.Printf("Error summarizing conversation: %v\n", err)
			return
		}

		// Update the conversation with the summary
		b.mutex.Lock()
		defer b.mutex.Unlock()
		conv, exists := b.conversations[chatID]
		if !exists {
			return
		}
		conv.Summary = summary
		conv.Messages = conv.Messages[len(conv.Messages)-5:] // Keep the last 5 messages for context

		// Update summary in DB
		if dbErr := b.saveConversationToDB(chatID, conv); dbErr != nil {
			fmt.Printf("Error saving summarized conversation to DB: %v\n", dbErr)
		}

		// Delete older messages from DB
		_, err = b.SqlDB.Exec("DELETE FROM messages WHERE conversation_chat_id = ? AND timestamp NOT IN (SELECT timestamp FROM messages WHERE conversation_chat_id = ? ORDER BY timestamp DESC LIMIT 5)", chatID, chatID)
		if err != nil {
			fmt.Printf("Error deleting old messages from DB: %v\n", err)
		}
	}()
}

func (b *Bot) initDBSchema() error {
	createConversationsTableSQL := `
	CREATE TABLE IF NOT EXISTS conversations (
		chat_id TEXT PRIMARY KEY,
		summary TEXT,
		last_active DATETIME,
		last_message_timestamp DATETIME DEFAULT '1970-01-01 00:00:00+00:00'
	);
	`

	createMessagesTableSQL := `
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_chat_id TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		timestamp DATETIME NOT NULL,
		FOREIGN KEY (conversation_chat_id) REFERENCES conversations(chat_id) ON DELETE CASCADE
	);
	`

	createUserPersonalitiesTableSQL := `
	CREATE TABLE IF NOT EXISTS user_personalities (
		user_id TEXT PRIMARY KEY,
		profile TEXT NOT NULL,
		updated_at DATETIME NOT NULL
	);
	`

	createScheduledTasksTableSQL := `
	CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id TEXT NOT NULL,
		target_jid TEXT NOT NULL,
		cron_expr TEXT NOT NULL,
		instruction TEXT NOT NULL,
		is_dynamic BOOLEAN DEFAULT 0,
		created_at DATETIME NOT NULL
	);
	`

	createImageCacheTableSQL := `
	CREATE TABLE IF NOT EXISTS image_cache (
		image_hash TEXT PRIMARY KEY,
		description TEXT NOT NULL,
		created_at DATETIME NOT NULL
	);
	`

	_, err := b.SqlDB.Exec(createConversationsTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create conversations table: %w", err)
	}

	_, err = b.SqlDB.Exec(createMessagesTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create messages table: %w", err)
	}

	_, err = b.SqlDB.Exec(createUserPersonalitiesTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create user_personalities table: %w", err)
	}

	_, err = b.SqlDB.Exec(createScheduledTasksTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create scheduled_tasks table: %w", err)
	}

	_, err = b.SqlDB.Exec(createImageCacheTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create image_cache table: %w", err)
	}

	return nil
}

// GetID returns the bot's ID
func (b *Bot) GetID() string {
	return b.botID
}

func (b *Bot) loadConversationsFromDB() error {
	// Load user personalities first
	pRows, err := b.SqlDB.Query("SELECT user_id, profile FROM user_personalities")
	if err == nil {
		defer pRows.Close()
		for pRows.Next() {
			var uID, profile string
			if err := pRows.Scan(&uID, &profile); err == nil {
				b.vectorStore.UserPersonality[uID] = profile
			}
		}
	}

	rows, err := b.SqlDB.Query("SELECT chat_id, summary, last_active, last_message_timestamp FROM conversations")
	if err != nil {
		return fmt.Errorf("failed to query conversations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var chatID string
		var summary sql.NullString
		var lastActive time.Time
		var lastMessageTimestamp time.Time
		if err := rows.Scan(&chatID, &summary, &lastActive, &lastMessageTimestamp); err != nil {
			return fmt.Errorf("failed to scan conversation: %w", err)
		}

		conv := &Conversation{
			LastActive:           lastActive,
			Summary:              summary.String,
			Messages:             make([]BotMessage, 0), // Messages will be loaded separately
			LastMessageTimestamp: lastMessageTimestamp,
		}
		b.conversations[chatID] = conv

		// Load messages for this conversation
		if err := func() error {
			msgRows, err := b.SqlDB.Query("SELECT role, content, timestamp FROM messages WHERE conversation_chat_id = ? ORDER BY timestamp ASC", chatID)
			if err != nil {
				return fmt.Errorf("failed to query messages for chat %s: %w", chatID, err)
			}
			defer msgRows.Close()

			for msgRows.Next() {
				var role, content string
				var msgTimestamp time.Time
				if err := msgRows.Scan(&role, &content, &msgTimestamp); err != nil {
					return fmt.Errorf("failed to scan message for chat %s: %w", chatID, err)
				}
				conv.Messages = append(conv.Messages, BotMessage{
					Role:    role,
					Content: content,
					Time:    msgTimestamp,
				})
			}
			return msgRows.Err()
		}(); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating conversation rows: %w", err)
	}

	return nil
}

func (b *Bot) saveConversationToDB(chatID string, conv *Conversation) error {
	_, err := b.SqlDB.Exec(
		"INSERT INTO conversations (chat_id, summary, last_active, last_message_timestamp) VALUES (?, ?, ?, ?) ON CONFLICT(chat_id) DO UPDATE SET summary = ?, last_active = ?, last_message_timestamp = ?",
		chatID, conv.Summary, conv.LastActive, conv.LastMessageTimestamp, conv.Summary, conv.LastActive, conv.LastMessageTimestamp,
	)
	if err != nil {
		return fmt.Errorf("failed to save conversation %s to DB: %w", chatID, err)
	}
	return nil
}

func (b *Bot) saveMessageToDB(chatID string, msg BotMessage) error {
	_, err := b.SqlDB.Exec(
		"INSERT INTO messages (conversation_chat_id, role, content, timestamp) VALUES (?, ?, ?, ?)",
		chatID, msg.Role, msg.Content, msg.Time,
	)
	if err != nil {
		return fmt.Errorf("failed to save message for chat %s to DB: %w", chatID, err)
	}
	return nil
}

// processAIResponse handles the final AI response: reaction tags, voice tags, history saving, and message sending.
// This refactored method eliminates duplication across text, audio, image, and document handlers.
func (b *Bot) processAIResponse(chatID string, chat wtypes.JID, msgID string, response string, tokens int, latency time.Duration, isVoiceRequested bool) {
	// 1. Double-check if the bot has been muted since the request started
	b.mutex.RLock()
	mutedUntil, isMuted := b.mutedUntil[chatID]
	b.mutex.RUnlock()
	if isMuted && time.Now().Before(mutedUntil) {
		fmt.Printf("Auto-pilot was muted during AI processing for chat %s. Cancelling response.\n", chatID)
		return
	}

	utils.RecordTimeout(true)
	utils.RecordLMStudioMetrics(latency, tokens)

	// 1. Process and STRIP all [REACT] tags first
	// This ensures they are removed from BOTH the history and the final message.
	for {
		responseLower := strings.ToLower(response)
		reactMarker := "[react"
		idx := strings.Index(responseLower, reactMarker)
		if idx == -1 {
			break
		}

		endIdx := strings.Index(response[idx:], "]")
		if endIdx == -1 {
			response = response[:idx] + response[idx+len(reactMarker):]
			continue
		}

		fullTag := response[idx : idx+endIdx+1]
		tagContent := fullTag[len(reactMarker) : len(fullTag)-1]
		emoji := strings.TrimSpace(strings.TrimLeft(tagContent, ": "))

		if emoji != "" {
			go b.sendReaction(chat, wtypes.MessageID(msgID), emoji)
		}

		response = response[:idx] + response[idx+endIdx+1:]
		response = strings.TrimSpace(response)
	}

	// 2. Process [SEARCH] and [VOICE] tags
	if strings.Contains(response, "[SEARCH:") {
		start := strings.Index(response, "[SEARCH:")
		end := strings.Index(response[start:], "]")
		if end != -1 {
			fullTag := response[start : start+end+1]
			response = strings.Replace(response, fullTag, "", -1)
			response = strings.TrimSpace(response)
		}
	}

	isVoice := isVoiceRequested || strings.Contains(response, "[VOICE]")
	if strings.Contains(response, "[VOICE]") {
		response = strings.ReplaceAll(response, "[VOICE]", "")
		response = strings.TrimSpace(response)
	}

	log.Debug().Msgf("AI Response (Clean): %s", response)

	// 3. Save CLEAN response to memory and database
	b.mutex.Lock()
	assistantMessage := BotMessage{
		Role:    "assistant",
		Content: response,
		Time:    time.Now(),
	}
	if conv, exists := b.conversations[chatID]; exists {
		conv.Messages = append(conv.Messages, assistantMessage)
		go b.summarizeConversation(chatID)
	}
	b.mutex.Unlock()

	// Save assistant response to DB
	if err := b.saveMessageToDB(chatID, assistantMessage); err != nil {
		fmt.Printf("Error saving assistant message to DB: %v\n", err)
	}

	b.mutex.RLock()
	conv, _ := b.conversations[chatID]
	if conv != nil {
		if err := b.saveConversationToDB(chatID, conv); err != nil {
			log.Error().Err(err).Msg("Error saving conversation to DB")
		}
	}
	b.mutex.RUnlock()

	// Send the response (either Voice or Text)
	if isVoice {
		go func() {
			oggBytes, err := utils.TextToVoice(response)
			if err != nil {
				fmt.Printf("Error converting text to voice: %v\n", err)
				// Fallback to text
				replyMsg := utils.CreateTextMessage(response)
				b.client.SendMessage(context.Background(), chat, replyMsg)
				return
			}
			if err := b.sendVoiceNote(chat, oggBytes); err != nil {
				fmt.Printf("Error sending voice note: %v\n", err)
				// Fallback to text
				replyMsg := utils.CreateTextMessage(response)
				b.client.SendMessage(context.Background(), chat, replyMsg)
			}
		}()
	} else {
		replyMsg := utils.CreateTextMessage(response)
		if _, err := b.client.SendMessage(context.Background(), chat, replyMsg); err != nil {
			fmt.Printf("Error sending message: %v\n", err)
			return
		}
	}

	go func() {
		if err := b.client.MarkRead(context.Background(), []wtypes.MessageID{msgID}, time.Now(), chat, chat); err != nil {
			fmt.Printf("Error marking message as read: %v\n", err)
		}
	}()
}

func extractTextFromPDF(data []byte) (string, error) {
	pdfReader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	for i := 1; i <= pdfReader.NumPage(); i++ {
		text, err := pdfReader.Page(i).GetPlainText(nil)
		if err != nil {
			continue
		}
		buf.WriteString(text)
	}
	return buf.String(), nil
}

func extractTextFromDocx(data []byte) (string, error) {
	docxReader, err := docx.ReadDocxFromMemory(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	defer docxReader.Close()
	return docxReader.Editable().GetContent(), nil
}

// handleScheduleCommand parses and schedules a task.
// Format: schedule "cron_expression" message
// Example: schedule "0 8 * * *" Remind me to take out the trash
// handleScheduleCommand parses and schedules a task using AI.
func (b *Bot) handleScheduleCommand(chat wtypes.JID, text string) {
	// 1. Ask Gemini to parse the request into a JSON structure
	parsePrompt := fmt.Sprintf(`### INSTRUCTION:
Parse the user's scheduling request into a JSON object with these fields:
- "cron_expression": A valid 5-part cron expression (e.g., "0 8 * * *").
- "target": The name or JID of the person to receive the message.
- "instruction": A clear instruction for me (Max's AI) to follow when the task runs.
- "is_dynamic": Boolean. Set to true if I need to GENERATE fresh content (like a poem, news, or weather) when the task runs.

### REFERENCE DATA:
- Wilma's JID: 97375716663491@s.whatsapp.net
- Stephanie's JID: 76420520931421@s.whatsapp.net
- My JID (the requester): %s

### USER REQUEST:
"%s"

### OUTPUT FORMAT:
Respond ONLY with the JSON object. Example:
{"cron_expression": "0 8 * * *", "target": "97375716663491@s.whatsapp.net", "instruction": "Write a romantic poem for Wilma.", "is_dynamic": true}`, chat.String(), text)

	response, _, _, err := ai.MakeAIRequest(parsePrompt, nil, "", DEFAULT_TIMEOUT)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Error parsing schedule request with AI.")
		return
	}

	// Simple cleanup of JSON response
	response = strings.TrimSpace(response)
	if strings.Contains(response, "```json") {
		response = strings.Split(strings.Split(response, "```json")[1], "```")[0]
	} else if strings.Contains(response, "```") {
		response = strings.Split(strings.Split(response, "```")[1], "```")[0]
	}
	response = strings.TrimSpace(response)

	var taskData struct {
		CronExpression string `json:"cron_expression"`
		Target         string `json:"target"`
		Instruction    string `json:"instruction"`
		IsDynamic      bool   `json:"is_dynamic"`
	}

	if err := json.Unmarshal([]byte(response), &taskData); err != nil {
		b.sendAcknowledgment(chat, "❌ AI generated an invalid schedule format: "+response)
		return
	}

	targetJID, err := wtypes.ParseJID(taskData.Target)
	if err != nil {
		targetJID = chat // Fallback to sender
	}

	// 2. Schedule the task
	_, err = b.cron.AddFunc(taskData.CronExpression, func() {
		b.executeScheduledTask(targetJID, taskData.Instruction, taskData.IsDynamic)
	})

	if err != nil {
		b.sendAcknowledgment(chat, fmt.Sprintf("❌ Error scheduling task: %v", err))
		return
	}

	// 3. Save to database
	_, err = b.SqlDB.Exec("INSERT INTO scheduled_tasks (chat_id, target_jid, cron_expr, instruction, is_dynamic, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		chat.String(), targetJID.String(), taskData.CronExpression, taskData.Instruction, taskData.IsDynamic, time.Now())
	if err != nil {
		log.Error().Err(err).Msg("Error saving scheduled task to DB")
	}

	confirmMsg := fmt.Sprintf("✅ *Task Scheduled!*\n- *Target:* %s\n- *Frequency:* %s\n- *Action:* %s", taskData.Target, taskData.CronExpression, taskData.Instruction)
	b.sendAcknowledgment(chat, confirmMsg)
}

// executeScheduledTask performs the actual work (AI generation or simple message)
func (b *Bot) executeScheduledTask(target wtypes.JID, instruction string, isDynamic bool) {
	var finalContent string

	if isDynamic {
		// Use AI to generate content (poems, news, weather, etc.)
		prompt := fmt.Sprintf("You are performing a scheduled task as Max. Your instruction is: \"%s\". Please generate the appropriate content now.", instruction)
		
		// If it looks like a news/weather request, hint at searching
		if strings.Contains(strings.ToLower(instruction), "news") || strings.Contains(strings.ToLower(instruction), "price") || strings.Contains(strings.ToLower(instruction), "weather") {
			prompt += " You MUST use your [SEARCH:...] tool if you need current information like weather, news, or prices. Once you have the results, provide the final summary."
		}

		response, _, _, err := ai.MakeAIRequest(prompt, nil, "", DEFAULT_TIMEOUT)
		if err != nil {
			log.Error().Err(err).Msg("Error generating AI content for scheduled task")
			finalContent = "⚠️ (Auto-Task Error): " + instruction
		} else {
			// PERFORM SEARCH LOOP IF AI REQUESTED IT
			for i := 0; i < 2; i++ {
				if !strings.Contains(response, "[SEARCH:") {
					break
				}
				startIdx := strings.Index(response, "[SEARCH:")
				endIdx := strings.Index(response[startIdx:], "]")
				if endIdx == -1 {
					break
				}
				searchQuery := response[startIdx+8 : startIdx+endIdx]
				log.Info().Msgf("Scheduled task AI requested web search: %s", searchQuery)

				searchResults, _ := utils.SearchWeb(searchQuery)
				prompt += fmt.Sprintf("\n\n### SEARCH RESULTS FOR \"%s\":\n%s\n\n(Based on these results, please provide your final response as Max.)", searchQuery, searchResults)
				response, _, _, err = ai.MakeAIRequest(prompt, nil, "", DEFAULT_TIMEOUT)
				if err != nil {
					break
				}
			}
			finalContent = CleanResponse(response)
		}
	} else {
		finalContent = instruction
	}

	log.Info().Msgf("Executing scheduled task for %s: %s", target.String(), finalContent)
	b.sendAcknowledgment(target, "⏰ *Scheduled Task:*\n\n"+finalContent)
}

func (b *Bot) loadScheduledTasks() error {
	rows, err := b.SqlDB.Query("SELECT target_jid, cron_expr, instruction, is_dynamic FROM scheduled_tasks")
	if err != nil {
		return fmt.Errorf("failed to query scheduled tasks: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var targetStr, cronExpr, instruction string
		var isDynamic bool
		if err := rows.Scan(&targetStr, &cronExpr, &instruction, &isDynamic); err != nil {
			log.Error().Err(err).Msg("Error scanning scheduled task row")
			continue
		}

		targetJID, err := wtypes.ParseJID(targetStr)
		if err != nil {
			log.Error().Err(err).Msgf("Error parsing target JID from DB: %s", targetStr)
			continue
		}

		// Re-schedule the task
		// Local variables to avoid closure capture issues
		tJID := targetJID
		instr := instruction
		dyn := isDynamic

		_, err = b.cron.AddFunc(cronExpr, func() {
			b.executeScheduledTask(tJID, instr, dyn)
		})
		if err != nil {
			log.Error().Err(err).Msgf("Error re-scheduling task from DB: %s", cronExpr)
		} else {
			count++
		}
	}
	log.Info().Msgf("Restored %d scheduled tasks from database", count)
	return nil
}

func (b *Bot) handleUnscheduleCommand(chat wtypes.JID, text string) {
	parts := strings.Fields(text)
	// format: remove schedule [id] or unschedule [id]
	
	if len(parts) < 3 && strings.HasPrefix(strings.ToLower(text), "remove schedule") {
		// List tasks if no ID
		b.listScheduledTasks(chat)
		return
	}
	if len(parts) < 2 && strings.HasPrefix(strings.ToLower(text), "unschedule") {
		b.listScheduledTasks(chat)
		return
	}

	idIdx := 2
	if strings.HasPrefix(strings.ToLower(text), "unschedule") {
		idIdx = 1
	}

	taskID := parts[idIdx]
	res, err := b.SqlDB.Exec("DELETE FROM scheduled_tasks WHERE id = ?", taskID)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Error removing task: "+err.Error())
		return
	}

	rows, _ := res.RowsAffected()
	if rows == 0 {
		b.sendAcknowledgment(chat, "❌ No task found with ID: "+taskID)
	} else {
		b.sendAcknowledgment(chat, "✅ Task "+taskID+" removed. (Note: It will stop running immediately, but internal cron entry remains until next restart. Use '!resume' to fully refresh if critical.)")
	}
}

func (b *Bot) listScheduledTasks(chat wtypes.JID) {
	rows, err := b.SqlDB.Query("SELECT id, cron_expr, instruction, target_jid FROM scheduled_tasks")
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Error listing tasks: "+err.Error())
		return
	}
	defer rows.Close()

	var builder strings.Builder
	builder.WriteString("📋 *Active Scheduled Tasks:*\n\n")
	found := false
	for rows.Next() {
		var id int
		var cron, instr, target string
		if err := rows.Scan(&id, &cron, &instr, &target); err == nil {
			found = true
			builder.WriteString(fmt.Sprintf("[%d] %s -> %s (%s)\n", id, cron, target, instr))
		}
	}

	if !found {
		b.sendAcknowledgment(chat, "No active scheduled tasks found.")
	} else {
		builder.WriteString("\nTo remove one, use: `remove schedule [ID]`")
		b.sendAcknowledgment(chat, builder.String())
	}
}

// ResumeAutopilot clears all mutes and refreshes scheduled tasks.
func (b *Bot) ResumeAutopilot() {
	b.mutex.Lock()
	b.mutedUntil = make(map[string]time.Time)
	b.mutex.Unlock()

	b.cron.Stop()
	b.cron = cron.New()
	b.cron.Start()
	b.loadScheduledTasks()
}

func (b *Bot) handleStatusCommand(chat wtypes.JID) {
	m := utils.GetMetrics()
	mem := utils.GetMemoryStats()
	
	status := fmt.Sprintf("🤖 *Maximus Status*\n\n" +
		"📈 *Metrics:*\n" +
		"- Total Requests: %d\n" +
		"- Active Sessions: %d\n" +
		"- Avg Latency: %v\n\n" +
		"🧠 *Memory:*\n" +
		"- Heap Alloc: %s\n" +
		"- Heap In-Use: %s\n" +
		"- Goroutines: %d",
		m.TotalRequests, m.ActiveSessions, time.Duration(m.AverageLatency),
		formatBytes(mem.HeapAlloc), formatBytes(mem.HeapInUse), m.GoroutineCount)
		
	b.sendAcknowledgment(chat, status)
}

func (b *Bot) handleLogsCommand(chat wtypes.JID) {
	logPath := filepath.Join("logs", "whatsapp-bot.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Could not read logs.")
		return
	}
	
	lines := strings.Split(string(data), "\n")
	start := len(lines) - 15
	if start < 0 { start = 0 }
	
	snippet := strings.Join(lines[start:], "\n")
	b.sendAcknowledgment(chat, "📋 *Recent Logs:*\n\n" + snippet)
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func (b *Bot) handleFactCommand(chat wtypes.JID, fact string) {
	f, err := os.OpenFile("truth.md", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Error saving fact.")
		return
	}
	defer f.Close()
	
	if _, err := f.WriteString("- " + fact + "\n"); err != nil {
		b.sendAcknowledgment(chat, "❌ Error writing fact.")
		return
	}
	b.sendAcknowledgment(chat, "✅ Fact added to truth journal: " + fact)
}

func (b *Bot) handleCorrectCommand(chat wtypes.JID, correction string) {
	f, err := os.OpenFile("truth.md", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Error saving correction.")
		return
	}
	defer f.Close()
	
	if _, err := f.WriteString("- " + correction + "\n"); err != nil {
		b.sendAcknowledgment(chat, "❌ Error writing correction.")
		return
	}
	
	b.sendAcknowledgment(chat, "✅ Correction noted. My apologies for the misinformation—let me clarify: " + correction)
}

// triggerHITLAlert notifies the human assistant that the bot is stuck and pauses autopilot for that chat.
func (b *Bot) triggerHITLAlert(chatID string, lastErr error) {
	alertMsg := fmt.Sprintf("🚨 *MAXIMUS CRITICAL ALERT*\n\n" +
		"Bot is stuck in chat: %s\n" +
		"Consecutive Failures: 3\n" +
		"Last Error: %v\n\n" +
		"Auto-pilot has been paused for this chat. Please intervene or use '!resume' when ready.",
		chatID, lastErr)
	
	// Send to Max
	targetJID, err := wtypes.ParseJID(b.humanAssistantJID)
	if err == nil {
		b.sendAcknowledgment(targetJID, alertMsg)
	}
	
	// Pause bot for this chat until manual resume
	b.mutex.Lock()
	b.mutedUntil[chatID] = time.Now().Add(24 * time.Hour)
	b.mutex.Unlock()
	
	log.Warn().Msgf("HITL Alert triggered for chat %s. Bot paused.", chatID)
}


