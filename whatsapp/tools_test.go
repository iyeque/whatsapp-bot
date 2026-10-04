package whatsapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"whatsapp-gpt-bot/ai"

	_ "modernc.org/sqlite"
)

func newToolBot(t *testing.T) *Bot {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "tools.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := &Bot{SqlDB: db, humanAssistantJID: "11111111111@s.whatsapp.net"}
	if err := b.ensureContactsSchema(); err != nil {
		t.Fatal(err)
	}
	// notes table is needed by the save_note handler.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS notes (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at DATETIME NOT NULL, contact_jid TEXT NOT NULL, contact_name TEXT NOT NULL, note TEXT NOT NULL, created_by_jid TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	b.registerTools()
	return b
}

// TestSpecsForTierEnforcesVisibility is the core security assertion: a lower
// tier must never even SEE a tool it cannot use, so the model cannot request it.
func TestSpecsForTierEnforcesVisibility(t *testing.T) {
	b := newToolBot(t)

	unknown := b.tools.SpecsForTier(TierUnknown)
	owner := b.tools.SpecsForTier(TierOwner)
	family := b.tools.SpecsForTier(TierFamily)

	names := func(specs []ai.ToolSpec) []string {
		var out []string
		for _, s := range specs {
			out = append(out, s.Name)
		}
		return out
	}
	un, own, fam := names(unknown), names(owner), names(family)
	t.Logf("unknown sees: %v", un)
	t.Logf("family  sees: %v", fam)
	t.Logf("owner   sees: %v", own)

	if contains(un, "schedule_meeting") {
		t.Fatal("unknown tier must NOT see schedule_meeting")
	}
	if contains(un, "send_message") {
		t.Fatal("unknown tier must NOT see send_message")
	}
	if !contains(un, "contact_lookup") {
		t.Fatal("unknown tier should see contact_lookup (read-only)")
	}
	if !contains(own, "schedule_meeting") || !contains(own, "send_message") {
		t.Fatal("owner must see all tools")
	}
	if contains(fam, "schedule_meeting") {
		t.Fatal("family must NOT see schedule_meeting (owner-only)")
	}
	if len(own) <= len(un) {
		t.Fatal("owner should see strictly more tools than unknown")
	}
}

// TestExecuteDeniesInsufficientTier proves that even a hand-crafted call to a
// privileged tool is refused, and that the refusal is reported as a denial
// (so the model cannot claim success).
func TestExecuteDeniesInsufficientTier(t *testing.T) {
	b := newToolBot(t)
	ctx := context.Background()

	tc := ToolContext{
		CallerJID:  "999@s.whatsapp.net",
		CallerName: "Stranger",
		CallerTier: TierUnknown,
		ChatID:     "999@s.whatsapp.net",
		Bot:        b,
	}
	call := ai.ToolCall{
		ID:        "c1",
		Name:      "schedule_meeting",
		Arguments: `{"attendee":"Wilma","when":"2026-10-08T14:00:00","duration_minutes":30}`,
	}
	res := b.tools.Execute(ctx, tc, call)

	if res.OK {
		t.Fatal("unknown tier must not be allowed to execute schedule_meeting")
	}
	if !res.Denied {
		t.Fatal("result should be flagged as a permission denial")
	}
	if !strings.Contains(res.Content, "permission denied") {
		t.Fatalf("denial payload should say permission denied, got: %s", res.Content)
	}
	// The model must be explicitly told not to claim success.
	if !strings.Contains(res.Content, "not able to do that") {
		t.Fatalf("denial should instruct the model to refuse, got: %s", res.Content)
	}
	t.Logf("denial payload: %s", res.Content)
}

// TestExecuteValidatesRequiredArgs proves missing required arguments are caught
// before the handler runs.
func TestExecuteValidatesRequiredArgs(t *testing.T) {
	b := newToolBot(t)
	ctx := context.Background()
	tc := ToolContext{CallerJID: "111@s.whatsapp.net", CallerName: "Max", CallerTier: TierOwner, ChatID: "111@s.whatsapp.net", Bot: b}

	// Missing 'when' and 'duration_minutes'.
	res := b.tools.Execute(ctx, tc, ai.ToolCall{ID: "c", Name: "schedule_meeting", Arguments: `{"attendee":"Wilma"}`})
	if res.OK {
		t.Fatal("should reject missing required args")
	}
	if !strings.Contains(res.Content, "missing required") {
		t.Fatalf("expected missing-required error, got: %s", res.Content)
	}
	t.Logf("validation payload: %s", res.Content)

	// Malformed JSON.
	res = b.tools.Execute(ctx, tc, ai.ToolCall{ID: "c", Name: "schedule_meeting", Arguments: `{not json`})
	if res.OK || !strings.Contains(res.Content, "could not parse") {
		t.Fatalf("expected parse error, got ok=%v: %s", res.OK, res.Content)
	}

	// Unknown tool.
	res = b.tools.Execute(ctx, tc, ai.ToolCall{ID: "c", Name: "nonexistent_tool", Arguments: `{}`})
	if res.OK || !strings.Contains(res.Content, "unknown tool") {
		t.Fatalf("expected unknown-tool error, got ok=%v: %s", res.OK, res.Content)
	}
}

