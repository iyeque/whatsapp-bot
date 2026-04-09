package whatsapp

import (
	"fmt"
	"os"
	"strings"
	"time"

	"whatsapp-gpt-bot/ai"
	"github.com/rs/zerolog/log"
)

// Reflect analyzes Max's recent messages and updates soul.md if necessary.
func (b *Bot) Reflect() {
	b.mutex.RLock()
	// Get recent Max messages
	var history strings.Builder
	for _, msg := range b.conversations[b.humanAssistantJID].Messages {
		if msg.Role == "user" {
			history.WriteString(msg.Content + "\n")
		}
	}
	b.mutex.RUnlock()

	soulContent, err := os.ReadFile("soul.md")
	if err != nil {
		log.Error().Err(err).Msg("Failed to read soul.md for reflection")
		return
	}

	prompt := fmt.Sprintf(`### INSTRUCTION:
Compare the current "Soul" definition with Max's recent communication style below. If Max's style has significantly evolved (e.g., tone, vocabulary, length of responses), update the "Soul" definition to better match his reality. If no major change is needed, respond with "NO_CHANGE".

### CURRENT SOUL DEFINITION:
%s

### MAX'S RECENT MESSAGES:
%s

### OUTPUT:
Provide the updated content for soul.md, or "NO_CHANGE". Respond ONLY with the content or the keyword.`, string(soulContent), history.String())

	response, _, _, err := ai.MakeAIRequest(prompt, nil, "", time.Minute)
	if err != nil {
		log.Error().Err(err).Msg("Failed to generate personality reflection")
		return
	}

	response = strings.TrimSpace(response)
	if response == "NO_CHANGE" {
		log.Info().Msg("Soul reflection: No changes needed.")
		return
	}

	// Backup current soul
	os.WriteFile("soul.md.bak", soulContent, 0644)
	
	// Update soul
	err = os.WriteFile("soul.md", []byte(response), 0644)
	if err != nil {
		log.Error().Err(err).Msg("Failed to update soul.md")
		return
	}

	log.Info().Msg("Soul evolved! soul.md updated based on Max's recent communication.")
}
