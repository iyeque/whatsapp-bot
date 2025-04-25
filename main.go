package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"whatsapp-gpt-bot/utils"

	"github.com/cenkalti/backoff/v4"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

const (
	LM_STUDIO_URL = "http://localhost:1234/v1/chat/completions"
	MAX_TOKENS    = 500
	MAX_HISTORY   = 10
	LOG_FILE      = "whatsapp-bot.log"
	DB_PATH       = "file:whatsapp.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&cache=shared&mode=rwc"
)

type Message struct {
	Role    string
	Content string
	Time    time.Time
}

type Conversation struct {
	Messages   []Message
	LastActive time.Time
}

type Bot struct {
	client        *whatsmeow.Client
	db            *sqlstore.Container
	conversations map[string]*Conversation
	mutex         sync.RWMutex
}

type LMResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (b *Bot) withRetry(operation func() error) error {
	return utils.WithRetry(operation, utils.DefaultRetryConfig())
}

func (b *Bot) handleMessage(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		if v.Info.IsFromMe {
			return
		}

		err := b.withRetry(func() error {
			chatID := v.Info.Chat.String()
			b.mutex.Lock()
			defer b.mutex.Unlock()

			if _, exists := b.conversations[chatID]; !exists {
				b.conversations[chatID] = &Conversation{
					Messages: make([]Message, 0),
				}
			}
			conv := b.conversations[chatID]
			conv.LastActive = time.Now()
			return nil
		})

		if err != nil {
			fmt.Printf("Error handling message: %v\n", err)
			return
		}

		// Handle different message types
		switch {
		case v.Message.GetConversation() != "":
			go b.handleTextMessage(v, v.Info.Chat.String())
		case v.Message.GetImageMessage() != nil:
			go b.handleImageMessage(v)
		case v.Message.GetDocumentMessage() != nil:
			go b.handleDocumentMessage(v)
		}

		// Send typing notification
		err = b.client.SendChatPresence(v.Info.Chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)
		if err != nil {
			fmt.Printf("Error sending chat presence: %v\n", err)
		}

	case *events.JoinedGroup:
		fmt.Printf("Joined group: %s\n", v.JID)
	case *events.GroupInfo:
		fmt.Printf("Group info updated: %s\n", v.JID)
	case *events.Receipt:
		if v.Type == events.ReceiptTypeRetry {
			// Ignore retry receipts as we cannot directly handle them
			fmt.Printf("Received retry receipt from %s\n", v.Sender)
		}
	case *events.AppState:
		b.handleAppState(v)
	}
}

func (b *Bot) handleAppState(evt *events.AppState) {
	// Handle app state changes (contacts, settings, etc)
	fmt.Printf("App state changed: %s\n", evt.Index)
}

func (b *Bot) handleTextMessage(msg *events.Message, chatID string) {
	userMsg := msg.Message.GetConversation()
	if userMsg == "" {
		return
	}

	b.mutex.Lock()
	conv := b.conversations[chatID]
	conv.Messages = append(conv.Messages, Message{
		Role:    "user",
		Content: userMsg,
		Time:    time.Now(),
	})

	// Create a copy of messages for processing
	messages := make([]map[string]string, len(conv.Messages))
	for i, msg := range conv.Messages {
		messages[i] = map[string]string{
			"role":    msg.Role,
			"content": msg.Content,
		}
	}
	b.mutex.Unlock()

	reqBody := map[string]interface{}{
		"messages":   messages,
		"max_tokens": MAX_TOKENS,
		"model":      "local-model",
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		fmt.Printf("Error marshaling request: %v\n", err)
		return
	}

	resp, err := http.Post(LM_STUDIO_URL, "application/json", strings.NewReader(string(jsonData)))
	if err != nil {
		fmt.Printf("Error making request to LM Studio: %v\n", err)
		return
	}
	defer resp.Body.Close()

	var lmResp LMResponse
	if err := json.NewDecoder(resp.Body).Decode(&lmResp); err != nil {
		fmt.Printf("Error decoding response: %v\n", err)
		return
	}

	if len(lmResp.Choices) > 0 {
		response := lmResp.Choices[0].Message.Content

		b.mutex.Lock()
		conv.Messages = append(conv.Messages, Message{
			Role:    "assistant",
			Content: response,
			Time:    time.Now(),
		})

		if len(conv.Messages) > MAX_HISTORY {
			conv.Messages = conv.Messages[len(conv.Messages)-MAX_HISTORY:]
		}
		b.mutex.Unlock()

		replyMsg := utils.CreateTextMessage(response)

		ctx := context.Background()
		_, err = b.client.SendMessage(ctx, msg.Info.Chat, replyMsg)
		if err != nil {
			fmt.Printf("Error sending message: %v\n", err)
			return
		}

		if err := b.client.MarkRead([]string{msg.Info.ID}, time.Now(), msg.Info.Chat, msg.Info.Sender); err != nil {
			fmt.Printf("Error marking message as read: %v\n", err)
		}
	}
}