// TestScheduleMeetingRejectsPastDate proves the hallucinated-date guard works.
// The live model was measured emitting past dates, so this is a real failure mode.
func TestScheduleMeetingRejectsPastDate(t *testing.T) {
	b := newToolBot(t)
	b.upsertContact(Contact{JID: "222@s.whatsapp.net", Name: "Wilma", Tier: TierFamily})
	ctx := context.Background()
	tc := ToolContext{CallerJID: "111@s.whatsapp.net", CallerName: "Max", CallerTier: TierOwner, ChatID: "111@s.whatsapp.net", Bot: b}

	res := b.tools.Execute(ctx, tc, ai.ToolCall{
		ID: "c", Name: "schedule_meeting",
		Arguments: `{"attendee":"Wilma","when":"2026-07-16T14:00:00","duration_minutes":30}`,
	})
	if res.OK {
		t.Fatal("past date must be rejected")
	}
	if !strings.Contains(res.Content, "in the past") {
		t.Fatalf("expected past-date error, got: %s", res.Content)
	}
	t.Logf("past-date payload: %s", res.Content)
}

// TestScheduleMeetingReportsNotConfigured proves the placeholder never claims
// success — the model is told explicitly that nothing was booked.
func TestScheduleMeetingReportsNotConfigured(t *testing.T) {
	b := newToolBot(t)
	b.upsertContact(Contact{JID: "222@s.whatsapp.net", Name: "Wilma", Tier: TierFamily})
	ctx := context.Background()
	tc := ToolContext{CallerJID: "111@s.whatsapp.net", CallerName: "Max", CallerTier: TierOwner, ChatID: "111@s.whatsapp.net", Bot: b}

	future := "2030-01-15T14:00:00"
	res := b.tools.Execute(ctx, tc, ai.ToolCall{
		ID: "c", Name: "schedule_meeting",
		Arguments: `{"attendee":"Wilma","when":"` + future + `","duration_minutes":30}`,
	})
	if !res.OK {
		t.Fatalf("expected success envelope, got: %s", res.Content)
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			Configured bool   `json:"configured"`
			Message    string `json:"message"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if out.Result.Configured {
		t.Fatal("calendar must report configured=false until wired")
	}
	if !strings.Contains(out.Result.Message, "NOT booked") {
		t.Fatalf("message must state the meeting was not booked, got: %s", out.Result.Message)
	}
}

// TestSaveNotePersists proves a permitted tool actually writes.
func TestSaveNotePersists(t *testing.T) {
	b := newToolBot(t)
	ctx := context.Background()
	tc := ToolContext{CallerJID: "222@s.whatsapp.net", CallerName: "Wilma", CallerTier: TierFamily, ChatID: "222@s.whatsapp.net", Bot: b}

	res := b.tools.Execute(ctx, tc, ai.ToolCall{ID: "c", Name: "save_note", Arguments: `{"note":"prefers morning calls"}`})
	if !res.OK {
		t.Fatalf("save_note failed: %s", res.Content)
	}
	var n int
	var stored string
	b.SqlDB.QueryRow("SELECT COUNT(*) FROM notes").Scan(&n)
	b.SqlDB.QueryRow("SELECT note FROM notes LIMIT 1").Scan(&stored)
	if n != 1 || stored != "prefers morning calls" {
		t.Fatalf("note not persisted: count=%d stored=%q", n, stored)
	}
}

// TestContactLookupHidesEmailFromNonOwner proves data scoping in a tool handler.
func TestContactLookupHidesEmailFromNonOwner(t *testing.T) {
	b := newToolBot(t)
	b.upsertContact(Contact{JID: "222@s.whatsapp.net", Name: "Wilma", Tier: TierFamily, Email: "wilma@example.com"})
	ctx := context.Background()

	stranger := b.tools.Execute(ctx, ToolContext{CallerTier: TierUnknown, CallerName: "X", Bot: b},
		ai.ToolCall{ID: "c", Name: "contact_lookup", Arguments: `{"name":"Wilma"}`})
	if strings.Contains(stranger.Content, "wilma@example.com") {
		t.Fatalf("email leaked to non-owner: %s", stranger.Content)
	}

	owner := b.tools.Execute(ctx, ToolContext{CallerTier: TierOwner, CallerName: "Max", Bot: b},
		ai.ToolCall{ID: "c", Name: "contact_lookup", Arguments: `{"name":"Wilma"}`})
	if !strings.Contains(owner.Content, "wilma@example.com") {
		t.Fatalf("owner should see email: %s", owner.Content)
	}
}

// TestDuplicateRegistrationRejected proves misconfiguration surfaces early.
func TestDuplicateRegistrationRejected(t *testing.T) {
	r := NewToolRegistry()
	tool := Tool{Name: "x", Handler: func(context.Context, ToolContext, map[string]interface{}) (interface{}, error) { return nil, nil }}
	if err := r.Register(tool); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(tool); err == nil {
		t.Fatal("duplicate registration must be rejected")
	}
	if err := r.Register(Tool{Name: "no-handler"}); err == nil {
		t.Fatal("tool without handler must be rejected")
	}
	// Unlabelled tools must fail closed (owner-only), not default to open.
	r2 := NewToolRegistry()
	r2.MustRegister(Tool{Name: "unlabelled", Handler: func(context.Context, ToolContext, map[string]interface{}) (interface{}, error) { return nil, nil }})
	if got := r2.tools["unlabelled"].RequiredTier; got != TierOwner {
		t.Fatalf("unlabelled tool should default to owner-only, got %q", got)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
