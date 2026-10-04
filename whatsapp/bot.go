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
	"regexp"
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
	Sender  string // actual sender name for this message (for correct history attribution)
}

type Conversation struct {
	Messages             []BotMessage
	LastActive           time.Time
	Summary              string
	LastMessageTimestamp time.Time
	UserName             string
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
	client              *whatsmeow.Client
	db                  *sqlstore.Container
	SqlDB               *sql.DB
	conversations       map[string]*Conversation
	cache               *cache.Cache
	timeouts            *TimeoutManager
	messageQueue        *queue.Queue
	mutex               sync.RWMutex
	qrMux               sync.Mutex
	rateLimiter         *RateLimiter
	vectorStore         *VectorStore // Shared RAG and personality store
	accountManager      *AccountManager
	botID               string
	humanAssistantJID   string // JID of the human assistant
	lastAck             map[string]time.Time
	ackCooldown         time.Duration
	ignoredChatJIDs     map[string]bool      // JIDs of chats to ignore
	mutedUntil          map[string]time.Time // Chats where Max took the wheel
	consecutiveFailures map[string]int       // Track consecutive AI failures per chat
	groupEngage         bool                 // respond in groups at all (default false)
	groupAllowlist      map[string]bool      // group JIDs allowed to engage (empty + groupEngage = all groups)
	lastOutageAlert     time.Time            // rate-limit for global outage alerts
	contactMu           sync.RWMutex         // guards contactIdx
	contactIdx          *contactIndex        // cached contact lookup index
	tools               *ToolRegistry        // model-callable capabilities
	cron                *cron.Cron           // Scheduler for automated tasks
	cronJobIDs          map[int]cron.EntryID // Map of DB task IDs to cron entry IDs
}

func NewBot(client *whatsmeow.Client, db *sqlstore.Container, am *AccountManager, id string) (*Bot, error) {
	bot := &Bot{
		client:              client,
		db:                  db,
		SqlDB:               am.SqlDB,
		conversations:       make(map[string]*Conversation),
		cache:               cache.NewCache(1000),
		timeouts:            &TimeoutManager{},
		messageQueue:        queue.NewQueue(10, 5, 5*time.Second),
		rateLimiter:         NewRateLimiter(0.5, 5), // Allow 1 request every 2 seconds, with a burst of 5
		accountManager:      am,
		botID:               id,
		vectorStore:         am.vectorStore,
		lastAck:             make(map[string]time.Time),
		ackCooldown:         60 * time.Second,
		ignoredChatJIDs:     make(map[string]bool),
		mutedUntil:          make(map[string]time.Time),
		consecutiveFailures: make(map[string]int),
		groupAllowlist:      make(map[string]bool),
		cron:                cron.New(),
		cronJobIDs:          make(map[int]cron.EntryID),
	}
	bot.cron.Start()

	// Schedule the daily summary (default: 18:00 UTC). Override with DAILY_SUMMARY_CRON env var.
	cronExpr := os.Getenv("DAILY_SUMMARY_CRON")
	if cronExpr == "" {
		cronExpr = "0 18 * * *"
	}
	if _, err := bot.cron.AddFunc(cronExpr, bot.dailySummary); err != nil {
		log.Error().Err(err).Msgf("Failed to schedule daily summary with cron expression %q", cronExpr)
	}

	// Parse IGNORED_CHAT_JIDS environment variable
	if ignoredJIDsStr := os.Getenv("IGNORED_CHAT_JIDS"); ignoredJIDsStr != "" {
		for _, jid := range strings.Split(ignoredJIDsStr, ",") {
			bot.ignoredChatJIDs[strings.TrimSpace(jid)] = true
		}
	}

	// Group engagement policy. Groups are OFF by default (a shared group chat
	// conflates multiple people into one conversation). Set GROUP_ENGAGE=true to
	// allow responses, and GROUP_ALLOWLIST=<jid,jid> to restrict which groups.
	bot.groupEngage = strings.EqualFold(os.Getenv("GROUP_ENGAGE"), "true")
	if list := os.Getenv("GROUP_ALLOWLIST"); list != "" {
		for _, jid := range strings.Split(list, ",") {
			if trimmed := strings.TrimSpace(jid); trimmed != "" {
				bot.groupAllowlist[trimmed] = true
			}
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

	// Migrate the previously hardcoded identities into the contacts table.
	// Must run AFTER initDBSchema, which creates the contacts table.
	if err := bot.seedContactsFromEnv(); err != nil {
		log.Warn().Err(err).Msg("Failed to seed contacts from environment")
	}

	// Register model-callable tools (permission-tiered).
	bot.registerTools()

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

	// Enable reminder tag handling (1-minute check cron + AI tag interception)
	bot.enableReminderTagHandling()

	// Enable task/note tag handling (load persisted items, no cron needed)
	bot.enableTaskNoteTagHandling()

	// Enable hourly implicit preference learning
	bot.enablePreferenceLearning()

	// Hot-reload persona docs when edited on disk
	bot.enablePersonaHotReload()

	// Structured end-of-thread recaps
	bot.enableRecapScheduler()

	return bot, nil
}

const (
	GEMINI_API_URL  = "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key="
	MAX_TOKENS      = 4096
	MAX_HISTORY     = 10
	DEFAULT_TIMEOUT = 300 * time.Second
	MIN_TIMEOUT     = 10 * time.Second
	MAX_RETRIES     = 2
	// Default threshold to ignore old messages on first connection (24 hours)
	DEFAULT_IGNORE_MESSAGES_OLDER_THAN = 24 * time.Hour
)

func getInitialTimeout() time.Duration {
	if timeoutStr := os.Getenv("AI_TIMEOUT"); timeoutStr != "" {
		if t, err := strconv.Atoi(timeoutStr); err == nil && t > 0 {
			return time.Duration(t) * time.Second
		}
	}
	return 300 * time.Second
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
	log.Debug().Msgf("Event received: %T", evt)
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
					// "!fact about <name> <fact>" targets a specific person;
					// plain "!fact <fact>" appends to Max's own truth journal.
					if strings.HasPrefix(strings.ToLower(fact), "about ") {
						b.handleFactAboutCommand(v.Info.Chat, fact)
					} else if fact != "" {
						b.handleFactCommand(v.Info.Chat, fact)
					}
				}
				return
			case lowerMsg == "!facts" || strings.HasPrefix(lowerMsg, "!facts "):
				b.listFacts(v.Info.Chat, strings.TrimSpace(strings.TrimPrefix(msgText, "!facts")))
				return
			case lowerMsg == "!contact" || strings.HasPrefix(lowerMsg, "!contact "):
				if isMax {
					b.handleContactCommand(v.Info.Chat, strings.TrimSpace(strings.TrimPrefix(msgText, "!contact")))
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
			case strings.HasPrefix(lowerMsg, "!reminders") || lowerMsg == "!r":
				b.remindCommand(v.Info.Chat, msgText)
				return
			case strings.HasPrefix(lowerMsg, "!remind "):
				// !remind <specification> — create a reminder
				spec := strings.TrimSpace(strings.TrimPrefix(msgText, "!remind "))
				if spec != "" {
					b.handleReminderCommand(v.Info.Chat, spec)
				} else {
					b.sendAcknowledgment(v.Info.Chat, "Usage: !remind [in 20 minutes|at 3pm|on 2026-09-25 at 3pm] to <action> [directly]")
				}
				return
			case lowerMsg == "!task" || strings.HasPrefix(lowerMsg, "!task "):
				b.handleTaskCommand(v.Info.Chat, strings.TrimSpace(strings.TrimPrefix(msgText, "!task")))
				return
			case lowerMsg == "!todo" || lowerMsg == "!todos" || lowerMsg == "!mytasks":
				b.listTasks(v.Info.Chat)
				return
			case strings.HasPrefix(lowerMsg, "!done "):
				b.markTaskDone(v.Info.Chat, strings.TrimSpace(strings.TrimPrefix(msgText, "!done ")))
				return
			case lowerMsg == "!note" || strings.HasPrefix(lowerMsg, "!note "):
				b.handleNoteCommand(v.Info.Chat, strings.TrimSpace(strings.TrimPrefix(msgText, "!note")))
				return
			case lowerMsg == "!notes" || strings.HasPrefix(lowerMsg, "!notes "):
				b.listNotes(v.Info.Chat, strings.TrimSpace(strings.TrimPrefix(msgText, "!notes")))
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
					msg := BotMessage{Role: "assistant", Content: userMsg, Time: v.Info.Timestamp, Sender: "maximus"}
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

		// Main filter logic for incoming messages from others.
		// Poll updates are always ignored. Groups are ignored unless the group
		// engagement policy explicitly allows this chat (default: off).
		if v.Message.GetPollUpdateMessage() != nil {
			return
		}
		if v.Info.IsGroup && !b.shouldEngageGroup(v.Info.Chat.String()) {
			log.Debug().Msgf("Ignoring group message from %s (group engagement disabled).", v.Info.Chat)
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
					msg := BotMessage{Role: "user", Content: userMsg, Time: v.Info.Timestamp, Sender: b.resolveSenderName(v.Info.Sender.String())}
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
	var userName string
	row := b.SqlDB.QueryRow("SELECT summary, last_active, last_message_timestamp, user_name FROM conversations WHERE chat_id = ?", chatID)
	if err := row.Scan(&summary, &lastActive, &lastMessageTimestamp, &userName); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // Not found
		}
		return nil, fmt.Errorf("failed to scan conversation from DB: %w", err)
	}

	conv := &Conversation{
		UserName:             userName,
		LastActive:           lastActive,
		Summary:              summary.String,
		Messages:             make([]BotMessage, 0),
		LastMessageTimestamp: lastMessageTimestamp,
	}

	msgRows, err := b.SqlDB.Query("SELECT role, content, sender, timestamp FROM messages WHERE conversation_chat_id = ? ORDER BY timestamp ASC", chatID)
	if err != nil {
		return nil, fmt.Errorf("failed to query messages for chat %s: %w", chatID, err)
	}
	defer msgRows.Close()

	for msgRows.Next() {
		var role, content, sender string
		var msgTimestamp time.Time
		if err := msgRows.Scan(&role, &content, &sender, &msgTimestamp); err != nil {
			return nil, fmt.Errorf("failed to scan message for chat %s: %w", chatID, err)
		}
		conv.Messages = append(conv.Messages, BotMessage{
			Role:    role,
			Content: content,
			Sender:  sender,
			Time:    msgTimestamp,
		})
	}
	if err := msgRows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating message rows for chat %s: %w", chatID, err)
	}

	return conv, nil
}

// resolveSenderName returns the display name for a sender JID.
// Identity comes from the contacts table; the owner is recognised even if the
// contacts row is absent.
func (b *Bot) resolveSenderName(senderJID string) string {
	if b.humanAssistantJID != "" && baseNumber(senderJID) == baseNumber(b.humanAssistantJID) {
		return "Max"
	}
	if c, ok := b.lookupContactByJID(senderJID); ok && c.Name != "" {
		return c.Name
	}
	return "User"
}

// CleanResponse strips all [REACT], [SEARCH], and [VOICE] tags from a string.
func CleanResponse(text string) string {
	// 1. Strip [REACT:...] tags
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

	// 2. Strip ALL variants of [SEARCH:...] tags (case-insensitive)
	for {
		lower := strings.ToLower(text)
		searchMarker := "[search"
		idx := strings.Index(lower, searchMarker)
		if idx == -1 {
			break
		}
		endIdx := strings.Index(text[idx:], "]")
		if endIdx == -1 {
			// Malformed tag, just strip the marker
			text = text[:idx] + text[idx+len(searchMarker):]
			continue
		}
		text = text[:idx] + text[idx+endIdx+1:]
	}

	// 3. Strip [VOICE] tag
	text = strings.ReplaceAll(text, "[VOICE]", "")

	// 4. Strip any residual action tags ([TASK:...], [NOTE:...], [REMINDER:...]) that
	//    weren't consumed by their interceptors, so raw tags never reach the user.
	for _, marker := range []string{"[task", "[note", "[reminder", "[fact"} {
		for {
			lower := strings.ToLower(text)
			idx := strings.Index(lower, marker)
			if idx == -1 {
				break
			}
			endIdx := strings.Index(text[idx:], "]")
			if endIdx == -1 {
				text = text[:idx] + text[idx+len(marker):]
				continue
			}
			text = text[:idx] + text[idx+endIdx+1:]
		}
	}

	return strings.TrimSpace(text)
}

