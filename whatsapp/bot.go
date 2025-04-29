package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"

	"whatsapp-gpt-bot/utils"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type BotMessage struct {
	Role    string
	Content string
	Time    time.Time
}

type Conversation struct {
	Messages   []BotMessage
	LastActive time.Time
}

type CacheEntry struct {
	Response  string
	Timestamp time.Time
	UseCount  int
}

type Bot struct {
	client        *whatsmeow.Client
	db            *sqlstore.Container
	conversations map[string]*Conversation
	cache         map[string]*CacheEntry
	timeouts      *TimeoutManager
	mutex         sync.RWMutex
	cacheMux      sync.RWMutex
	qrMux         sync.Mutex
}

type TimeoutManager struct {
	averageResponseTime time.Duration
	mutex               sync.RWMutex
}

func NewBot(client *whatsmeow.Client, db *sqlstore.Container) *Bot {
	bot := &Bot{
		client:        client,
		db:            db,
		conversations: make(map[string]*Conversation),
		cache:         make(map[string]*CacheEntry),
		timeouts:      &TimeoutManager{},
	}

	client.AddEventHandler(bot.handleMessage)
	client.AddEventHandler(bot.handleQREvent)
	go bot.cleanupCache()

	return bot
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

const (
	LM_STUDIO_URL   = "http://localhost:1234/v1/chat/completions"
	MAX_TOKENS      = 500
	MAX_HISTORY     = 10
	DEFAULT_TIMEOUT = 30 * time.Second
	MIN_TIMEOUT     = 10 * time.Second
	INITIAL_TIMEOUT = 15 * time.Second
	MAX_TIMEOUT     = 60 * time.Second
	MAX_RETRIES     = 2
)

type LMResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (b *Bot) handleQREvent(evt interface{}) {
	b.qrMux.Lock()
	defer b.qrMux.Unlock()

	if qrEvt, ok := evt.(*events.QR); ok {
		qrCode, _ := qrcode.New(qrEvt.Codes[0], qrcode.Medium)
		fmt.Printf("\n\x1b[36m╔══════════════════════════════════╗\n║          SCAN QR CODE          ║\n╚══════════════════════════════════╝\n\x1b[0m\n%s\n\x1b[36mScan this QR code with your WhatsApp mobile app\x1b[0m\n\n", qrCode.ToSmallString(false))
	}
}

func (b *Bot) handleMessage(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		if v.Info.IsFromMe {
			return
		}

		chatID := v.Info.Chat.String()
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
			fmt.Printf("Error handling message: %v\n", err)
			return
		}

		switch {
		case v.Message.GetConversation() != "":
			go b.handleTextMessage(v, chatID)
		case v.Message.GetImageMessage() != nil:
			go b.handleImageMessage(v)
		case v.Message.GetDocumentMessage() != nil:
			go b.handleDocumentMessage(v)
		}

		err := b.client.SendChatPresence(v.Info.Chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)
		if err != nil {
			fmt.Printf("Error sending chat presence: %v\n", err)
		}
	}
}

func (b *Bot) initConversation(chatID string) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if _, exists := b.conversations[chatID]; !exists {
		b.conversations[chatID] = &Conversation{
			Messages: make([]BotMessage, 0),
		}
	}
	b.conversations[chatID].LastActive = time.Now()
	return nil
}

func (b *Bot) handleTextMessage(msg *events.Message, chatID string) {
	start := time.Now()
	utils.IncrementRequests()
	defer func() {
		utils.RecordLatency(time.Since(start))
	}()

	userMsg := msg.Message.GetConversation()
	if userMsg == "" {
		return
	}

	b.client.SendChatPresence(msg.Info.Chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)

	if cachedResp, found := b.getCachedResponse(userMsg); found {
		utils.IncrementCacheHit()
		if err := b.sendAcknowledgment(msg.Info.Chat, cachedResp); err == nil {
			return
		}
	}
	utils.IncrementCacheMiss()

	// Add message to batch queue
	b.mutex.Lock()
	if _, exists := b.conversations[chatID]; !exists {
		b.conversations[chatID] = &Conversation{
			Messages: make([]BotMessage, 0),
		}
	}
	b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, BotMessage{
		Role:    "user",
		Content: userMsg,
		Time:    time.Now(),
	})
	b.mutex.Unlock()

	// Process batch after short delay
	time.AfterFunc(500*time.Millisecond, func() {
		b.processBatch(chatID)
	})
}

