
package whatsapp

import (
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	"golang.org/x/time/rate"
)

// AccountManager handles multiple WhatsApp bot instances
type AccountManager struct {
	container *sqlstore.Container
	bots      map[string]*Bot
	logger    waLog.Logger
	mutex     sync.RWMutex
	
	// Rate limiting and priority management
	priorityQueues map[string]chan *BotRequest
	rateLimiters  map[string]*rate.Limiter
}

// BotRequest represents a request to the bot
type BotRequest struct {
	Message   string
	ChatID    string
	Priority  int
	Timestamp time.Time
}

// NewAccountManager creates a new account manager
func NewAccountManager(dbPath string, logger waLog.Logger) (*AccountManager, error) {
	container, err := sqlstore.New("sqlite", dbPath, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create store: %v", err)
	}

	return &AccountManager{
		container:      container,
		bots:           make(map[string]*Bot),
		logger:         logger,
		priorityQueues: make(map[string]chan *BotRequest),
		rateLimiters:   make(map[string]*rate.Limiter),
	}, nil
}

// CreateNewBot creates a new bot instance
func (am *AccountManager) CreateNewBot() (*Bot, error) {
	deviceStore := am.container.NewDevice()
	client := whatsmeow.NewClient(deviceStore, am.logger)
	bot := NewBot(client, am.container)

	am.mutex.Lock()
	botID := fmt.Sprintf("bot_%d", len(am.bots)+1)
	am.bots[botID] = bot
	am.mutex.Unlock()

	return bot, nil
}

// ListBots returns all active bot instances
func (am *AccountManager) ListBots() map[string]*Bot {
	am.mutex.RLock()
	defer am.mutex.RUnlock()

	// Create a copy to avoid concurrent access issues
	bots := make(map[string]*Bot)
	for id, bot := range am.bots {
		bots[id] = bot
	}
	return bots
}

// GetBot retrieves a specific bot by ID
func (am *AccountManager) GetBot(botID string) (*Bot, bool) {
	am.mutex.RLock()
	defer am.mutex.RUnlock()
	bot, exists := am.bots[botID]
	return bot, exists
}

// DisconnectAll safely disconnects all bots
func (am *AccountManager) DisconnectAll() {
	am.mutex.Lock()
	defer am.mutex.Unlock()

	for _, bot := range am.bots {
		if bot.client != nil {
			bot.client.Disconnect()
		}
	}
}

// RemoveBot disconnects and removes a bot instance
func (am *AccountManager) RemoveBot(botID string) error {
	am.mutex.Lock()
	defer am.mutex.Unlock()

	bot, exists := am.bots[botID]
	if !exists {
		return fmt.Errorf("bot %s not found", botID)
	}

	if bot.client != nil {
		bot.client.Disconnect()
	}
	delete(am.bots, botID)
	return nil
}

// Close closes the account manager and all associated resources
func (am *AccountManager) Close() error {
	am.DisconnectAll()
	return am.container.Close()
}