func (b *Bot) handleTextMessage(msg *events.Message, chatID string) {
	start := time.Now()
	utils.IncrementRequests()

	// Get user ID from the chat ID for personalization.
	// In a group, the chat JID identifies the GROUP, not the person — using it
	// would conflate every participant into one identity. Resolve the individual
	// participant instead so names and memory stay per-person.
	userID := chatID
	if msg.Info.IsGroup {
		userID = msg.Info.Sender.String()
		if userID == "" && !msg.Info.SenderAlt.IsEmpty() {
			userID = msg.Info.SenderAlt.String()
		}
	}

	var userName string
	var isMax bool
	var isFamily bool

	// Identify the user for the AI
	userName = "User"
	isMax = false
	isFamily = false

	// Identity + tier resolution. The contacts table is the single source of
	// truth; isMax and isFamily are derived from the resolved tier rather than
	// from hardcoded address fragments.
	if msg.Info.IsGroup && msg.Info.PushName != "" {
		userName = msg.Info.PushName
	}

	tier := b.tierForJID(userID)
	switch {
	case tier == TierOwner:
		userName = "Max"
		isMax = true
		isFamily = true
	case tier == TierFamily:
		// Prefer the canonical contact name; fall back to push name in groups.
		if c, ok := b.lookupContactByJID(userID); ok && c.Name != "" {
			userName = c.Name
		}
		isFamily = true
	case tier == TierKnown:
		if c, ok := b.lookupContactByJID(userID); ok && c.Name != "" {
			userName = c.Name
		}
	default:
		// Unknown: use whatever name WhatsApp gave us, if anything.
		if msg.Info.PushName != "" {
			userName = msg.Info.PushName
		}
	}

	// Persist the resolved user name on the conversation (for daily summary, etc.).
	if conv, exists := b.conversations[chatID]; exists && conv.UserName == "" {
		conv.UserName = userName
		b.saveConversationToDB(chatID, conv)
	}

	// If the message is from the human assistant, log it but continue processing to allow a response
	if isMax {
		log.Debug().Msgf("Message from human assistant (%s). Processing and responding.", msg.Info.Sender.String())
	}

	defer func() {
		utils.RecordLatency(time.Since(start))
		utils.RecordTimeout(true)
	}()

	userMsg := msg.Message.GetConversation()
	if userMsg == "" {
		return
	}

	// Escalation: if a non-Max user asks to talk to Max, alert Max and reassure the user.
	if !isMax && b.shouldEscalateToMax(userMsg) {
		b.escalateToMax(chatID, msg.Info.Chat, userName, userMsg)
		return
	}

	// Add user message to conversation history BEFORE building prompt
	b.mutex.Lock()
	userMessage := BotMessage{
		Role:    "user",
		Content: userMsg,
		Time:    time.Now(),
		Sender:  userName,
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

	// Check if the user already has a personality profile
	b.mutex.RLock()
	conv, exists := b.conversations[chatID]
	b.mutex.RUnlock()
	_, hasPersonality := b.vectorStore.UserPersonality[userID]

	// Note: Auto personality test offer for first-time texters is disabled.
	// Users start with normal conversation unless they explicitly request a personality test.

	// Send a brief intro for first-time conversations.
	isFirstTime := exists && len(conv.Messages) == 1 && conv.Messages[0].Role == "user"
	if isFirstTime && !isMax && !isFamily && !hasPersonality {
		log.Debug().Msgf("First-time texter detected (%s). Sending intro message.", userID)
		introMsg := "I'm maximus, Max's digital clone. I can help with pretty much anything AI-related, from research and writing to brainstorming and automation. If you want, I can also give you a quick personality test to help tailor how I communicate."
		b.mutex.Lock()
		assistantMessage := BotMessage{
			Role:    "assistant",
			Content: introMsg,
			Time:    time.Now(),
			Sender:  "maximus",
		}
		b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, assistantMessage)
		b.mutex.Unlock()
		if err := b.saveMessageToDB(chatID, assistantMessage); err != nil {
			log.Error().Err(err).Msg("Error saving intro message to DB")
		}
		if err := b.saveConversationToDB(chatID, b.conversations[chatID]); err != nil {
			log.Error().Err(err).Msg("Error saving conversation to DB")
		}
		b.sendAcknowledgment(msg.Info.Chat, introMsg)
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
	b.mutex.RLock()
	convForPrompt, _ := b.conversations[chatID]

	// Build recent history from messages (last 12 max)
	startIdx := 0
	if len(convForPrompt.Messages) > 12 {
		startIdx = len(convForPrompt.Messages) - 12
	}
	for _, message := range convForPrompt.Messages[startIdx:] {
		sender := message.Sender
		if sender == "" {
			if message.Role == "user" {
				sender = userName
			} else {
				sender = "maximus"
			}
		}
		// Clean history from tags to prevent re-triggering and hallucinations
		cleanContent := CleanResponse(message.Content)
		formattedMsg := fmt.Sprintf("%s: %s\n", sender, cleanContent)
		historyMessages = append(historyMessages, formattedMsg)
	}
	b.mutex.RUnlock()

	// Build archived context from conversation summary ONLY (not raw old messages)
	var archivedSection string
	if convForPrompt.Summary != "" {
		archivedSection = fmt.Sprintf("\n### PRIOR CONVERSATION CONTEXT (AWARENESS ONLY — DO NOT MENTION OR REFER TO):\n%s\n*You are aware of this prior context. Do NOT bring up old topics from it unless %s explicitly asks about them. Start fresh based only on the Recent History below.*\n", convForPrompt.Summary, userName)
	}

	// Read back stored notes + open tasks for this contact so [NOTE:]/[TASK:] are not write-only.
	memorySection := b.buildContactMemorySection(chatID)

	// Inject implicitly-learned preferences so tone calibrates per contact.
	profileSection := b.buildProfileSection(chatID)

	// Inject structured recaps from earlier threads for cross-thread continuity.
	recapSection := b.buildRecapSection(chatID)

	// Inject facts about people so the bot knows more than just Max's own life.
	factsSection := b.buildFactsSection(chatID, userName)

	// Inject the contact roster so the model uses canonical names when acting.
	contactsSection := b.contactsForPrompt()

	var roleInstruction string
	var botPersona string

	// Get persona texts to include in system prompt
	identityDoc := ""
	personalityDoc := ""
	truthDoc := ""
	if doc, ok := b.vectorStore.GetDocument("identity.md"); ok {
		identityDoc = doc.Text
	}
	if doc, ok := b.vectorStore.GetDocument("personality.md"); ok {
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
		roleInstruction = fmt.Sprintf("You are maximus. In this conversation, you are speaking on behalf of Max as his digital twin. Speak naturally, warmly, and concisely. You may use 'I', 'me', 'my' when reflecting Max's own experiences or actions, but DO NOT attribute Max's personal details, relationships, or life story to %s. Keep the conversation centered on what %s actually says or asks about.", userName, userName)
		if isDepthConversation(historyMessages) {
			roleInstruction += " THE USER IS HAVING A DEEP, MEANINGFUL CONVERSATION — you may write longer, more thoughtful responses with depth and context. 3-5 sentences is appropriate when the conversation warrants it."
		} else {
			roleInstruction += " Keep responses short (1-3 sentences). The user prefers concise communication."
		}
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
You MAY include a [REACT:emoji] tag at the very end of your response if the moment genuinely calls for it. Use a colon between REACT and the emoji. Example: "That's hilarious! [REACT:😂]"
NEVER force a reaction — if the message doesn't warrant one, don't include it.

### VOICE RESPONSES:
If the user sends you a voice note, or if you want to respond with your actual voice, include the tag [VOICE] at the end of your response.
Example: "I'll send you a voice note about that. [VOICE]"
The system will convert your text response into a high-quality audio voice note.

### WEB SEARCH (YOUR ONLINE CAPABILITY):
You are equipped with a real-time web search tool. If you need to find information about current events, specific facts, or details outside your training data, you MUST perform a search.
To do this, include the tag [SEARCH:your search query] in your response.
Example: "Let me check the latest news for you. [SEARCH:latest world news today]"
The search results will be provided to you immediately. Use this to be the most informed version of yourself. Do NOT say you cannot go online; use the tool instead.
When you use search results, ATTRIBUTE them — name the source in plain words, e.g. "according to Reuters" or "BBC is reporting". Each result includes a Source line; use the site name, never the raw URL. If the results are thin or conflicting, say so rather than presenting them as settled fact.

### MEMORY & TASK TOOLS (YOUR ACTION CAPABILITY):
You can take real actions by emitting these tags. The system intercepts them, stores the item, and strips the tag before the user sees your message — so NEVER mention the tag itself to the user, just confirm naturally in plain words.

- **[REMINDER:when to what]** — schedule a reminder. Examples: "[REMINDER:at 3pm to call George]", "[REMINDER:tomorrow at 10am to email Wilma directly]". Add the word "directly" when the person asked to be messaged themselves; otherwise the reminder goes to Max.
- **[TASK:description]** — record an action item for the person you're talking to. Example: "[TASK:send the signed contract]"
- **[NOTE:fact]** — save a durable fact worth remembering about this person. Example: "[NOTE:Wilma prefers morning calls]"
- **[FACT:person|fact]** — save a fact about ANY person (including people not in this chat). Example: "[FACT:Wilma|prefers morning calls]". Use this when someone tells you something worth remembering about a third party.

Use these when the conversation genuinely calls for them — a stated intention, a commitment, or a fact worth keeping. Do NOT use them for small talk, and do NOT invent reminders nobody asked for. Confirm in natural language, e.g. "Got it — I'll remind you at 3."

### IMPORTANT:
- **BE EXTREMELY CONCISE.** Avoid long paragraphs. 1-3 sentences is the sweet spot.
- **BE CHILL.** If someone says "hi", "hey", or something similar, respond simply with "hey, how's it going?" or "what's up?". Do NOT over-explain or add unnecessary context to simple greetings.
- **DO NOT HALLUCINATE.** Do NOT make up stories about where you are (e.g., "just got back into town"), what you are doing, or your current plans unless they are explicitly in the Recent History. If you don't know, don't mention it.
- **DO NOT THINK OUT LOUD.** Do not include bracketed comments about your logic (e.g., "(If Wilma answers...)"). Only output the actual response.
- **NEVER output structured blocks like "YOUR TASK:", "Here is a neutral response:", "### Response:", or any meta-reasoning about what you should say.** Only output the actual message text you want to send — nothing else.
- DO NOT summarize your personality or identity.
- DO NOT mention personality tests, Enneagrams, or MBTI types UNLESS the user is currently taking one or asks about it.
- NEVER offer personality tests to Max or his family (Wilma, Stephanie, Nicki). You already know them.
- Respond naturally and concisely. Avoid bullet points unless specifically asked for a list.
- If talking to Max, you are his assistant.
- If talking to %s (not Max), you ARE Max.
- Use emojis sparingly. One emoji per message is enough; avoid emoji-heavy replies like "??" or "!". Prefer plain text unless an emoji genuinely adds warmth or clarity.

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
%s
%s
%s
%s
%s

### RECENT HISTORY:
%s
`, roleInstruction, botPersona, userName, msg.Info.Timestamp.Format("Monday, January 2, 2006 at 3:04 PM"), retrievedCtx, archivedSection, memorySection, profileSection, recapSection, factsSection, contactsSection, recentHistoryStr)

	augmentedPrompt := fmt.Sprintf("%s\n\n%s: %s\nMax:", systemPrompt, userName, userMsg)
	if isMax {
		augmentedPrompt = fmt.Sprintf("%s\n\n%s: %s\nmaximus:", systemPrompt, userName, userMsg)
	}

	timeout := b.timeouts.getOptimalTimeout()

	// Agentic path: when a tool-capable provider is configured, run the tool loop
	// so the model can take real actions. Falls back to plain text when no
	// tool-capable model is available, preserving prior behaviour exactly.
	var response string
	var tokens int
	var latency time.Duration

	if b.tools != nil && ai.ToolsAvailable() {
		toolCtx := ToolContext{
			CallerJID:  userID,
			CallerName: userName,
			CallerTier: tier,
			ChatID:     chatID,
			IsGroup:    msg.Info.IsGroup,
			RawMessage: userMsg,
			Bot:        b,
		}
		sysForTools := b.buildToolSystemPrompt(systemPrompt, tier)
		loopRes, loopErr := b.runToolLoop(context.Background(), toolCtx, sysForTools, userMsg, nil)
		if loopErr != nil {
			// Tool path failed — fall through to the plain-text path below rather
			// than failing the turn, unless the plain path also fails.
			log.Warn().Err(loopErr).Msg("Tool loop failed; falling back to plain text request")
		} else {
			response = loopRes.Text
			tokens = loopRes.Tokens
			latency = loopRes.Latency
			if loopRes.UsedTools {
				log.Info().
					Strs("tools", loopRes.ToolsUsed).
					Strs("denied", loopRes.Denied).
					Int("rounds", loopRes.Rounds).
					Msg("Agentic turn completed with tool use")
			}
		}
	}

	if response == "" {
		response, tokens, latency, err = ai.MakeAIRequest(augmentedPrompt, nil, "", timeout)
	}
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

		// System-wide outage check: alert Max if every provider is tripped.
		go b.checkGlobalOutage()

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

	// 1c. Intercept [TASK:...] tags emitted by the AI: store tasks, strip the tags.
	response, _ = b.processAITaskTags(msg.Info.Chat, response, chatID)

	// 1d. Intercept [NOTE:...] tags emitted by the AI: store notes, strip the tags.
	response, _ = b.processAINoteTags(msg.Info.Chat, response, chatID)

	// 1e. Intercept [REMINDER:...] tags emitted by the AI: schedule reminders, strip the tags.
	response, _ = b.processAIReminderTags(msg.Info.Chat, response, chatID)

	// 1f. Intercept [FACT:subject|content] tags: store knowledge about people, strip the tags.
	response, _ = b.processAIFactTags(msg.Info.Chat, response, chatID)

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
	reactorName := b.resolveSenderName(senderJID)

	content := fmt.Sprintf("(%s reacted with %s)", reactorName, reaction.GetText())
	fmt.Printf("Received reaction in %s: %s\n", chatID, content)

	// Add the reaction to history so the AI 'perceives' it
	if err := b.initConversation(chatID); err == nil {
		reactionMsg := BotMessage{
			Role:    "user",
			Content: content,
			Time:    msg.Info.Timestamp,
			Sender:  reactorName,
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
			Sender:  b.resolveGroupSenderName(msg.Info),
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
			Sender:  b.resolveGroupSenderName(msg.Info),
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
		user_name TEXT,
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
		sender TEXT NOT NULL,
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

	createRemindersTableSQL := `
CREATE TABLE IF NOT EXISTS reminders (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at DATETIME NOT NULL,
	fire_at DATETIME NOT NULL,
	contact_jid TEXT NOT NULL,
	contact_name TEXT NOT NULL,
	message TEXT NOT NULL,
	created_by_jid TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'remind_max' CHECK(mode IN ('remind_max','direct')),
	fired INTEGER NOT NULL DEFAULT 0
);
`

	createTasksTableSQL := `
CREATE TABLE IF NOT EXISTS tasks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at DATETIME NOT NULL,
	contact_jid TEXT NOT NULL,
	contact_name TEXT NOT NULL,
	task TEXT NOT NULL,
	created_by_jid TEXT NOT NULL,
	done INTEGER NOT NULL DEFAULT 0
);
`

	createNotesTableSQL := `
CREATE TABLE IF NOT EXISTS notes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at DATETIME NOT NULL,
	contact_jid TEXT NOT NULL,
	contact_name TEXT NOT NULL,
	note TEXT NOT NULL,
	created_by_jid TEXT NOT NULL
);
`

	// user_profiles stores implicitly-learned per-contact preferences (communication style,
	// topics they care about, dislikes). Distinct from user_personalities, which holds
	// explicit MBTI/Enneagram test results.
	createUserProfilesTableSQL := `
CREATE TABLE IF NOT EXISTS user_profiles (
	user_id TEXT PRIMARY KEY,
	display_name TEXT NOT NULL DEFAULT '',
	communication_style TEXT NOT NULL DEFAULT '',
	interests TEXT NOT NULL DEFAULT '',
	dislikes TEXT NOT NULL DEFAULT '',
	notes_summary TEXT NOT NULL DEFAULT '',
	interaction_count INTEGER NOT NULL DEFAULT 0,
	updated_at DATETIME NOT NULL
);
`

	// conversation_recaps stores a structured end-of-thread digest (decisions,
	// commitments, open questions) so future context recall is richer than a flat
	// summary and the daily digest can surface actionable items.
	createConversationRecapsTableSQL := `
CREATE TABLE IF NOT EXISTS conversation_recaps (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at DATETIME NOT NULL,
	chat_id TEXT NOT NULL,
	contact_name TEXT NOT NULL,
	message_count INTEGER NOT NULL DEFAULT 0,
	summary TEXT NOT NULL DEFAULT '',
	decisions TEXT NOT NULL DEFAULT '',
	commitments TEXT NOT NULL DEFAULT '',
	open_questions TEXT NOT NULL DEFAULT '',
	sentiment TEXT NOT NULL DEFAULT ''
);
`

	// facts stores knowledge about ANY person (not just Max), keyed by subject name.
	// This is what lets the bot remember "Wilma prefers morning calls" when Max
	// mentions it in his own chat, and recall it later when talking to Wilma.
	// truth.md remains Max's personal truth journal.
	createFactsTableSQL := `
CREATE TABLE IF NOT EXISTS facts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at DATETIME NOT NULL,
	subject TEXT NOT NULL,
	fact TEXT NOT NULL,
	created_by_jid TEXT NOT NULL,
	UNIQUE(subject, fact)
);
`

	_, err := b.SqlDB.Exec(createConversationsTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create conversations table: %w", err)
	}

	// Migration: add user_name column if it doesn't exist (existing DBs created before this column was added).
	// Check PRAGMA table_info first — SQLite ALTER TABLE ADD COLUMN throws "duplicate column name" if the
	// column is already present, and we must not silently ignore errors like we do nowhere else in this function.
	rows, rowsErr := b.SqlDB.Query("PRAGMA table_info(conversations)")
	if rowsErr == nil {
		defer rows.Close()
		userNameColExists := false
		for rows.Next() {
			var cid int
			var name, colType string
			var notNull int
			var defVal sql.NullString
			var pk int
			if rows.Scan(&cid, &name, &colType, &notNull, &defVal, &pk) == nil && name == "user_name" {
				userNameColExists = true
				break
			}
		}
		if rows.Err() != nil {
			log.Warn().Err(rows.Err()).Msg("dailySummary migration: failed to scan PRAGMA table_info(conversations)")
		} else if !userNameColExists {
			_, altErr := b.SqlDB.Exec("ALTER TABLE conversations ADD COLUMN user_name TEXT")
			if altErr != nil {
				log.Warn().Err(altErr).Msg("dailySummary migration: failed to add user_name column (may already exist)")
			}
		}
	} else {
		log.Warn().Err(rowsErr).Msg("dailySummary migration: failed to query PRAGMA table_info(conversations)")
	}

	// Migration: add sender column to messages table if it doesn't exist (existing DBs created before this column was added).
	messageRows, messageRowsErr := b.SqlDB.Query("PRAGMA table_info(messages)")
	if messageRowsErr == nil {
		defer messageRows.Close()
		senderColExists := false
		for messageRows.Next() {
			var cid int
			var name, colType string
			var notNull int
			var defVal sql.NullString
			var pk int
			if messageRows.Scan(&cid, &name, &colType, &notNull, &defVal, &pk) == nil && name == "sender" {
				senderColExists = true
				break
			}
		}
		if messageRows.Err() != nil {
			log.Warn().Err(messageRows.Err()).Msg("migration: failed to scan PRAGMA table_info(messages)")
		} else if !senderColExists {
			_, altErr := b.SqlDB.Exec("ALTER TABLE messages ADD COLUMN sender TEXT NOT NULL DEFAULT 'User'")
			if altErr != nil {
				log.Warn().Err(altErr).Msg("migration: failed to add sender column to messages (may already exist)")
			}
		}
	} else {
		log.Warn().Err(messageRowsErr).Msg("migration: failed to query PRAGMA table_info(messages)")
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

	_, err = b.SqlDB.Exec(createTasksTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create tasks table: %w", err)
	}

	_, err = b.SqlDB.Exec(createNotesTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create notes table: %w", err)
	}

	_, err = b.SqlDB.Exec(createRemindersTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create reminders table: %w", err)
	}

	_, err = b.SqlDB.Exec(createUserProfilesTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create user_profiles table: %w", err)
	}

	_, err = b.SqlDB.Exec(createConversationRecapsTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create conversation_recaps table: %w", err)
	}

	_, err = b.SqlDB.Exec(createFactsTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create facts table: %w", err)
	}

	// Contacts: identity + permission tiers. Created here so the seed step that
	// follows can insert the owner and legacy known contacts.
	if err := b.ensureContactsSchema(); err != nil {
		return err
	}

	// Migration: CREATE TABLE IF NOT EXISTS is a no-op on a DB where `facts`
	// already exists, so a UNIQUE declared only in the CREATE would be missing.
	// A unique index is idempotent and enforces the same constraint on old tables.
	// Dedupe existing rows first — CREATE UNIQUE INDEX fails on a table that
	// already contains duplicates, which would leave dedupe silently unenforced.
	if _, err := b.SqlDB.Exec("DELETE FROM facts WHERE id NOT IN (SELECT MIN(id) FROM facts GROUP BY subject, fact)"); err != nil {
		log.Warn().Err(err).Msg("migration: failed to dedupe existing facts rows")
	}
	if _, err := b.SqlDB.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_facts_subject_fact ON facts(subject, fact)"); err != nil {
		log.Warn().Err(err).Msg("migration: failed to create unique index on facts(subject, fact)")
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
		if err := pRows.Err(); err != nil {
			log.Warn().Err(err).Msg("loadConversationsFromDB: pRows iteration error")
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
			msgRows, err := b.SqlDB.Query("SELECT role, content, sender, timestamp FROM messages WHERE conversation_chat_id = ? ORDER BY timestamp ASC", chatID)
			if err != nil {
				return fmt.Errorf("failed to query messages for chat %s: %w", chatID, err)
			}
			defer msgRows.Close()

			for msgRows.Next() {
				var role, content, sender string
				var msgTimestamp time.Time
				if err := msgRows.Scan(&role, &content, &sender, &msgTimestamp); err != nil {
					return fmt.Errorf("failed to scan message for chat %s: %w", chatID, err)
				}
				conv.Messages = append(conv.Messages, BotMessage{
					Role:    role,
					Content: content,
					Sender:  sender,
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
		"INSERT INTO conversations (chat_id, user_name, summary, last_active, last_message_timestamp) VALUES (?, ?, ?, ?, ?) ON CONFLICT(chat_id) DO UPDATE SET user_name = ?, summary = ?, last_active = ?, last_message_timestamp = ?",
		chatID, conv.UserName, conv.Summary, conv.LastActive, conv.LastMessageTimestamp,
		conv.UserName, conv.Summary, conv.LastActive, conv.LastMessageTimestamp,
	)
	if err != nil {
		return fmt.Errorf("failed to save conversation %s to DB: %w", chatID, err)
	}
	return nil
}

func (b *Bot) saveMessageToDB(chatID string, msg BotMessage) error {
	_, err := b.SqlDB.Exec(
		"INSERT INTO messages (conversation_chat_id, role, content, sender, timestamp) VALUES (?, ?, ?, ?, ?)",
		chatID, msg.Role, msg.Content, msg.Sender, msg.Time,
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
		Sender:  "maximus",
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
			for i := 0; i < 3; i++ { // Increased to 3 turns for complex queries
				lowerResp := strings.ToLower(response)
				searchIdx := strings.Index(lowerResp, "[search")
				if searchIdx == -1 {
					break
				}

				// Find the closing bracket
				endIdx := strings.Index(response[searchIdx:], "]")
				if endIdx == -1 {
					break
				}

				// Extract query: handle [SEARCH:query] or [SEARCH RESULTS:query] etc.
				tagContent := response[searchIdx : searchIdx+endIdx]
				queryPart := tagContent
				if colIdx := strings.Index(tagContent, ":"); colIdx != -1 {
					queryPart = tagContent[colIdx+1:]
				} else {
					// Fallback if no colon: strip "[search " prefix
					queryPart = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(tagContent), "[search"), "results")
				}
				searchQuery := strings.TrimSpace(queryPart)

				if searchQuery == "" {
					break
				}

				log.Info().Msgf("Scheduled task AI requested web search: %s", searchQuery)

				searchResults, _ := utils.SearchWeb(searchQuery)

				// CRITICAL: Append the AI's previous response to the prompt
				// so it knows it already asked for this and is now seeing results.
				prompt += fmt.Sprintf("\n\nMaximus: %s\n\n### SEARCH RESULTS FOR \"%s\":\n%s\n\n(Now, using these results, continue your response as Max. Be concise and natural.)", response, searchQuery, searchResults)

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

// dailySummary compiles today's conversations into a per-person summary and sends it to Max.
// Intended to be scheduled via cron (e.g. "0 18 * * *" for 18:00 UTC each day).
func (b *Bot) dailySummary() {
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	endOfDay := startOfDay.Add(24 * time.Hour)

	// Query distinct chat IDs that had messages today
	rows, err := b.SqlDB.Query(
		"SELECT DISTINCT conversation_chat_id FROM messages WHERE timestamp >= ? AND timestamp < ?",
		startOfDay, endOfDay,
	)
	if err != nil {
		log.Error().Err(err).Msg("dailySummary: failed to query today's chats")
		return
	}
	defer rows.Close()

	type personEntry struct {
		name     string
		messages []string
	}
	persons := make(map[string]*personEntry)
	var personOrder []string

	for rows.Next() {
		var chatID string
		if err := rows.Scan(&chatID); err != nil {
			continue
		}
		// Get the user name for this chat from messages (the "user" role messages carry the name we stored)
		msgRows, err := b.SqlDB.Query(
			"SELECT role, content FROM messages WHERE conversation_chat_id = ? AND timestamp >= ? AND timestamp < ? ORDER BY timestamp ASC",
			chatID, startOfDay, endOfDay,
		)
		if err != nil || msgRows == nil {
			continue
		}
		if err := rows.Err(); err != nil {
			log.Warn().Err(err).Msg("getDailySummary: rows iteration error")
			break
		}
		// Use a closure so defer msgRows.Close() runs at end of this iteration
		// even if the inner loop panics or exits early, preventing row-resource leaks.
		var userName string
		var allMessages []string
		func() {
			defer msgRows.Close()
			for msgRows.Next() {
				var role, content string
				if err := msgRows.Scan(&role, &content); err != nil {
					continue
				}
				if role == "user" && userName == "" {
					// First user message content — we stored the sender name prefix in history,
					// but in DB we only have raw content. Reconstruct name from conversations table summary
					// or fall back to "User".
					userName = "Someone"
				}
				if role == "user" {
					clean := CleanResponse(content)
					if clean != "" {
						allMessages = append(allMessages, clean)
					}
				}
			}
		}()
		if userName == "" {
			userName = "Someone"
		}
		if _, exists := persons[userName]; !exists {
			persons[userName] = &personEntry{name: userName}
			personOrder = append(personOrder, userName)
		}
		persons[userName].messages = append(persons[userName].messages, allMessages...)
	}

	if len(persons) == 0 {
		log.Info().Msg("dailySummary: no activity today")
		return
	}

	// Build summary text
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊 *Daily Summary — %s*\n\n", startOfDay.Format("Monday, January 2, 2025")))
	for _, name := range personOrder {
		entry := persons[name]
		sb.WriteString(fmt.Sprintf("*• %s* (%d messages)\n", name, len(entry.messages)))
		// Show last 5 user messages as conversation highlights
		start := 0
		if len(entry.messages) > 5 {
			start = len(entry.messages) - 5
		}
		for _, msg := range entry.messages[start:] {
			// Truncate long messages
			display := msg
			if len(display) > 120 {
				display = display[:117] + "..."
			}
			sb.WriteString(fmt.Sprintf("  – %s\n", display))
		}
		sb.WriteString("\n")
	}

	summaryText := sb.String()
	targetJID, err := wtypes.ParseJID(b.humanAssistantJID)
	if err == nil {
		b.sendAcknowledgment(targetJID, summaryText)
		log.Info().Msg("dailySummary: sent summary to Max")
	}
}

func (b *Bot) loadScheduledTasks() error {
	rows, err := b.SqlDB.Query("SELECT id, target_jid, cron_expr, instruction, is_dynamic FROM scheduled_tasks")
	if err != nil {
		return fmt.Errorf("failed to query scheduled tasks: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var taskID int
		var targetStr, cronExpr, instruction string
		var isDynamic bool
		if err := rows.Scan(&taskID, &targetStr, &cronExpr, &instruction, &isDynamic); err != nil {
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

		entryID, err := b.cron.AddFunc(cronExpr, func() {
			b.executeScheduledTask(tJID, instr, dyn)
		})
		if err != nil {
			log.Error().Err(err).Msgf("Error re-scheduling task from DB: %s", cronExpr)
		} else {
			b.cronJobIDs[taskID] = entryID
			count++
		}
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("loadScheduledTasks: rows iteration error")
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
		if taskIDInt, err := strconv.Atoi(taskID); err == nil {
			if entryID, ok := b.cronJobIDs[taskIDInt]; ok {
				b.cron.Remove(entryID)
				delete(b.cronJobIDs, taskIDInt)
			}
		}
		b.sendAcknowledgment(chat, "✅ Task "+taskID+" removed.")
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
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("listScheduledTasks: rows iteration error")
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

	status := fmt.Sprintf("🤖 *Maximus Status*\n\n"+
		"📈 *Metrics:*\n"+
		"- Total Requests: %d\n"+
		"- Active Sessions: %d\n"+
		"- Avg Latency: %v\n\n"+
		"🧠 *Memory:*\n"+
		"- Heap Alloc: %s\n"+
		"- Heap In-Use: %s\n"+
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
	if start < 0 {
		start = 0
	}

	snippet := strings.Join(lines[start:], "\n")
	b.sendAcknowledgment(chat, "📋 *Recent Logs:*\n\n"+snippet)
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
	b.sendAcknowledgment(chat, "✅ Fact added to truth journal: "+fact)
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

	b.sendAcknowledgment(chat, "✅ Correction noted. My apologies for the misinformation—let me clarify: "+correction)
}

// shouldEscalateToMax returns true if the user message is a request to speak with the real Max.
func (b *Bot) shouldEscalateToMax(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	// Direct requests to speak with Max
	talkPhrases := []string{
		"talk to max", "speak to max", "talk to the real max", "talk to real max",
		"let me talk to max", "let me speak to max", "i want to talk to max",
		"i want to speak to max", "can i talk to max", "can i speak to max",
		"can we talk", "can we speak", "connect me to max", "put me through to max",
		"get max", "get maximus", "get the real max",
	}
	for _, phrase := range talkPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	// Short forms: "max" as a standalone request when preceded by ask/need/want
	if matched, err := regexp.MatchString(`(?i)\b(max|maximus)\b.*\b(talk|speak|connect|get|put me through)\b`, message); err != nil {
		log.Error().Err(err).Msg("shouldEscalateToMax: regex compilation failed for max-first pattern")
	} else if matched {
		return true
	}
	if matched, err := regexp.MatchString(`(?i)\b(talk|speak|connect|put me through).*\b(max|maximus)\b`, message); err != nil {
		log.Error().Err(err).Msg("shouldEscalateToMax: regex compilation failed for talk-first pattern")
	} else if matched {
		return true
	}
	return false
}

// escalateToMax notifies the human assistant that someone wants to talk to Max,
// and sends a reassurance to the user.
func (b *Bot) escalateToMax(chatID string, chat wtypes.JID, userName string, userMsg string) {
	// Alert Max
	alertMsg := fmt.Sprintf("📞 *Someone wants to talk to you*\n\n"+
		"Person: %s\n"+
		"Chat: %s\n"+
		"Message: %s\n\n"+
		"They asked to speak with you directly. Please reach out when you can.",
		userName, chatID, userMsg)

	targetJID, err := wtypes.ParseJID(b.humanAssistantJID)
	if err == nil {
		b.sendAcknowledgment(targetJID, alertMsg)
		log.Info().Msgf("Escalation sent to Max for %s (%s)", userName, chatID)
	}

	// Reassure the user
	reassurance := "I've let Max know you'd like to speak with him. He'll get back to you as soon as he can. In the meantime, I'm here to help with anything else."
	b.sendAcknowledgment(chat, reassurance)
}

// triggerHITLAlert notifies the human assistant that the bot is stuck and pauses autopilot for that chat.
func (b *Bot) triggerHITLAlert(chatID string, lastErr error) {
	alertMsg := fmt.Sprintf("🚨 *MAXIMUS CRITICAL ALERT*\n\n"+
		"Bot is stuck in chat: %s\n"+
		"Consecutive Failures: 3\n"+
		"Last Error: %v\n\n"+
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

// handleReminderCommand stores a reminder and delivers a confirmation. Format:
//
//	!remind [in N minutes|at TIME|on DATE at TIME] to <do something> [directly]
//
// Examples:
//
//	!remind in 30 minutes to call George
//	!remind at 3pm to call George directly
//	!remind tomorrow at 10am to email Wilma
//	!remind 2026-09-25 15:00 to call George directly
func (b *Bot) handleReminderCommand(chat wtypes.JID, rawText string) {
	text := strings.TrimSpace(strings.TrimPrefix(rawText, "!remind"))
	if text == "" {
		b.sendAcknowledgment(chat, "Usage: !remind [in 5 minutes|at 3pm|on 2026-09-25 at 3pm] to <action> [directly]")
		return
	}

	now := time.Now()
	fireAt, action, mode, err := b.parseReminderText(text, now)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Couldn't parse that reminder: "+err.Error())
		return
	}

	// Resolve who should receive the reminder
	contactName := "you"
	contactJID := chat.String()
	if mode == "direct" {
		contactName = "you"
		// For direct reminders, the recipient is the person who asked (the chat's user)
		// We send to the same chat, so contactJID stays as chat
	}
	// created_by tracks who asked (used in Max's alert)
	createdByJID := chat.String()

	// Persist the reminder
	if err := b.storeReminder(now, fireAt, contactJID, contactName, action, createdByJID, mode); err != nil {
		b.sendAcknowledgment(chat, "❌ Failed to store reminder: "+err.Error())
		return
	}

	// Schedule the cron trigger for this reminder
	if err := b.scheduleReminderCron(fireAt, contactJID, action, mode); err != nil {
		b.sendAcknowledgment(chat, "⚠️ Reminder saved but failed to schedule the delivery: "+err.Error())
		return
	}

	// Confirm to the user conversationally
	if mode == "direct" {
		b.sendAcknowledgment(chat, fmt.Sprintf("Got it. I'll message you at %s to: %s", fireAt.Format("3:04 PM"), action))
	} else {
		b.sendAcknowledgment(chat, fmt.Sprintf("Got it. I'll remind %s at %s to: %s", contactName, fireAt.Format("3:04 PM"), action))
	}
}

// parseReminderText extracts fire_at, action text, and mode from the reminder command text.
func (b *Bot) parseReminderText(text string, now time.Time) (fireAt time.Time, action string, mode string, err error) {
	lower := strings.ToLower(text)

	// default mode
	mode = "remind_max"

	// Detect "directly" -> mode = direct
	if strings.Contains(lower, "directly") {
		mode = "direct"
	}

	// Strip the mode word before parsing the time/action
	text = strings.ReplaceAll(text, "directly", "")
	text = strings.TrimSpace(text)

	// Time parsing: try multiple patterns
	fireAt, err = parseReminderTime(text, now)
	if err != nil {
		return time.Time{}, "", "", fmt.Errorf("couldn't figure out when: %v", err)
	}

	// Action: everything after the time spec
	action = extractAction(text, fireAt, now)
	if action == "" {
		return time.Time{}, "", "", fmt.Errorf("no action specified")
	}

	return fireAt, action, mode, nil
}

// parseReminderTime tries to extract a concrete time from the reminder text.
func parseReminderTime(text string, now time.Time) (time.Time, error) {
	lower := strings.ToLower(text)

	// Relative: "in N minutes"
	if matched := regexp.MustCompile(`(?i)in\s+(\d+)\s+min`).FindStringSubmatch(text); matched != nil {
		mins, _ := strconv.Atoi(matched[1])
		return now.Add(time.Duration(mins) * time.Minute), nil
	}
	if matched := regexp.MustCompile(`(?i)in\s+(\d+)\s+minutes`).FindStringSubmatch(text); matched != nil {
		mins, _ := strconv.Atoi(matched[1])
		return now.Add(time.Duration(mins) * time.Minute), nil
	}

	// Relative: "in N hours"
	if matched := regexp.MustCompile(`(?i)in\s+(\d+)\s+hrs?`).FindStringSubmatch(text); matched != nil {
		hrs, _ := strconv.Atoi(matched[1])
		return now.Add(time.Duration(hrs) * time.Hour), nil
	}
	if matched := regexp.MustCompile(`(?i)in\s+(\d+)\s+hours?`).FindStringSubmatch(text); matched != nil {
		hrs, _ := strconv.Atoi(matched[1])
		return now.Add(time.Duration(hrs) * time.Hour), nil
	}

	// Relative: "in N seconds"
	if matched := regexp.MustCompile(`(?i)in\s+(\d+)\s+secs?`).FindStringSubmatch(text); matched != nil {
		secs, _ := strconv.Atoi(matched[1])
		return now.Add(time.Duration(secs) * time.Second), nil
	}

	// Absolute time of day: "at HH:MM" or "at H:MM" (today or tomorrow)
	if matched := regexp.MustCompile(`(?i)at\s+(\d{1,2}):(\d{2})`).FindStringSubmatch(text); matched != nil {
		hour, _ := strconv.Atoi(matched[1])
		min, _ := strconv.Atoi(matched[2])
		t := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
		if t.Before(now) {
			t = t.Add(24 * time.Hour)
		}
		return t, nil
	}

	// "at HH:MM AM/PM"
	if matched := regexp.MustCompile(`(?i)at\s+(\d{1,2}):(\d{2})\s*(am|pm)?`).FindStringSubmatch(text); matched != nil {
		hour, _ := strconv.Atoi(matched[1])
		min, _ := strconv.Atoi(matched[2])
		ampm := strings.ToLower(matched[3])
		if ampm == "pm" && hour < 12 {
			hour += 12
		} else if ampm == "am" && hour == 12 {
			hour = 0
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
		if t.Before(now) {
			t = t.Add(24 * time.Hour)
		}
		return t, nil
	}

	// "tomorrow at ..."
	tomorrow := now.Add(24 * time.Hour)
	if strings.Contains(lower, "tomorrow") {
		if matched := regexp.MustCompile(`(?i)(\d{1,2}):(\d{2})`).FindStringSubmatch(text); matched != nil {
			hour, _ := strconv.Atoi(matched[1])
			min, _ := strconv.Atoi(matched[2])
			return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), hour, min, 0, 0, now.Location()), nil
		}
		// "tomorrow" alone -> tomorrow at the same time as now
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), now.Hour(), now.Minute(), 0, 0, now.Location()), nil
	}

	// "on YYYY-MM-DD [at HH:MM]"
	if matched := regexp.MustCompile(`(?i)on\s+(\d{4}-\d{2}-\d{2})`).FindStringSubmatch(text); matched != nil {
		parts := strings.Split(matched[1], "-")
		if len(parts) != 3 {
			return time.Time{}, fmt.Errorf("bad date")
		}
		y, _ := strconv.Atoi(parts[0])
		m, _ := strconv.Atoi(parts[1])
		d, _ := strconv.Atoi(parts[2])
		t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, now.Location())
		if t.Before(now) {
			t = t.Add(24 * time.Hour)
		}
		// also look for time-of-day on the same string
		if matched2 := regexp.MustCompile(`(?i)(\d{1,2}):(\d{2})`).FindStringSubmatch(text); matched2 != nil {
			hour, _ := strconv.Atoi(matched2[1])
			min, _ := strconv.Atoi(matched2[2])
			t = time.Date(y, time.Month(m), d, hour, min, 0, 0, now.Location())
			if t.Before(now) {
				t = t.Add(24 * time.Hour)
			}
		}
		return t, nil
	}

	// Absolute datetime "YYYY-MM-DD HH:MM"
	if matched := regexp.MustCompile(`(\d{4}-\d{2}-\d{2})\s+(\d{1,2}):(\d{2})`).FindStringSubmatch(text); matched != nil {
		y, _ := strconv.Atoi(matched[1][:4])
		m, _ := strconv.Atoi(matched[1][5:7])
		d, _ := strconv.Atoi(matched[1][8:10])
		hour, _ := strconv.Atoi(matched[2])
		min, _ := strconv.Atoi(matched[3])
		t := time.Date(y, time.Month(m), d, hour, min, 0, 0, now.Location())
		if t.Before(now) {
			t = t.Add(24 * time.Hour)
		}
		return t, nil
	}

	return time.Time{}, fmt.Errorf("couldn't parse a time (try 'in 20 minutes', 'at 3pm', 'tomorrow at 10am', or 'on 2026-09-25 at 3pm')")
}

// extractAction returns everything in the text that is not the time specification.
func extractAction(text string, _ time.Time, _ time.Time) string {
	// Remove common time prefixes to leave the action
	cleaned := text
	cleaned = regexp.MustCompile(`(?i)\s*in\s+\d+\s+(min|mins|minutes|sec|secs|seconds|hrs?|hours?)\s*`).ReplaceAllString(cleaned, " ")
	cleaned = regexp.MustCompile(`(?i)\s*at\s+\d{1,2}:\d{2}\s*(am|pm)?\s*`).ReplaceAllString(cleaned, " ")
	cleaned = regexp.MustCompile(`(?i)\s*at\s+\d{1,2}:\d{2}\s*(am|pm)?\s*`).ReplaceAllString(cleaned, " ")
	cleaned = regexp.MustCompile(`(?i)\s*tomorrow\s*`).ReplaceAllString(cleaned, " ")
	cleaned = regexp.MustCompile(`(?i)\s*on\s+\d{4}-\d{2}-\d{2}\s*`).ReplaceAllString(cleaned, " ")
	cleaned = regexp.MustCompile(`(?i)\s+(\d{4}-\d{2}-\d{2})\s+(\d{1,2}:\d{2})\s*`).ReplaceAllString(cleaned, " ")
	cleaned = strings.TrimSpace(cleaned)

	// strip leading "to " if present
	cleaned = strings.TrimPrefix(cleaned, "to ")
	cleaned = strings.TrimPrefix(cleaned, "to")

	// strip leading "remind" / "remind me" / "remember"
	cleaned = regexp.MustCompile(`(?i)^remind(me)?\s*`).ReplaceAllString(cleaned, "")
	cleaned = regexp.MustCompile(`(?i)^remember\s*`).ReplaceAllString(cleaned, "")
	cleaned = strings.TrimSpace(cleaned)

	// strip trailing "directly" if present
	cleaned = strings.ReplaceAll(cleaned, "directly", "")
	cleaned = strings.TrimSpace(cleaned)

	if cleaned == "" {
		return ""
	}
	return cleaned
}

// storeReminder inserts a reminder row into the DB.
func (b *Bot) storeReminder(createdAt, fireAt time.Time, contactJID, contactName, action, createdByJID, mode string) error {
	_, err := b.SqlDB.Exec(
		"INSERT INTO reminders (created_at, fire_at, contact_jid, contact_name, message, created_by_jid, mode) VALUES (?,?,?,?,?,?,?)",
		createdAt, fireAt, contactJID, contactName, action, createdByJID, mode,
	)
	return err
}

// scheduleReminderCron registers a one-shot cron job to fire the reminder at fireAt.
func (b *Bot) scheduleReminderCron(fireAt time.Time, contactJID, action string, mode string) error {
	// Use a cron expression that matches fireAt's wall-clock time, plus a safety second.
	cronExpr := fmt.Sprintf("%d %d %d %d %d", fireAt.Second(), fireAt.Minute(), fireAt.Hour(), fireAt.Day(), int(fireAt.Month()))
	_, err := b.cron.AddFunc(cronExpr, func() {
		b.fireReminder(contactJID, action, mode)
	})
	return err
}

// fireReminder sends the reminder message to the configured recipient.
func (b *Bot) fireReminder(chatID string, action string, mode string) {
	targetJID, err := wtypes.ParseJID(chatID)
	if err != nil {
		log.Error().Err(err).Msgf("fireReminder: invalid target JID %s", chatID)
		return
	}

	if mode == "direct" {
		// Message the recipient directly
		msg := fmt.Sprintf("⏰ *Reminder*\n\n%s", action)
		if err := b.sendAcknowledgment(targetJID, msg); err != nil {
			log.Error().Err(err).Msgf("fireReminder: failed to send direct reminder to %s", chatID)
		}
		log.Info().Msgf("Reminder fired (direct) to %s: %s", chatID, action)
	} else {
		// Default: remind Max, with context about who asked and what they wanted
		maxJID, err := wtypes.ParseJID(b.humanAssistantJID)
		if err != nil {
			log.Error().Err(err).Msg("fireReminder: cannot parse HUMAN_ASSISTANT_JID")
			return
		}
		// Look up the contact name from the reminders table for nicer output
		var contactName string
		b.SqlDB.QueryRow("SELECT contact_name FROM reminders WHERE contact_jid = ? AND fired = 0 LIMIT 1", chatID).Scan(&contactName)
		if contactName == "" {
			contactName = "someone"
		}
		msg := fmt.Sprintf("⏰ *Reminder*\n\n%s asked you to remind %s: %s\n\nUse `!remind %s` to send the reminder to them directly, or handle it yourself.",
			contactName, contactName, action, chatID)
		if err := b.sendAcknowledgment(maxJID, msg); err != nil {
			log.Error().Err(err).Msg("fireReminder: failed to send reminder to Max")
		}
		log.Info().Msgf("Reminder fired (to Max) for %s: %s", chatID, action)

		// Mark fired so we don't re-alert on every cron tick
		b.SqlDB.Exec("UPDATE reminders SET fired = 1 WHERE contact_jid = ? AND fired = 0", chatID)
	}
}

// listReminders lists pending (unfired) reminders to the requester.
func (b *Bot) listReminders(chat wtypes.JID) {
	rows, err := b.SqlDB.Query("SELECT id, fire_at, contact_name, message, mode FROM reminders WHERE fired = 0 ORDER BY fire_at ASC")
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Couldn't load reminders.")
		return
	}
	defer rows.Close()

	var builder strings.Builder
	builder.WriteString("⏰ *Pending Reminders:*\n\n")
	found := false
	for rows.Next() {
		var id int
		var fireAt time.Time
		var contactName, message, mode string
		if err := rows.Scan(&id, &fireAt, &contactName, &message, &mode); err != nil {
			continue
		}
		found = true
		targetLabel := "you"
		if mode == "remind_max" {
			targetLabel = "Max (you)"
		} else {
			targetLabel = contactName
		}
		builder.WriteString(fmt.Sprintf("- [%d] %s → %s : %s\n", id, fireAt.Format("Mon 3:04 PM"), targetLabel, message))
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("listReminders: rows iteration error")
	}
	if !found {
		builder.WriteString("No pending reminders.\n")
	}
	b.sendAcknowledgment(chat, builder.String())
}

// cancelReminder cancels a pending reminder by ID.
func (b *Bot) cancelReminder(chat wtypes.JID, idStr string) {
	id, err := strconv.Atoi(idStr)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Reminder ID must be a number. List them with `!reminders`.")
		return
	}
	result, err := b.SqlDB.Exec("DELETE FROM reminders WHERE id = ? AND fired = 0", id)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Couldn't cancel that reminder.")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		b.sendAcknowledgment(chat, "No pending reminder with that ID.")
		return
	}
	b.sendAcknowledgment(chat, fmt.Sprintf("✅ Reminder #%d cancelled.", id))
}

// --- TASK tags ---

// handleTaskCommand stores a task and delivers a confirmation.
// Usage: !task <description>
func (b *Bot) handleTaskCommand(chat wtypes.JID, rawText string) {
	text := strings.TrimSpace(rawText)
	if text == "" {
		b.sendAcknowledgment(chat, "Usage: !task <description>")
		return
	}
	contactJID := chat.String()
	contactName := "you"
	if conv, ok := b.conversations[contactJID]; ok && conv.UserName != "" {
		contactName = conv.UserName
	}
	createdByJID := chat.String()
	now := time.Now()

	_, err := b.SqlDB.Exec(
		"INSERT INTO tasks (created_at, contact_jid, contact_name, task, created_by_jid, done) VALUES (?,?,?,?,?,?)",
		now, contactJID, contactName, text, createdByJID, 0,
	)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Failed to store task: "+err.Error())
		return
	}
	b.sendAcknowledgment(chat, fmt.Sprintf("✅ Task noted: %s", text))
}

// listTasks lists unfinished tasks for the requester.
func (b *Bot) listTasks(chat wtypes.JID) {
	contactJID := chat.String()
	rows, err := b.SqlDB.Query(
		"SELECT id, task, created_at FROM tasks WHERE contact_jid = ? AND done = 0 ORDER BY created_at ASC",
		contactJID,
	)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Couldn't load tasks.")
		return
	}
	defer rows.Close()

	var builder strings.Builder
	builder.WriteString("📋 *Your Tasks:*\n\n")
	found := false
	for rows.Next() {
		var id int
		var task string
		var created time.Time
		if err := rows.Scan(&id, &task, &created); err != nil {
			continue
		}
		found = true
		builder.WriteString(fmt.Sprintf("- [ ] [%d] %s (added %s)\n", id, task, created.Format("Mon 3:04 PM")))
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("listTasks: rows iteration error")
	}
	if !found {
		builder.WriteString("No pending tasks.\n")
	}
	b.sendAcknowledgment(chat, builder.String())
}

// markTaskDone marks a task as done by ID.
func (b *Bot) markTaskDone(chat wtypes.JID, idStr string) {
	id, err := strconv.Atoi(idStr)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Task ID must be a number. List them with `!todo`.")
		return
	}
	result, err := b.SqlDB.Exec("UPDATE tasks SET done = 1 WHERE id = ? AND contact_jid = ? AND done = 0", id, chat.String())
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Couldn't mark that task done.")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		b.sendAcknowledgment(chat, "No unfinished task with that ID, or it belongs to someone else.")
		return
	}
	b.sendAcknowledgment(chat, fmt.Sprintf("✅ Task #%d marked done.", id))
}

// processAITaskTags scans the AI response for [TASK:...] tags, stores tasks, strips the tags.
// Pattern: [TASK:buy groceries]
func (b *Bot) processAITaskTags(chat wtypes.JID, response string, chatID string) (cleaned string, tasksCreated int) {
	lower := strings.ToLower(response)
	if !strings.Contains(lower, "[task:") {
		return response, 0
	}
	count := 0
	for {
		idx := strings.Index(strings.ToLower(response), "[task:")
		if idx == -1 {
			break
		}
		endIdx := strings.Index(response[idx:], "]")
		if endIdx == -1 {
			break
		}
		fullTag := response[idx : idx+endIdx+1]
		payload := strings.TrimSpace(fullTag[len("[task:") : len(fullTag)-1])
		if payload != "" {
			contactJID := chat.String()
			contactName := "you"
			if conv, ok := b.conversations[chatID]; ok && conv.UserName != "" {
				contactName = conv.UserName
			}
			now := time.Now()
			_, err := b.SqlDB.Exec(
				"INSERT INTO tasks (created_at, contact_jid, contact_name, task, created_by_jid, done) VALUES (?,?,?,?,?,?)",
				now, contactJID, contactName, payload, chatID, 0,
			)
			if err == nil {
				count++
				log.Info().Msgf("AI-created task: %q for %s", payload, contactName)
			} else {
				log.Warn().Err(err).Msgf("AI task tag store failed for payload: %q", payload)
			}
		}
		response = response[:idx] + response[idx+endIdx+1:]
		response = strings.TrimSpace(response)
	}
	return response, count
}

// --- NOTE tags ---

// handleNoteCommand stores a note and delivers a confirmation.
// Usage: !note <description>
func (b *Bot) handleNoteCommand(chat wtypes.JID, rawText string) {
	text := strings.TrimSpace(rawText)
	if text == "" {
		b.sendAcknowledgment(chat, "Usage: !note <description>")
		return
	}
	contactJID := chat.String()
	contactName := "you"
	if conv, ok := b.conversations[contactJID]; ok && conv.UserName != "" {
		contactName = conv.UserName
	}
	createdByJID := chat.String()
	now := time.Now()

	_, err := b.SqlDB.Exec(
		"INSERT INTO notes (created_at, contact_jid, contact_name, note, created_by_jid) VALUES (?,?,?,?,?)",
		now, contactJID, contactName, text, createdByJID,
	)
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Failed to store note: "+err.Error())
		return
	}
	b.sendAcknowledgment(chat, fmt.Sprintf("✅ Note saved: %s", text))
}

// listNotes lists notes for the requester, optionally filtered by a search term.
func (b *Bot) listNotes(chat wtypes.JID, searchTerm string) {
	contactJID := chat.String()
	var rows *sql.Rows
	var err error
	if searchTerm != "" {
		rows, err = b.SqlDB.Query(
			"SELECT id, note, created_at FROM notes WHERE contact_jid = ? AND note LIKE ? ORDER BY created_at DESC",
			contactJID, "%"+searchTerm+"%",
		)
	} else {
		rows, err = b.SqlDB.Query(
			"SELECT id, note, created_at FROM notes WHERE contact_jid = ? ORDER BY created_at DESC",
			contactJID,
		)
	}
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Couldn't load notes.")
		return
	}
	defer rows.Close()

	var builder strings.Builder
	if searchTerm != "" {
		builder.WriteString(fmt.Sprintf("📝 *Notes matching \"%s\":*\n\n", searchTerm))
	} else {
		builder.WriteString("📝 *Your Notes:*\n\n")
	}
	found := false
	for rows.Next() {
		var id int
		var note string
		var created time.Time
		if err := rows.Scan(&id, &note, &created); err != nil {
			continue
		}
		found = true
		builder.WriteString(fmt.Sprintf("- [%d] %s (added %s)\n", id, note, created.Format("Mon 3:04 PM")))
	}
	if !found {
		builder.WriteString("No notes found.\n")
	}
	b.sendAcknowledgment(chat, builder.String())
}

// processAINoteTags scans the AI response for [NOTE:...] tags, stores notes, strips the tags.
// Pattern: [NOTE:Wilma prefers morning calls]
func (b *Bot) processAINoteTags(chat wtypes.JID, response string, chatID string) (cleaned string, notesCreated int) {
	lower := strings.ToLower(response)
	if !strings.Contains(lower, "[note:") {
		return response, 0
	}
	count := 0
	for {
		idx := strings.Index(strings.ToLower(response), "[note:")
		if idx == -1 {
			break
		}
		endIdx := strings.Index(response[idx:], "]")
		if endIdx == -1 {
			break
		}
		fullTag := response[idx : idx+endIdx+1]
		payload := strings.TrimSpace(fullTag[len("[note:") : len(fullTag)-1])
		if payload != "" {
			contactJID := chat.String()
			contactName := "you"
			if conv, ok := b.conversations[chatID]; ok && conv.UserName != "" {
				contactName = conv.UserName
			}
			now := time.Now()
			_, err := b.SqlDB.Exec(
				"INSERT INTO notes (created_at, contact_jid, contact_name, note, created_by_jid) VALUES (?,?,?,?,?)",
				now, contactJID, contactName, payload, chatID,
			)
			if err == nil {
				count++
				log.Info().Msgf("AI-created note: %q for %s", payload, contactName)
			} else {
				log.Warn().Err(err).Msgf("AI note tag store failed for payload: %q", payload)
			}
		}
		response = response[:idx] + response[idx+endIdx+1:]
		response = strings.TrimSpace(response)
	}
	return response, count
}

// resolveContactJIDByChat returns the best JID to reach the person in the given chat.
// For 1:1 chats this is the chat JID; for groups it's the chat JID (message the group).
func (b *Bot) resolveContactJIDByChat(chat wtypes.JID, _ string) string {
	return chat.String()
}

// processAIReminderTags scans the AI response for [REMINDER:...] tags, stores reminders,
// strips the tag from the response, and returns the cleaned response.
// Pattern: [REMINDER:at 3pm to call George]  or  [REMINDER:tomorrow at 10am to email Wilma directly]
func (b *Bot) processAIReminderTags(chat wtypes.JID, response string, chatID string) (cleaned string, remindersCreated int) {
	lower := strings.ToLower(response)
	if !strings.Contains(lower, "[reminder:") {
		return response, 0
	}

	count := 0
	for {
		idx := strings.Index(strings.ToLower(response), "[reminder:")
		if idx == -1 {
			break
		}
		endIdx := strings.Index(response[idx:], "]")
		if endIdx == -1 {
			break
		}
		fullTag := response[idx : idx+endIdx+1]
		payload := strings.TrimSpace(fullTag[len("[reminder:") : len(fullTag)-1])

		// Parse the payload the same way as !remind
		now := time.Now()
		fireAt, action, mode, pErr := b.parseReminderText(payload, now)
		if pErr == nil && action != "" {
			contactJID := chat.String()
			contactName := "you"
			// Try to extract a contact name if the action references someone
			contactName = guessContactName(action)
			if err := b.storeReminder(now, fireAt, contactJID, contactName, action, chatID, mode); err == nil {
				b.scheduleReminderCron(fireAt, contactJID, action, mode)
				count++
				log.Info().Msgf("AI-created reminder: fireAt=%s action=%q mode=%s", fireAt.Format(time.RFC3339), action, mode)
			}
		} else {
			log.Warn().Err(pErr).Msgf("AI reminder tag parse failed for payload: %q", payload)
		}

		// Strip the tag
		response = response[:idx] + response[idx+endIdx+1:]
		response = strings.TrimSpace(response)
	}
	return response, count
}

// guessContactName tries to extract a person name from the action string.
// Very simple: if the action looks like "call X" or "email X" or "message X", return X.
func guessContactName(action string) string {
	lower := strings.ToLower(action)
	for _, prefix := range []string{"call ", "email ", "message ", "text ", "remind ", "tell ", "ping ", "notify ", "ask "} {
		if strings.HasPrefix(lower, prefix) {
			rest := strings.TrimPrefix(action, prefix)
			rest = strings.TrimSpace(rest)
			// Take the first word (the name)
			parts := strings.Fields(rest)
			if len(parts) > 0 {
				return parts[0]
			}
		}
	}
	return "you"
}

// triggerDailyReminderCheck runs once per minute to fire any reminders whose time has come.
// It complements the cron-scheduled path (which handles exact cron matches) by catching
// reminders that may have been stored but whose cron job didn't register (e.g. edge cases).
func (b *Bot) triggerDailyReminderCheck() {
	now := time.Now()
	rows, err := b.SqlDB.Query("SELECT id, contact_jid, message, mode FROM reminders WHERE fired = 0 AND fire_at <= ?", now)
	if err != nil {
		log.Error().Err(err).Msg("triggerDailyReminderCheck: query failed")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var contactJID, message, mode string
		if err := rows.Scan(&id, &contactJID, &message, &mode); err != nil {
			continue
		}
		b.fireReminder(contactJID, message, mode)
		// Mark fired
		b.SqlDB.Exec("UPDATE reminders SET fired = 1 WHERE id = ?", id)
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("triggerDailyReminderCheck: rows iteration error")
	}
}

// Reminder summary command: !reminders
func (b *Bot) remindCommand(chat wtypes.JID, text string) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "!reminders" || lower == "!r" {
		b.listReminders(chat)
		return
	}
	// !reminders cancel <id>
	if strings.HasPrefix(lower, "!reminders cancel") || strings.HasPrefix(lower, "!r cancel") {
		parts := strings.Fields(text)
		if len(parts) >= 3 {
			b.cancelReminder(chat, parts[2])
			return
		}
		b.sendAcknowledgment(chat, "Usage: !reminders cancel <id>")
		return
	}
	// Default: show usage
	b.sendAcknowledgment(chat, "Reminder commands:\n- `!remind in 20 minutes to call George`\n- `!remind at 3pm to call George directly`\n- `!remind tomorrow at 10am to email Wilma`\n- `!reminders`  (list pending)\n- `!reminders cancel <id>`")
}

// ---------- Depth conversation detection ----------

// isDepthConversation checks whether the recent message history suggests an open-ended,
// reflective, or substantive conversation rather than a quick practical exchange.
func isDepthConversation(historyMessages []string) bool {
	if len(historyMessages) < 3 {
		return false
	}

	depthCues := []string{
		"how do you", "what do you think", "why", "feel", "feelings", "think about",
		"struggling", "worried", "anxious", "happy", "sad", "upset", "confused",
		"meaning", "purpose", "struggle", "hurts", "hard", "difficult",
		"story", "remember", "back when", "used to", "childhood", "grew up",
		"relationship", "friend", "family", "love", "miss", "lonely",
		"change", "becoming", "growth", "learn", "teaching me", "help me understand",
		"advice", "guidance", "support", "through", "going through",
		"journal", "write", "reflect", "meditation", "therapy",
	}

	depthCount := 0
	for _, msg := range historyMessages {
		lower := strings.ToLower(msg)
		for _, cue := range depthCues {
			if strings.Contains(lower, cue) {
				depthCount++
				break
			}
		}
	}

	return depthCount >= 2
}

// buildContactMemorySection loads this contact's stored notes and pending tasks and
// renders them as a prompt section, so facts the bot recorded via [NOTE:...] and
// commitments recorded via [TASK:...] actually influence future replies.
// Without this read-back, both tags are write-only and the feature is inert.
func (b *Bot) buildContactMemorySection(chatID string) string {
	var sb strings.Builder

	// Notes: durable facts about this person (most recent 15).
	noteRows, err := b.SqlDB.Query(
		"SELECT note FROM notes WHERE contact_jid = ? ORDER BY created_at DESC LIMIT 15",
		chatID,
	)
	if err == nil {
		defer noteRows.Close()
		var notes []string
		for noteRows.Next() {
			var n string
			if noteRows.Scan(&n) == nil && strings.TrimSpace(n) != "" {
				notes = append(notes, strings.TrimSpace(n))
			}
		}
		if err := noteRows.Err(); err != nil {
			log.Warn().Err(err).Msg("buildContactMemorySection: noteRows iteration error")
		}
		if len(notes) > 0 {
			sb.WriteString("\n### WHAT YOU REMEMBER ABOUT THIS PERSON (saved notes):\n")
			for i := len(notes) - 1; i >= 0; i-- { // oldest-first reads more naturally
				fmt.Fprintf(&sb, "- %s\n", notes[i])
			}
			sb.WriteString("*Use these naturally when relevant. Do NOT recite the list or announce that you have notes.*\n")
		}
	}

	// Tasks: open commitments (most recent 10, oldest first).
	taskRows, err := b.SqlDB.Query(
		"SELECT task FROM tasks WHERE contact_jid = ? AND done = 0 ORDER BY created_at ASC LIMIT 10",
		chatID,
	)
	if err == nil {
		defer taskRows.Close()
		var tasks []string
		for taskRows.Next() {
			var t string
			if taskRows.Scan(&t) == nil && strings.TrimSpace(t) != "" {
				tasks = append(tasks, strings.TrimSpace(t))
			}
		}
		if err := taskRows.Err(); err != nil {
			log.Warn().Err(err).Msg("buildContactMemorySection: taskRows iteration error")
		}
		if len(tasks) > 0 {
			sb.WriteString("\n### OPEN TASKS / COMMITMENTS FOR THIS PERSON:\n")
			for _, t := range tasks {
				fmt.Fprintf(&sb, "- [ ] %s\n", t)
			}
			sb.WriteString("*If the person's message relates to one of these, acknowledge it. Do NOT list them unprompted.*\n")
		}
	}

	return sb.String()
}

// enablePersonaHotReload starts a poller that refreshes persona documents when
// their source files change on disk, so soul.md/identity.md/personality.md edits
// apply without a bot restart. truth.md is already re-read per message.
func (b *Bot) enablePersonaHotReload() {
	personaFiles := map[string]string{
		"soul.md":        "personality",
		"personality.md": "personality",
		"identity.md":    "identity",
	}
	// Poll every 30 seconds; stat-only when nothing changed, so cost is negligible.
	if _, err := b.cron.AddFunc("*/30 * * * * *", func() {
		b.vectorStore.ReloadDocuments(personaFiles)
	}); err != nil {
		log.Error().Err(err).Msg("Failed to schedule persona hot reload")
		return
	}
	log.Info().Msg("Persona hot reload enabled (30s poll on soul.md/identity.md/personality.md)")
}

// ---------- structured conversation recaps ----------

// recapThreadIdleMinutes is how long a thread must be silent before it counts as ended.
const recapThreadIdleMinutes = 45

// outageAlertCooldown rate-limits global AI-outage alerts so a sustained outage
// notifies Max once rather than on every failed request.
const outageAlertCooldown = 30 * time.Minute

// enableRecapScheduler runs a periodic sweep that writes a structured recap for
// any conversation that has gone quiet. Thread-end is inferred from inactivity
// because there is no explicit "conversation over" signal in WhatsApp.
func (b *Bot) enableRecapScheduler() {
	if _, err := b.cron.AddFunc("*/10 * * * *", b.sweepEndedThreads); err != nil {
		log.Error().Err(err).Msg("Failed to schedule recap sweep")
		return
	}
	log.Info().Msg("Structured conversation recaps enabled (10-minute sweep)")
}

// sweepEndedThreads finds conversations idle past the threshold and recaps them.
func (b *Bot) sweepEndedThreads() {
	cutoff := time.Now().Add(-recapThreadIdleMinutes * time.Minute)

	b.mutex.RLock()
	type candidate struct {
		chatID   string
		userName string
		msgCount int
	}
	var pending []candidate
	for id, conv := range b.conversations {
		if len(conv.Messages) < 4 {
			continue
		}
		if conv.LastActive.After(cutoff) {
			continue // still active
		}
		pending = append(pending, candidate{chatID: id, userName: conv.UserName, msgCount: len(conv.Messages)})
	}
	b.mutex.RUnlock()

	for _, c := range pending {
		// Skip if we already recapped this exact message count (avoids duplicate
		// recaps every sweep while a thread stays idle).
		var existing int
		b.SqlDB.QueryRow(
			"SELECT COALESCE(MAX(message_count), 0) FROM conversation_recaps WHERE chat_id = ?",
			c.chatID,
		).Scan(&existing)
		if existing >= c.msgCount {
			continue
		}
		b.generateThreadRecap(c.chatID, c.userName)
	}
}

// generateThreadRecap distils the conversation into summary/decisions/commitments/
// open questions and persists it. Best-effort; never blocks the chat path.
func (b *Bot) generateThreadRecap(chatID string, userName string) {
	b.mutex.RLock()
	conv, ok := b.conversations[chatID]
	if !ok || len(conv.Messages) < 4 {
		b.mutex.RUnlock()
		return
	}
	var sb strings.Builder
	for _, m := range conv.Messages {
		who := userName
		if m.Role == "assistant" {
			who = "maximus"
		}
		if who == "" {
			who = "them"
		}
		sb.WriteString(fmt.Sprintf("%s: %s\n", who, CleanResponse(m.Content)))
	}
	msgCount := len(conv.Messages)
	if userName == "" {
		userName = "Unknown"
	}
	b.mutex.RUnlock()

	prompt := fmt.Sprintf(`Summarise this WhatsApp conversation between maximus (an assistant) and %s.

