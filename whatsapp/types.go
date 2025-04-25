package whatsapp

type ConnectionState int

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
	StateReconnecting
)

type SessionData struct {
	Data []byte
}

func NewSessionData(data []byte) *SessionData {
	return &SessionData{
		Data: data,
	}
}
