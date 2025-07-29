
package whatsapp

import (
	"context"
	"encoding/json"
	"github.com/skip2/go-qrcode"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"whatsapp-gpt-bot/cache"
	"whatsapp-gpt-bot/queue"
	"whatsapp-gpt-bot/types"
	"whatsapp-gpt-bot/utils"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	wtypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type BotMessage struct {
	Role    string
	Content string
	Time    time.Time
}

type Conversation struct {
	Messages   []BotMessage
	LastActive time.Time
	Summary    string
}

type CacheEntry struct {
	Response  string
	Timestamp time.Time
	UseCount  int
}

type TimeoutManager struct {
	responseTimes     []time.Duration
	timeoutCount      int
	mutex             sync.RWMutex
	averageResponseTime time.Duration
}

type CachedResponse struct {
	Content  string
	Tokens   int
	Latency  time.Duration
	Timestamp time.Time
}

type Bot struct {
	client        *whatsmeow.Client
	db            *sqlstore.Container
	conversations map[string]*Conversation
	cache         *cache.Cache
	timeouts      *TimeoutManager
	messageQueue  *queue.Queue
	mutex         sync.RWMutex
	qrMux         sync.Mutex
	cacheMux      sync.RWMutex
	responseCache map[string]CachedResponse
	rateLimiter   *RateLimiter
	accountManager *AccountManager
	botID         string
}

func NewBot(client *whatsmeow.Client, db *sqlstore.Container, am *AccountManager, id string) *Bot {
	bot := &Bot{
		client:         client,
		db:             db,
		conversations:  make(map[string]*Conversation),
		cache:          cache.NewCache(1000),
		timeouts:       &TimeoutManager{},
		messageQueue:   queue.NewQueue(10, 5, 5*time.Second),
		responseCache:  make(map[string]CachedResponse),
		rateLimiter:    NewRateLimiter(0.5, 1), // Allow 1 request every 2 seconds
		accountManager: am,
		botID:          id,
	}

	// Register event handlers
	client.AddEventHandler(bot.handleMessage)
	client.AddEventHandler(bot.handleQREvent)
	client.AddEventHandler(bot.handleLoggedOut)

	// Start cache cleanup routine
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

func (b *Bot) decodeAndSaveQR(qr string) {
	qrCode, _ := qrcode.New(qr, qrcode.Medium)
	fmt.Printf("\n\x1b[36m╔══════════════════════════════════╗\n║          SCAN QR CODE          ║\n╚══════════════════════════════════╝\n\x1b[0m\n%s\n\x1b[36mScan this QR code with your WhatsApp mobile app\x1b[0m\n\n", qrCode.ToSmallString(false))
}

const (
	LM_STUDIO_URL   = "http://localhost:1234/v1/chat/completions"
	MAX_TOKENS      = 500
	MAX_HISTORY     = 10
	DEFAULT_TIMEOUT = 300 * time.Second
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
		b.decodeAndSaveQR(qrEvt.Codes[0])
	}
}

func (b *Bot) handleLoggedOut(evt interface{}) {
	if _, ok := evt.(*events.LoggedOut); ok {
		b.accountManager.RemoveBot(b.botID)
	}
}

func (b *Bot) handleMessage(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		// Ignore messages from self or status updates
        if v.Info.Chat.String() != "" && v.Info.Sender.String() != "" {
            fmt.Printf("Received message from Chat: %v, Sender: %v\n", v.Info.Chat, v.Info.Sender)
        }

		// Get the bot's own JID. It might be nil if we're not connected yet.
		botJID := b.client.Store.ID
		if botJID == nil {
			return
		}

		// The main filter logic:
		// - Ignore group messages
		// - Ignore poll updates
		// - Ignore messages from self, UNLESS the chat is with self (which is the case for business accounts)
        if v.Info.IsGroup || v.Message.GetPollUpdateMessage() != nil || (v.Info.IsFromMe && v.Info.Chat.String() != botJID.String()) {
			return
		}

		// Rate limit messages
		if !b.rateLimiter.Allow(v.Info.Sender.String()) {
			b.sendAcknowledgment(v.Info.Chat, "You are sending messages too fast. Please wait a moment.")
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
		case v.Message.GetTemplateButtonReplyMessage() != nil:
			// Handle template button replies
			v.Message.Conversation = proto.String(v.Message.GetTemplateButtonReplyMessage().GetSelectedID())
			go b.handleTextMessage(v, chatID)
		}

		err := b.client.SendChatPresence(v.Info.Chat, wtypes.ChatPresenceComposing, wtypes.ChatPresenceMediaText)
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
	var retrySuccess bool
defer func() {
	utils.RecordLatency(time.Since(start))
	utils.RecordTimeout(retrySuccess)
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

	b.client.SendChatPresence(msg.Info.Chat, wtypes.ChatPresenceComposing, wtypes.ChatPresenceMediaText)

	if cachedResp, found := b.getCachedResponse(userMsg); found {
		utils.IncrementCacheHit()
		if err := b.sendAcknowledgment(msg.Info.Chat, cachedResp); err == nil {
			return
		}
	}
	utils.IncrementCacheMiss()

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
			b.sendAcknowledgment(msg.Info.Chat, fmt.Sprintf("Retrying with longer timeout (%ds)...", int(timeout.Seconds())))
		}

		response, tokens, latency, err = b.makeAIRequest(userMsg, chatID, timeout)
		if err == nil {
			utils.RecordTimeout(true)
			utils.RecordLMStudioMetrics(latency, tokens)
			break
		}

		if retries == MAX_RETRIES || !isTimeoutError(err) {
			retrySuccess = true
		utils.RecordTimeout(true)
			utils.IncrementFailedRequest()
			errorMsg := "I'm having trouble processing your request right now. Please try again."
			if isTimeoutError(err) {
				errorMsg = "The response is still taking too long. Please try a shorter message."
			}
			b.sendAcknowledgment(msg.Info.Chat, errorMsg)
			return
		}
	}

	b.cacheResponse(userMsg, response)
	b.mutex.Lock()
	b.conversations[chatID].Messages = append(b.conversations[chatID].Messages, BotMessage{
		Role:    "assistant",
		Content: response,
		Time:    time.Now(),
	})
	b.mutex.Unlock()

	replyMsg := utils.CreateTextMessage(response)
	if _, err := b.client.SendMessage(context.Background(), msg.Info.Chat, replyMsg); err != nil {
		fmt.Printf("Error sending message: %v\n", err)
		return
	}

	go func() {
		if err := b.client.MarkRead([]string{msg.Info.ID}, time.Now(), msg.Info.Chat, msg.Info.Sender); err != nil {
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

		// Summarize conversation if it's too long
		if len(conv.Messages) > MAX_HISTORY {
			go b.summarizeConversation(chatID)
		}

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
		// Store response before returning
		b.mutex.Lock()
		b.responseCache[chatID] = CachedResponse{
			Content:   resp.content,
			Tokens:    resp.tokens,
			Latency:   resp.latency,
			Timestamp: time.Now(),
		}
		b.mutex.Unlock()
		return resp.content, resp.tokens, resp.latency, nil
	case err := <-errChan:
		return "", 0, 0, err
	// unreachable code removed or refactored as per warning at lines 134 and 136-173
	case <-ctx.Done():
		b.timeouts.recordTimeout()
		return "", 0, 0, ctx.Err()
	}
}

func (b *Bot) makeIndependentAIRequest(prompt string, timeout time.Duration) (string, int, time.Duration, error) {
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
		messages := []map[string]string{
			{"role": "user", "content": prompt},
		}

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
		b.timeouts.recordTimeout()
		return "", 0, 0, ctx.Err()
	}
}

func (b *Bot) handleImageMessage(msg *events.Message) {
	img := msg.Message.GetImageMessage()
	if img != nil {
		data, err := b.client.Download(context.Background(), img)
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
		data, err := b.client.Download(context.Background(), doc)
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

func (b *Bot) sendAcknowledgment(chat wtypes.JID, text string) error {
	msg := utils.CreateTextMessage(text)
	_, err := b.client.SendMessage(context.Background(), chat, msg)
	return err
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

func (tm *TimeoutManager) updateResponseTime(duration time.Duration) {
	// Add mutex to TimeoutManager struct if not present
	// var mutex sync.RWMutex
	// Add averageResponseTime field if not present
	// var averageResponseTime time.Duration
	// Implementation below assumes these fields exist
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

func (tm *TimeoutManager) recordTimeout() {
	tm.mutex.Lock()
	defer tm.mutex.Unlock()
	tm.timeoutCount++
}

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
		summary, _, _, err := b.makeIndependentAIRequest(prompt, DEFAULT_TIMEOUT)
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
		conv.Messages = conv.Messages[len(conv.Messages)-2:] // Keep the last 2 messages for context
	}()
}