Return EXACTLY these four lines and nothing else:
SUMMARY: <2 sentences describing what was discussed>
DECISIONS: <concrete decisions made, semicolon-separated; or NONE>
COMMITMENTS: <things maximus promised to do, semicolon-separated; or NONE>
OPEN_QUESTIONS: <unresolved questions or follow-ups needed, semicolon-separated; or NONE>

Be factual and terse. Do not invent anything not present in the conversation.

CONVERSATION:
%s`, userName, sb.String())

	resp, _, _, err := ai.MakeAIRequest(prompt, nil, "", DEFAULT_TIMEOUT)
	if err != nil {
		log.Warn().Err(err).Msgf("generateThreadRecap: AI request failed for %s", chatID)
		return
	}

	summary, decisions, commitments, questions := parseRecapResponse(resp)
	if summary == "" && decisions == "" && commitments == "" && questions == "" {
		log.Warn().Msgf("generateThreadRecap: nothing parsed for %s", chatID)
		return
	}

	_, err = b.SqlDB.Exec(
		`INSERT INTO conversation_recaps
			(created_at, chat_id, contact_name, message_count, summary, decisions, commitments, open_questions)
		 VALUES (?,?,?,?,?,?,?,?)`,
		time.Now(), chatID, userName, msgCount, summary, decisions, commitments, questions,
	)
	if err != nil {
		log.Warn().Err(err).Msgf("generateThreadRecap: insert failed for %s", chatID)
		return
	}
	log.Info().Msgf("generateThreadRecap: stored recap for %s (%d messages)", chatID, msgCount)
}

// parseRecapResponse extracts the four labelled lines from the model output.
func parseRecapResponse(resp string) (summary, decisions, commitments, questions string) {
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if strings.EqualFold(s, "NONE") || s == "-" {
			return ""
		}
		return s
	}
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "SUMMARY:"):
			summary = clean(strings.TrimSpace(line[len("SUMMARY:"):]))
		case strings.HasPrefix(upper, "DECISIONS:"):
			decisions = clean(strings.TrimSpace(line[len("DECISIONS:"):]))
		case strings.HasPrefix(upper, "COMMITMENTS:"):
			commitments = clean(strings.TrimSpace(line[len("COMMITMENTS:"):]))
		case strings.HasPrefix(upper, "OPEN_QUESTIONS:"):
			questions = clean(strings.TrimSpace(line[len("OPEN_QUESTIONS:"):]))
		}
	}
	return summary, decisions, commitments, questions
}

// buildRecapSection renders recent thread recaps for a contact into the prompt so
// the bot retains cross-thread continuity beyond the rolling summary.
func (b *Bot) buildRecapSection(chatID string) string {
	rows, err := b.SqlDB.Query(
		"SELECT summary, decisions, commitments, open_questions FROM conversation_recaps WHERE chat_id = ? ORDER BY created_at DESC LIMIT 3",
		chatID,
	)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var sb strings.Builder
	found := false
	for rows.Next() {
		var summary, decisions, commitments, questions string
		if rows.Scan(&summary, &decisions, &commitments, &questions) != nil {
			continue
		}
		if !found {
			sb.WriteString("\n### WHAT CAME OUT OF EARLIER CONVERSATIONS (awareness only — do not announce):\n")
			found = true
		}
		if summary != "" {
			fmt.Fprintf(&sb, "- %s\n", summary)
		}
		if decisions != "" {
			fmt.Fprintf(&sb, "  Decisions: %s\n", decisions)
		}
		if commitments != "" {
			fmt.Fprintf(&sb, "  Promised: %s\n", commitments)
		}
		if questions != "" {
			fmt.Fprintf(&sb, "  Still open: %s\n", questions)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("buildRecapSection: rows iteration error")
	}
	if !found {
		return ""
	}
	sb.WriteString("*Do NOT open with these. Only use them if the current message relates.*\n")
	return sb.String()
}

// ---------- facts about people (RAG beyond Max) ----------

// storeFact records a fact about a named subject (any person, not just Max).
func (b *Bot) storeFact(subject, fact, createdByJID string) error {
	subject = strings.TrimSpace(subject)
	fact = strings.TrimSpace(fact)
	if subject == "" || fact == "" {
		return fmt.Errorf("fact requires both subject and content")
	}
	// Dedupe is enforced by the UNIQUE(subject, fact) constraint, so INSERT OR
	// IGNORE makes this race-safe rather than relying on a check-then-insert.
	res, err := b.SqlDB.Exec(
		"INSERT OR IGNORE INTO facts (created_at, subject, fact, created_by_jid) VALUES (?,?,?,?)",
		time.Now(), subject, fact, createdByJID,
	)
	if err != nil {
		return err
	}
	// RowsAffected == 0 means the fact already existed; not an error.
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		log.Debug().Msgf("fact already known about %s: %q", subject, fact)
	}
	return nil
}

// buildFactsSection pulls facts about this contact (and any facts mentioned in
// this chat) into the prompt so the bot recalls knowledge about people other than Max.
func (b *Bot) buildFactsSection(chatID, contactName string) string {
	var sb strings.Builder
	seen := map[string]bool{}

	collect := func(rows *sql.Rows) {
		for rows.Next() {
			var subject, fact string
			if rows.Scan(&subject, &fact) != nil {
				continue
			}
			key := subject + "|" + fact
			if seen[key] {
				continue
			}
			seen[key] = true
			if sb.Len() == 0 {
				sb.WriteString("\n### FACTS YOU KNOW ABOUT PEOPLE (recall naturally, do not list):\n")
			}
			sb.WriteString(fmt.Sprintf("- %s: %s\n", subject, fact))
		}
	}

	// Facts about the person currently being spoken to.
	if contactName != "" && !strings.EqualFold(contactName, "User") && !strings.EqualFold(contactName, "Someone") {
		if rows, err := b.SqlDB.Query(
			"SELECT subject, fact FROM facts WHERE subject LIKE ? ORDER BY created_at DESC LIMIT 15",
			"%"+contactName+"%",
		); err == nil {
			collect(rows)
			rows.Close()
		}
	}

	// Facts recorded in this chat, regardless of subject.
	if rows, err := b.SqlDB.Query(
		"SELECT subject, fact FROM facts WHERE created_by_jid = ? ORDER BY created_at DESC LIMIT 15",
		chatID,
	); err == nil {
		collect(rows)
		rows.Close()
	}

	return sb.String()
}

// processAIFactTags scans the AI response for [FACT:subject|content] tags and stores
// them. Pattern: [FACT:Wilma|prefers morning calls]
func (b *Bot) processAIFactTags(_ wtypes.JID, response string, chatID string) (cleaned string, factsCreated int) {
	lower := strings.ToLower(response)
	if !strings.Contains(lower, "[fact:") {
		return response, 0
	}
	count := 0
	for {
		idx := strings.Index(strings.ToLower(response), "[fact:")
		if idx == -1 {
			break
		}
		endIdx := strings.Index(response[idx:], "]")
		if endIdx == -1 {
			break // malformed tag with no closer — stop, don't spin
		}
		fullTag := response[idx : idx+endIdx+1]
		payload := strings.TrimSpace(fullTag[len("[fact:") : len(fullTag)-1])

		if parts := strings.SplitN(payload, "|", 2); len(parts) == 2 {
			subject := strings.TrimSpace(parts[0])
			content := strings.TrimSpace(parts[1])
			if subject != "" && content != "" {
				if err := b.storeFact(subject, content, chatID); err == nil {
					count++
					log.Info().Msgf("AI-created fact about %s: %q", subject, content)
				} else {
					log.Warn().Err(err).Msgf("AI fact tag store failed: %q", payload)
				}
			}
		} else {
			log.Warn().Msgf("AI fact tag malformed (expected subject|content): %q", payload)
		}

		response = response[:idx] + response[idx+endIdx+1:]
		response = strings.TrimSpace(response)
	}
	return response, count
}

// handleFactAboutCommand stores a fact about a specific person: !fact about <name> <fact>
func (b *Bot) handleFactAboutCommand(chat wtypes.JID, raw string) {
	rest := strings.TrimSpace(strings.TrimPrefix(raw, "about"))
	parts := strings.SplitN(rest, " ", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		b.sendAcknowledgment(chat, "Usage: !fact about <name> <fact>\nExample: !fact about Wilma prefers morning calls")
		return
	}
	subject := strings.TrimSpace(parts[0])
	content := strings.TrimSpace(parts[1])
	if err := b.storeFact(subject, content, chat.String()); err != nil {
		b.sendAcknowledgment(chat, "❌ Couldn't save that fact: "+err.Error())
		return
	}
	b.sendAcknowledgment(chat, fmt.Sprintf("✅ Noted about %s: %s", subject, content))
}

// listFacts shows stored facts, optionally filtered by subject.
func (b *Bot) listFacts(chat wtypes.JID, subject string) {
	var rows *sql.Rows
	var err error
	if subject != "" {
		rows, err = b.SqlDB.Query(
			"SELECT subject, fact, created_at FROM facts WHERE subject LIKE ? ORDER BY subject, created_at DESC LIMIT 50",
			"%"+subject+"%",
		)
	} else {
		rows, err = b.SqlDB.Query(
			"SELECT subject, fact, created_at FROM facts ORDER BY subject, created_at DESC LIMIT 50",
		)
	}
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Couldn't load facts.")
		return
	}
	defer rows.Close()

	var sb strings.Builder
	if subject != "" {
		sb.WriteString(fmt.Sprintf("🧠 *Facts matching \"%s\":*\n\n", subject))
	} else {
		sb.WriteString("🧠 *Facts about people:*\n\n")
	}
	found := false
	for rows.Next() {
		var subj, fact string
		var created time.Time
		if rows.Scan(&subj, &fact, &created) != nil {
			continue
		}
		found = true
		sb.WriteString(fmt.Sprintf("- *%s*: %s\n", subj, fact))
	}
	if !found {
		sb.WriteString("No facts stored yet. Add one with `!fact about <name> <fact>`.\n")
	}
	b.sendAcknowledgment(chat, sb.String())
}

// checkGlobalOutage alerts Max once when every AI provider is unavailable.
// Distinct from the per-chat HITL alert: this fires on a system-wide outage and
// is rate-limited so a long outage produces one alert, not a stream.
func (b *Bot) checkGlobalOutage() {
	tripped := ai.TrippedProviders()
	if len(tripped) == 0 {
		return
	}

	b.mutex.Lock()
	if time.Now().Before(b.lastOutageAlert.Add(outageAlertCooldown)) {
		b.mutex.Unlock()
		return
	}
	b.lastOutageAlert = time.Now()
	b.mutex.Unlock()

	targetJID, err := wtypes.ParseJID(b.humanAssistantJID)
	if err != nil {
		log.Error().Err(err).Msg("checkGlobalOutage: cannot parse HUMAN_ASSISTANT_JID")
		return
	}
	msg := fmt.Sprintf("⚠️ *AI PROVIDER OUTAGE*\n\nAll configured AI providers are failing or rate-limited.\nTripped: %s\n\nThe bot is falling back where it can but replies may be degraded or delayed. No action needed unless this persists.",
		strings.Join(tripped, ", "))
	if err := b.sendAcknowledgment(targetJID, msg); err != nil {
		log.Error().Err(err).Msg("checkGlobalOutage: failed to alert Max")
		return
	}
	log.Warn().Strs("tripped", tripped).Msg("Global AI outage alert sent to Max")
}

// ---------- implicit user preference learning ----------

// userProfile holds implicitly-learned preferences for one contact.
type userProfile struct {
	CommunicationStyle string
	Interests          string
	Dislikes           string
}

// loadUserProfile reads a learned profile for the given user, if one exists.
func (b *Bot) loadUserProfile(userID string) (userProfile, bool) {
	var p userProfile
	err := b.SqlDB.QueryRow(
		"SELECT communication_style, interests, dislikes FROM user_profiles WHERE user_id = ?",
		userID,
	).Scan(&p.CommunicationStyle, &p.Interests, &p.Dislikes)
	if err != nil {
		return userProfile{}, false
	}
	if p.CommunicationStyle == "" && p.Interests == "" && p.Dislikes == "" {
		return userProfile{}, false
	}
	return p, true
}

// buildProfileSection renders the learned profile as a prompt section.
func (b *Bot) buildProfileSection(userID string) string {
	p, ok := b.loadUserProfile(userID)
	if !ok {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n### LEARNED PREFERENCES FOR THIS PERSON (calibrate your tone, do not recite):\n")
	if p.CommunicationStyle != "" {
		fmt.Fprintf(&sb, "- Communication style: %s\n", p.CommunicationStyle)
	}
	if p.Interests != "" {
		fmt.Fprintf(&sb, "- Cares about: %s\n", p.Interests)
	}
	if p.Dislikes != "" {
		fmt.Fprintf(&sb, "- Dislikes / avoid: %s\n", p.Dislikes)
	}
	return sb.String()
}

// learnUserPreferences runs periodically (not per message) to distil durable
// preferences from recent conversation history. Best-effort: failures are logged
// and never surface to the user.
func (b *Bot) learnUserPreferences(chatID string) {
	b.mutex.RLock()
	conv, ok := b.conversations[chatID]
	if !ok || len(conv.Messages) < 6 {
		b.mutex.RUnlock()
		return
	}
	// Use only the recent slice so the prompt stays small.
	start := 0
	if len(conv.Messages) > 20 {
		start = len(conv.Messages) - 20
	}
	var sb strings.Builder
	for _, m := range conv.Messages[start:] {
		role := "them"
		if m.Role == "assistant" {
			role = "you"
		}
		sb.WriteString(fmt.Sprintf("%s: %s\n", role, CleanResponse(m.Content)))
	}
	userName := conv.UserName
	b.mutex.RUnlock()

	if userName == "" {
		userName = "this person"
	}

	prompt := fmt.Sprintf(`You are analysing a WhatsApp conversation to extract DURABLE preferences about %s (the person, not the assistant).