func (b *Bot) sendAcknowledgment(chat types.JID, text string) error {
	msg := utils.CreateTextMessage(text)
	_, err := b.client.SendMessage(context.Background(), chat, msg)
	return err
}

func (b *Bot) SendMediaMessage(chat types.JID, caption string, data []byte, mediaType string) error {
	fmt.Println("Uploading media...")
	uploaded, err := b.client.Upload(context.Background(), data, whatsmeow.MediaType(mediaType))
	if err != nil {
		return fmt.Errorf("failed to upload media: %v", err)
	}

	msg := utils.CreateImageMessage(caption, uploaded, data, mediaType)
	fmt.Println("Sending media message...")
	_, err = b.client.SendMessage(context.Background(), chat, msg)
	return err
}

func (b *Bot) SendStatusMessage(text string) error {
	msg := utils.CreateTextMessage(text)
	_, err := b.client.SendMessage(context.Background(), types.StatusBroadcastJID, msg)
	return err
}

func (b *Bot) SendStatusImage(caption string, data []byte) error {
	uploaded, err := b.client.Upload(context.Background(), data, whatsmeow.MediaImage)
	if err != nil {
		return err
	}

	msg := utils.CreateImageMessage(caption, uploaded, data, "image/jpeg")
	_, err = b.client.SendMessage(context.Background(), types.StatusBroadcastJID, msg)
	return err
}

func (b *Bot) handleImageMessage(msg *events.Message) {
	img := msg.Message.GetImageMessage()
	if img != nil {
		data, err := b.client.Download(img)
		if err != nil {
			fmt.Printf("Error downloading image: %v\n", err)
			return
		}

		if err := b.sendAcknowledgment(msg.Info.Chat, "✅ Image received"); err != nil {
			fmt.Printf("Error sending image acknowledgment: %v\n", err)
		}
		fmt.Printf("Received image of size: %d bytes\n", len(data))
	}
}

func (b *Bot) handleDocumentMessage(msg *events.Message) {
	doc := msg.Message.GetDocumentMessage()
	if doc != nil {
		data, err := b.client.Download(doc)
		if err != nil {
			fmt.Printf("Error downloading document: %v\n", err)
			return
		}

		if err := b.sendAcknowledgment(msg.Info.Chat, "✅ Document received: "+doc.GetFileName()); err != nil {
			fmt.Printf("Error sending document acknowledgment: %v\n", err)
		}
		fmt.Printf("Received document: %s, size: %d bytes\n", doc.GetFileName(), len(data))
	}
}

// Presence and Chat State Functions
type PresenceType types.Presence

const (
	PresenceAvailable   = PresenceType(types.PresenceAvailable)
	PresenceUnavailable = PresenceType(types.PresenceUnavailable)
)

func (b *Bot) SetPresence(presence PresenceType) error {
	return b.client.SendPresence(types.Presence(presence))
}

