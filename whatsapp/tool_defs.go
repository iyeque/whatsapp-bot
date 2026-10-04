package whatsapp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/rs/zerolog/log"
)

// registerTools builds the tool registry for this bot. Tools are declared here
// with an explicit RequiredTier; the registry enforces the tier at execution
// time against the caller's resolved contact tier, so a model cannot escalate
// privileges by being persuasive.
func (b *Bot) registerTools() {
	r := NewToolRegistry()

	// ---- contact_lookup: any contact may resolve names to people ----
	r.MustRegister(Tool{
		Name:        "contact_lookup",
		Description: "Look up a person the assistant knows. Use this to resolve a name to a known contact before acting on their behalf, or to check someone's email.",
		Parameters: objSchema(map[string]interface{}{
			"name": strProp("The person's name, or part of it."),
		}, "name"),
		RequiredTier: TierUnknown, // read-only, no side effects
		Handler: func(ctx context.Context, tc ToolContext, args map[string]interface{}) (interface{}, error) {
			name, _ := args["name"].(string)
			c, ok := tc.Bot.lookupContactByName(name)
			if !ok {
				return map[string]interface{}{
					"found":   false,
					"message": fmt.Sprintf("No known contact matching %q.", name),
				}, nil
			}
			// Do not leak email addresses to non-owner callers.
			out := map[string]interface{}{
				"found": true,
				"name":  c.Name,
				"tier":  string(c.Tier),
			}
			if tc.CallerTier.AtLeast(TierOwner) {
				out["email"] = c.Email
				out["jid"] = c.JID
				out["aliases"] = c.Aliases
			}
			return out, nil
		},
	})

	// ---- save_note: remember a durable fact about the current person ----
	r.MustRegister(Tool{
		Name:        "save_note",
		Description: "Save a durable fact worth remembering about the person you are talking to. Use when they state a preference, a personal detail, or something that should persist.",
		Parameters: objSchema(map[string]interface{}{
			"note": strProp("The fact to remember, phrased concisely."),
		}, "note"),
		RequiredTier: TierUnknown, // about the caller themselves
		Handler: func(ctx context.Context, tc ToolContext, args map[string]interface{}) (interface{}, error) {
			note, _ := args["note"].(string)
			note = strings.TrimSpace(note)
			if note == "" {
				return nil, fmt.Errorf("note was empty")
			}
			name := tc.CallerName
			if name == "" {
				name = "Unknown"
			}
			_, err := tc.Bot.SqlDB.Exec(
				"INSERT INTO notes (created_at, contact_jid, contact_name, note, created_by_jid) VALUES (?,?,?,?,?)",
				time.Now(), tc.CallerJID, name, note, tc.ChatID,
			)
			if err != nil {
				return nil, fmt.Errorf("could not save note: %w", err)
			}
			return map[string]interface{}{"saved": true, "note": note}, nil
		},
	})

	// ---- schedule_meeting: placeholder until the calendar backend is wired ----
	// Declared now so the loop, permissions and validation are exercised
	// end-to-end. The handler deliberately reports "not configured" instead of
	// pretending, so the model cannot tell the user a meeting was booked.
	r.MustRegister(Tool{
		Name:        "schedule_meeting",
		Description: "Schedule a meeting with a known contact. Requires a person, a start time, and a duration.",
		Parameters: objSchema(map[string]interface{}{
			"attendee":         strProp("Name of the known contact to meet."),
			"when":             strProp("Start time in RFC3339 / ISO-8601 format, e.g. 2026-10-08T14:00:00+04:00."),
			"duration_minutes": intProp("Length of the meeting in minutes."),
			"title":            strProp("Short title for the meeting."),
		}, "attendee", "when", "duration_minutes"),
		RequiredTier: TierOwner, // creates outward-facing invitations
		Handler: func(ctx context.Context, tc ToolContext, args map[string]interface{}) (interface{}, error) {
			attendee, _ := args["attendee"].(string)
			whenRaw, _ := args["when"].(string)

			contact, ok := tc.Bot.lookupContactByName(attendee)
			if !ok {
				return nil, fmt.Errorf("no known contact named %q; ask the user to add them first", attendee)
			}

			// The model is known to emit plausible-but-wrong datetimes (including
			// past dates), so parse strictly and reject the past outright.
			when, err := parseWhenFlexible(whenRaw)
			if err != nil {
				return nil, fmt.Errorf("could not understand the start time %q: %v", whenRaw, err)
			}
			if when.Before(time.Now().Add(-time.Minute)) {
				return nil, fmt.Errorf("the start time %s is in the past; ask the user to confirm the date", when.Format(time.RFC3339))
			}

			return map[string]interface{}{
				"configured": false,
				"attendee":   contact.Name,
				"when":       when.Format(time.RFC3339),
				"message":    "Calendar integration is not configured yet, so no invitation was sent. Tell the user the meeting was NOT booked.",
			}, nil
		},
	})

	// ---- send_message: message a known contact on the owner's behalf ----
	r.MustRegister(Tool{
		Name:        "send_message",
		Description: "Send a WhatsApp message to a known contact on the owner's behalf. Use only when explicitly asked to relay something to someone.",
		Parameters: objSchema(map[string]interface{}{
			"recipient": strProp("Name of the known contact to message."),
			"message":   strProp("The message text to send."),
		}, "recipient", "message"),
		RequiredTier: TierOwner, // outward-facing: only the owner may send as the bot
		Handler: func(ctx context.Context, tc ToolContext, args map[string]interface{}) (interface{}, error) {
			recipient, _ := args["recipient"].(string)
			body, _ := args["message"].(string)

			contact, ok := tc.Bot.lookupContactByName(recipient)
			if !ok {
				return nil, fmt.Errorf("no known contact named %q", recipient)
			}
			targetJID, err := types.ParseJID(contact.JID)
			if err != nil {
				return nil, fmt.Errorf("contact %q has an unusable address: %v", contact.Name, err)
			}
			if err := tc.Bot.sendReminderToContact(targetJID, body); err != nil {
				return nil, fmt.Errorf("failed to send: %w", err)
			}
			return map[string]interface{}{"sent": true, "to": contact.Name}, nil
		},
	})

	b.tools = r
	log.Info().Strs("tools", r.Names()).Msg("Tool registry initialised")
}

// parseWhenFlexible accepts the datetime shapes models actually emit, in order
// of strictness, so a slightly-off format does not force a retry loop.
func parseWhenFlexible(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	layouts := []string{
		time.RFC3339,                // 2026-10-08T14:00:00+04:00
		"2006-01-02T15:04:05Z07:00", // explicit offset
		"2006-01-02T15:04:05",       // no zone
		"2006-01-02 15:04:05",       // space separator
		"2006-01-02T15:04",          // no seconds
		"2006-01-02 15:04",          // space, no seconds
		"2006-01-02",                // date only
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised datetime format")
}
