package whatsapp

type Message struct {
	Text    string
	To      string
	From    string
	Type    string
	Content []byte
}

// IEventHandler defines the interface for handling WhatsApp events
type IEventHandler interface {
	Handle(interface{}) error
	RestoreSession([]byte) error
}

// IMessenger defines the core messaging interface
type IMessenger interface {
	Send(msg Message) error
	SetHandler(IEventHandler)
}

// IClientOption defines options for configuring the client
type IClientOption func(*Client) error
