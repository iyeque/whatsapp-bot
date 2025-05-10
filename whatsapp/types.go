package whatsapp

import (
	"time"
)

type ConnectionState string

const (
	StateConnecting   ConnectionState = "connecting"
	StateConnected    ConnectionState = "connected"
	StateDisconnected ConnectionState = "disconnected"
	StateReconnecting ConnectionState = "reconnecting"
)

type SessionData struct {
	ID         string
	JID        string
	Created    time.Time
	LastActive time.Time
	State      ConnectionState
	LastError  error
	Data       interface{}
}