func (b *Bot) processBatch(chatID string) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	conv, exists := b.conversations[chatID]
	if !exists || len(conv.Messages) == 0 {
		return
	}

	// Get all unprocessed messages
	var messages []BotMessage
	for _, msg := range conv.Messages {
		if msg.Role == "user" {
			messages = append(messages, msg)
		}
	}

	if len(messages) == 0 {
		return
	}

	timeout := b.timeouts.getOptimalTimeout()
	var response string
	var tokens int
	var latency time.Duration
	var err error

	for retries := 0; retries <= MAX_RETRIES; retries++ {
		if retries > 0 {
			timeout = time.Duration(float64(timeout) * 1.5)
			if timeout > MAX_TIMEOUT {
				timeout = MAX_TIMEOUT
			}
			jid, err := types.ParseJID(chatID)
			if err != nil {
				fmt.Printf("Error parsing JID: %v\n", err)
				return
			}
			b.sendAcknowledgment(jid, fmt.Sprintf("Retrying with longer timeout (%ds)...", int(timeout.Seconds())))
		}

		response, tokens, latency, err = b.makeAIRequest(messages[len(messages)-1].Content, chatID, timeout)
		if err == nil {
			utils.RecordTimeout(true)
			utils.RecordLMStudioMetrics(latency, tokens)
			break
		}

		if retries == MAX_RETRIES || !isTimeoutError(err) {
			utils.RecordTimeout(false)
			utils.IncrementFailedRequest()
			errorMsg := "I'm having trouble processing your request right now. Please try again."
			if isTimeoutError(err) {
				errorMsg = "The response is still taking too long. Please try a shorter message."
			}
			jid, err := types.ParseJID(chatID)
			if err != nil {
				fmt.Printf("Error parsing JID: %v\n", err)
				return
			}
			b.sendAcknowledgment(jid, errorMsg)
			return
		}
	}

	b.cacheResponse(messages[len(messages)-1].Content, response)
	b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, BotMessage{
		Role:    "assistant",
		Content: response,
		Time:    time.Now(),
	})

	replyMsg := utils.CreateTextMessage(response)
	jid, err := types.ParseJID(chatID)
	if err != nil {
		fmt.Printf("Error parsing JID: %v\n", err)
		return
	}
	if _, err := b.client.SendMessage(context.Background(), jid, replyMsg); err != nil {
		fmt.Printf("Error sending message: %v\n", err)
		return
	}

	go func() {
		jid, err := types.ParseJID(chatID)
		if err != nil {
			fmt.Printf("Error parsing JID: %v\n", err)
			return
		}
		if err := b.client.MarkRead([]string{messages[len(messages)-1].Content}, time.Now(), jid, jid); err != nil {
			fmt.Printf("Error marking message as read: %v\n", err)
		}
	}()
}

func (b *Bot) makeAIRequest(userMsg, chatID string, timeout time.Duration) (string, int, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	respChan := make(chan struct {
		content string
		tokens  int
		latency time.Duration
	}, 1)
	errChan := make(chan error, 1)

	lmStart := time.Now()

	go func() {
		b.mutex.Lock()
		conv := b.conversations[chatID]
		if len(conv.Messages) > MAX_HISTORY {
			conv.Messages = conv.Messages[len(conv.Messages)-MAX_HISTORY:]
		}

		conv.Messages = append(conv.Messages, BotMessage{
			Role:    "user",
			Content: userMsg,
			Time:    time.Now(),
		})

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
			"stream":     false,
		}

		jsonData, err := json.Marshal(reqBody)
		if err != nil {
			errChan <- err
			return
		}

		req, err := http.NewRequestWithContext(ctx, "POST", LM_STUDIO_URL, strings.NewReader(string(jsonData)))
		if err != nil {
			errChan <- err
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errChan <- err
			return
		}
		defer resp.Body.Close()

		latency := time.Since(lmStart)
		b.timeouts.updateResponseTime(latency)

		var lmResp LMResponse
		if err := json.NewDecoder(resp.Body).Decode(&lmResp); err != nil {
			errChan <- err
			return
		}

		if len(lmResp.Choices) > 0 {
			tokens := len(strings.Split(lmResp.Choices[0].Message.Content, " "))
			respChan <- struct {
				content string
				tokens  int
				latency time.Duration
			}{
				content: lmResp.Choices[0].Message.Content,
				tokens:  tokens,
				latency: latency,
			}
		} else {
			errChan <- fmt.Errorf("no response from AI")
		}
	}()

	select {
	case resp := <-respChan:
		return resp.content, resp.tokens, resp.latency, nil
	case err := <-errChan:
		return "", 0, 0, err
	case <-ctx.Done():
		return "", 0, 0, ctx.Err()
	}
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

func (b *Bot) sendAcknowledgment(chat types.JID, text string) error {
	msg := utils.CreateTextMessage(text)
	_, err := b.client.SendMessage(context.Background(), chat, msg)
	return err
}

func (b *Bot) cleanupCache() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		b.cacheMux.Lock()
		now := time.Now()
		for key, entry := range b.cache {
			if now.Sub(entry.Timestamp) > 24*time.Hour || entry.UseCount < 2 {
				delete(b.cache, key)
			}
		}
		b.cacheMux.Unlock()
	}
}

func (b *Bot) getCachedResponse(query string) (string, bool) {
	b.cacheMux.RLock()
	defer b.cacheMux.RUnlock()

	if entry, exists := b.cache[query]; exists {
		if time.Since(entry.Timestamp) < 24*time.Hour {
			entry.UseCount++
			return entry.Response, true
		}
	}
	return "", false
}

func (b *Bot) cacheResponse(query, response string) {
	b.cacheMux.Lock()
	defer b.cacheMux.Unlock()

	b.cache[query] = &CacheEntry{
		Response:  response,
		Timestamp: time.Now(),
		UseCount:  1,
	}
}

func (tm *TimeoutManager) updateResponseTime(duration time.Duration) {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()

	if tm.averageResponseTime == 0 {
		tm.averageResponseTime = duration
	} else {
		tm.averageResponseTime = time.Duration(float64(tm.averageResponseTime)*0.7 + float64(duration)*0.3)
	}
}

func (tm *TimeoutManager) getOptimalTimeout() time.Duration {
	tm.mutex.RLock()
	defer tm.mutex.RUnlock()

	if tm.averageResponseTime == 0 {
		return INITIAL_TIMEOUT
	}

	timeout := tm.averageResponseTime * 2

	if timeout < INITIAL_TIMEOUT {
		return INITIAL_TIMEOUT
	}
	if timeout > MAX_TIMEOUT {
		return MAX_TIMEOUT
	}
	return timeout
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	return err == context.DeadlineExceeded || strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline exceeded")
}
