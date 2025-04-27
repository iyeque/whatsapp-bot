package whatsapp

import "time"

type ClientConfig struct {
	Handler        IEventHandler
	SessionStorage SessionStorage
}

type SessionStorage interface {
	Save(*SessionData) error
	Load() (*SessionData, error)
}

const (
	ReconnectDelay = 30 * time.Second
)