package whatsapp

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func newProbeBot(t *testing.T) *Bot {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := &Bot{SqlDB: db, humanAssistantJID: "11111111111@s.whatsapp.net"}
	if err := b.ensureContactsSchema(); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestContactsSeedAndLookup(t *testing.T) {
	b := newProbeBot(t)

	os.Setenv("KNOWN_CONTACTS", "97375716663491:Wilma:family,76420520931421:Stephanie:family")
	os.Setenv("OWNER_EMAIL", "max@example.com")
	defer os.Unsetenv("KNOWN_CONTACTS")
	defer os.Unsetenv("OWNER_EMAIL")

	if err := b.seedContactsFromEnv(); err != nil {
		t.Fatal(err)
	}
	// Seed must be idempotent.
	if err := b.seedContactsFromEnv(); err != nil {
		t.Fatal(err)
	}
	var n int
	b.SqlDB.QueryRow("SELECT COUNT(*) FROM contacts").Scan(&n)
	if n != 3 {
		t.Fatalf("expected 3 contacts after double-seed, got %d", n)
	}

	// JID lookup, including device-suffixed and bare-number forms.
	if c, ok := b.lookupContactByJID("97375716663491@s.whatsapp.net"); !ok || c.Name != "Wilma" {
		t.Fatalf("lookup by JID failed: ok=%v name=%q", ok, c.Name)
	}
	if c, ok := b.lookupContactByJID("97375716663491:12@s.whatsapp.net"); !ok || c.Name != "Wilma" {
		t.Fatalf("device-suffix lookup failed: ok=%v", ok)
	}
	if c, ok := b.lookupContactByJID("97375716663491"); !ok || c.Name != "Wilma" {
		t.Fatalf("bare-number lookup failed: ok=%v", ok)
	}

	// Name lookup.
	if c, ok := b.lookupContactByName("wilma"); !ok || c.Name != "Wilma" {
		t.Fatal("name lookup failed")
	}
	if _, ok := b.lookupContactByName("nobody"); ok {
		t.Fatal("nonexistent name should not resolve")
	}

	// Tiers.
	if got := b.tierForJID("97375716663491@s.whatsapp.net"); got != TierFamily {
		t.Fatalf("want family, got %s", got)
	}
	if got := b.tierForJID("11111111111@s.whatsapp.net"); got != TierOwner {
		t.Fatalf("want owner, got %s", got)
	}
	if got := b.tierForJID("99999999999@s.whatsapp.net"); got != TierUnknown {
		t.Fatalf("want unknown, got %s", got)
	}

	// Display name resolution + push-name fallback.
	if got := b.resolveSenderName("97375716663491@s.whatsapp.net"); got != "Wilma" {
		t.Fatalf("resolveSenderName=%q", got)
	}
	if got := b.resolveSenderName("11111111111@s.whatsapp.net"); got != "Max" {
		t.Fatalf("owner name=%q", got)
	}
	if got := b.resolveSenderName("88888888888@s.whatsapp.net"); got != "User" {
		t.Fatalf("unknown should be User, got %q", got)
	}
	if got := b.displayNameForJID("88888888888@s.whatsapp.net", "Random Guy"); got != "Random Guy" {
		t.Fatalf("push fallback=%q", got)
	}

	// Tier ordering must be correct for gating decisions.
	if !TierOwner.AtLeast(TierFamily) {
		t.Fatal("owner should satisfy family")
	}
	if TierFamily.AtLeast(TierOwner) {
		t.Fatal("family must NOT satisfy owner")
	}
	if !TierKnown.AtLeast(TierUnknown) {
		t.Fatal("known should satisfy unknown")
	}
	if TierUnknown.AtLeast(TierKnown) {
		t.Fatal("unknown must NOT satisfy known")
	}
}

func TestContactsUpsertAndCacheInvalidation(t *testing.T) {
	b := newProbeBot(t)

	if err := b.upsertContact(Contact{JID: "5551234@s.whatsapp.net", Name: "Test Person", Tier: TierKnown}); err != nil {
		t.Fatal(err)
	}
	if c, ok := b.lookupContactByJID("5551234@s.whatsapp.net"); !ok || c.Name != "Test Person" {
		t.Fatal("upsert not visible")
	}

	// Update must replace, not duplicate, and the cache must see it immediately.
	c := Contact{JID: "5551234@s.whatsapp.net", Name: "Test Person", Tier: TierFamily, Email: "t@example.com"}
	if err := b.upsertContact(c); err != nil {
		t.Fatal(err)
	}
	var n int
	b.SqlDB.QueryRow("SELECT COUNT(*) FROM contacts").Scan(&n)
	if n != 1 {
		t.Fatalf("upsert duplicated: %d rows", n)
	}
	got, _ := b.lookupContactByJID("5551234@s.whatsapp.net")
	if got.Tier != TierFamily || got.Email != "t@example.com" {
		t.Fatalf("upsert did not apply: tier=%s email=%s", got.Tier, got.Email)
	}

	// Empty JID / name must be rejected.
	if err := b.upsertContact(Contact{JID: "", Name: "x"}); err == nil {
		t.Fatal("expected error for empty JID")
	}
	if err := b.upsertContact(Contact{JID: "1@s.whatsapp.net", Name: ""}); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestContactsPromptRoster(t *testing.T) {
	b := newProbeBot(t)
	b.upsertContact(Contact{JID: "1@s.whatsapp.net", Name: "Wilma", Tier: TierFamily, Email: "w@example.com", Aliases: "Wil"})
	out := b.contactsForPrompt()
	if out == "" {
		t.Fatal("roster empty")
	}
	for _, want := range []string{"Wilma", "w@example.com", "Wil"} {
		if !stringsContains(out, want) {
			t.Fatalf("roster missing %q: %s", want, out)
		}
	}
}

func stringsContains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