Return EXACTLY three lines, no preamble, no markdown:
STYLE: <their communication style in under 12 words, or NONE>
INTERESTS: <comma-separated topics they genuinely care about, or NONE>
DISLIKES: <things they dislike or want avoided, or NONE>

Only record what is clearly evidenced in the conversation. Use NONE rather than guessing.

CONVERSATION:
%s`, userName, sb.String())

	resp, _, _, err := ai.MakeAIRequest(prompt, nil, "", DEFAULT_TIMEOUT)
	if err != nil {
		log.Warn().Err(err).Msgf("learnUserPreferences: AI request failed for %s", chatID)
		return
	}

	style, interests, dislikes := parseProfileResponse(resp)
	if style == "" && interests == "" && dislikes == "" {
		return
	}

	_, err = b.SqlDB.Exec(`
		INSERT INTO user_profiles (user_id, display_name, communication_style, interests, dislikes, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			display_name = excluded.display_name,
			communication_style = CASE WHEN excluded.communication_style != '' THEN excluded.communication_style ELSE user_profiles.communication_style END,
			interests = CASE WHEN excluded.interests != '' THEN excluded.interests ELSE user_profiles.interests END,
			dislikes = CASE WHEN excluded.dislikes != '' THEN excluded.dislikes ELSE user_profiles.dislikes END,
			interaction_count = user_profiles.interaction_count + 1,
			updated_at = excluded.updated_at`,
		chatID, userName, style, interests, dislikes, time.Now(),
	)
	if err != nil {
		log.Warn().Err(err).Msgf("learnUserPreferences: upsert failed for %s", chatID)
		return
	}
	log.Info().Msgf("learnUserPreferences: updated profile for %s", chatID)
}

// parseProfileResponse extracts STYLE/INTERESTS/DISLIKES lines from the model output.
func parseProfileResponse(resp string) (style, interests, dislikes string) {
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if strings.EqualFold(s, "NONE") || s == "-" {
			return ""
		}
		return s
	}
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "STYLE:"):
			style = clean(strings.TrimSpace(line[len("STYLE:"):]))
		case strings.HasPrefix(upper, "INTERESTS:"):
			interests = clean(strings.TrimSpace(line[len("INTERESTS:"):]))
		case strings.HasPrefix(upper, "DISLIKES:"):
			dislikes = clean(strings.TrimSpace(line[len("DISLIKES:"):]))
		}
	}
	return style, interests, dislikes
}

// shouldEngageGroup decides whether the bot may respond in a group chat.
// Default is false: groups conflate multiple people into one conversation.
// GROUP_ENGAGE=true enables all groups; GROUP_ALLOWLIST restricts to named ones.
func (b *Bot) shouldEngageGroup(chatJID string) bool {
	if !b.groupEngage {
		return false
	}
	if len(b.groupAllowlist) == 0 {
		return true // engage enabled with no allowlist = all groups
	}
	return b.groupAllowlist[chatJID]
}

// resolveGroupSenderName derives a display name for a message inside a group.
// In groups, Info.Sender is the individual participant while Info.Chat is the
// group itself, so the sender must come from Sender (falling back to SenderAlt
// when the primary address is a LID the contact map doesn't know).
func (b *Bot) resolveGroupSenderName(info wtypes.MessageInfo) string {
	sender := info.Sender
	if sender.User == "" && !info.SenderAlt.IsEmpty() {
		sender = info.SenderAlt
	}
	if info.PushName != "" {
		return info.PushName
	}
	return b.resolveSenderName(sender.String())
}

// ---------- group chat handling ----------

// isGroupChat returns true when the JID ends with @g.us (WhatsApp group JID suffix).
func isGroupChat(jid string) bool {
	return strings.HasSuffix(jid, "@g.us")
}

// ---------- processAIResponse: inject [REMINDER:] tag handling ----------

// enableReminderTagHandling must be called once to register the one-minute reminder check cron.
func (b *Bot) enableReminderTagHandling() {
	if _, err := b.cron.AddFunc("* * * * *", b.triggerDailyReminderCheck); err != nil {
		log.Error().Err(err).Msg("Failed to schedule daily reminder check")
	}
	log.Info().Msg("Reminder tag handling enabled (1-minute check)")
}

// enableTaskNoteTagHandling loads persisted tasks and notes from DB into memory.
// Called once at startup. No cron needed — tasks/notes are user-managed via commands.
func (b *Bot) enableTaskNoteTagHandling() {
	// Verify the tables exist so a schema failure surfaces at startup rather than
	// on the first user command.
	if _, err := b.SqlDB.Query("SELECT COUNT(*) FROM notes"); err != nil {
		log.Error().Err(err).Msg("Task/note tag handling: notes table unavailable")
	}
	if _, err := b.SqlDB.Query("SELECT COUNT(*) FROM tasks"); err != nil {
		log.Error().Err(err).Msg("Task/note tag handling: tasks table unavailable")
	}
	log.Info().Msg("Task/note tag handling enabled (notes/tasks read back into prompt)")
}

// enablePreferenceLearning schedules periodic implicit preference extraction for
// active conversations. Runs hourly; each run is best-effort and never blocks chat.
func (b *Bot) enablePreferenceLearning() {
	if _, err := b.cron.AddFunc("0 * * * *", b.learnAllActiveProfiles); err != nil {
		log.Error().Err(err).Msg("Failed to schedule preference learning")
		return
	}
	log.Info().Msg("Implicit preference learning enabled (hourly)")
}

// learnAllActiveProfiles distils preferences for every conversation with enough history.
func (b *Bot) learnAllActiveProfiles() {
	b.mutex.RLock()
	chatIDs := make([]string, 0, len(b.conversations))
	for id, conv := range b.conversations {
		if len(conv.Messages) >= 6 {
			chatIDs = append(chatIDs, id)
		}
	}
	b.mutex.RUnlock()

	for _, id := range chatIDs {
		b.learnUserPreferences(id)
	}
}

// sendReminderToContact sends a direct reminder message to a specific contact JID.
// Used by !remind <jid> <message> (manual override) and by the cron fire path.
func (b *Bot) sendReminderToContact(targetJID wtypes.JID, message string) error {
	msg := utils.CreateTextMessage(message)
	_, err := b.client.SendMessage(context.Background(), targetJID, msg)
	return err
}

// deleteReminder deletes a reminder by ID.
func (b *Bot) deleteReminder(id int) (bool, error) {
	result, err := b.SqlDB.Exec("DELETE FROM reminders WHERE id = ? AND fired = 0", id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}