func (b *Bot) SubscribePresence(jid types.JID) error {
	return b.client.SubscribePresence(jid)
}

// Initialize event handlers in NewBot
func NewBot(client *whatsmeow.Client, db *sqlstore.Container) *Bot {
	bot := &Bot{
		client:        client,
		db:            db,
		conversations: make(map[string]*Conversation),
	}

	client.AddEventHandler(bot.handleMessage)
	client.AddEventHandler(bot.handlePresence)

	return bot
}

func (b *Bot) handlePresence(evt interface{}) {
	switch v := evt.(type) {
	case *events.Presence:
		fmt.Printf("Presence update from %s: %t\n", v.From, v.Unavailable)
	}
}

func (b *Bot) startPresenceKeeper(ctx context.Context) {
	// Maintain online presence by periodically refreshing status
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := b.SetPresence(PresenceAvailable); err != nil {
					fmt.Printf("Failed to refresh online presence: %v\n", err)
				}
			case <-ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()
}

func setupLogging() (*os.File, error) {
	logPath := filepath.Join("logs", LOG_FILE)
	// Create logs directory if it doesn't exist
	if err := os.MkdirAll("logs", 0755); err != nil {
		return nil, err
	}

	// Open log file with append mode
	return os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}

func main() {
	fmt.Println("Starting WhatsApp bot...")

	logFile, err := setupLogging()
	if err != nil {
		fmt.Printf("Failed to set up logging: %v\n", err)
		return
	}
	defer logFile.Close()

	// Create logger using Stdout method with more verbose output
	logger := waLog.Stdout("Bot", "DEBUG", true)
	fmt.Println("Logger initialized...")

	// Use background context instead of timeout
	ctx := context.Background()

	connectionStatus := make(chan string, 1)
	fmt.Println("Setting up database connection...")

	// Create container with retry mechanism and timeout
	var container *sqlstore.Container
	backoffConfig := backoff.NewExponentialBackOff()
	backoffConfig.MaxElapsedTime = 25 * time.Second
	backoffConfig.InitialInterval = 1 * time.Second // Increased initial interval

	err = backoff.Retry(func() error {
		var err error
		container, err = sqlstore.New("sqlite", DB_PATH, logger)
		if err != nil {
			fmt.Printf("Database connection attempt failed: %v\n", err)
			// Check if this is a foreign key error
			if strings.Contains(err.Error(), "foreign keys") {
				// Force close any existing connections
				if container != nil {
					container.Close()
				}
			}
			return err
		}
		return nil
	}, backoffConfig)

	if err != nil {
		logger.Errorf("Failed to connect to database after retries: %v", err)
		return
	}
	fmt.Println("Database connection established")

	// Setup WhatsApp client with proper error handling
	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		if strings.Contains(err.Error(), "no device") {
			fmt.Println("No existing device found - starting new session")
			deviceStore = container.NewDevice()
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	client := whatsmeow.NewClient(deviceStore, logger)
	bot := NewBot(client, container)

	// Use bot instance for connection handling
	bot.client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Connected:
			fmt.Println("WhatsApp connection established!")
			// Set bot as online when connected and start presence keeper
			if err := bot.SetPresence(PresenceAvailable); err != nil {
				logger.Errorf("Failed to set online presence: %v", err)
			}
			bot.startPresenceKeeper(ctx)
			connectionStatus <- "connected"
		case *events.ConnectFailure:
			fmt.Printf("Connection failed: %v\n", v.Reason)
			connectionStatus <- "failed"
		}
	})

	// Connect with proper error handling
	fmt.Println("Attempting to connect to WhatsApp...")
	if err := client.Connect(); err != nil {
		logger.Errorf("Failed to start connection: %v", err)
		return
	}

	// Setup clean shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	// Block until interrupt received
	select {
	case <-c:
		logger.Infof("Interrupt received, shutting down...")
		client.Disconnect()
	case <-ctx.Done():
		logger.Errorf("Global timeout reached")
		client.Disconnect()
	}
}
