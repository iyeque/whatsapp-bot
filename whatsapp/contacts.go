package whatsapp

import (
	"fmt"
	"os"
	"strings"
	"time"

	wtypes "go.mau.fi/whatsmeow/types"

	"github.com/rs/zerolog/log"
)

// ContactTier determines what a contact is allowed to make the bot do.
// Tiers are ordered by privilege: a higher tier implies everything below it.
type ContactTier string

const (
	// TierOwner is the account owner (Max). Full capability.
	TierOwner ContactTier = "owner"
	// TierFamily is trusted inner circle. May use personal tools, not outward ones.
	TierFamily ContactTier = "family"
	// TierKnown is someone the bot has interacted with and who is identified.
	// Conversational only.
	TierKnown ContactTier = "known"
	// TierUnknown is anyone unidentified. Conversational only, most restricted.
	TierUnknown ContactTier = "unknown"
)

// tierRank lets us compare tiers numerically for "at least this tier" checks.
func tierRank(t ContactTier) int {
	switch t {
	case TierOwner:
		return 3
	case TierFamily:
		return 2
	case TierKnown:
		return 1
	default:
		return 0
	}
}

// AtLeast reports whether this tier is equal to or higher than the required tier.
func (t ContactTier) AtLeast(required ContactTier) bool {
	return tierRank(t) >= tierRank(required)
}

// Contact is a resolved identity: a stable WhatsApp address plus the metadata
// needed to act on someone's behalf (name, email, relationship, permissions).
type Contact struct {
	ID        int64
	JID       string // normalised, e.g. "97375716663491@s.whatsapp.net"
	LID       string // alternative addressing form, may be empty
	Name      string // canonical display name
	Aliases   string // comma-separated alternate names the model/user may use
	Email     string // for calendar invites and email
	Tier      ContactTier
	Notes     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// contactCacheTTL bounds how stale the in-memory contact index may be.
const contactCacheTTL = 60 * time.Second

// normalizeJID strips the device suffix and lowercases, so the same person
// resolves consistently regardless of which device they message from.
// "97375716663491:12@s.whatsapp.net" -> "97375716663491@s.whatsapp.net"
func normalizeJID(jid string) string {
	if jid == "" {
		return ""
	}
	jid = strings.TrimSpace(jid)
	if at := strings.Index(jid, "@"); at != -1 {
		user, server := jid[:at], jid[at:]
		// Drop the ":device" suffix that whatsmeow appends for multi-device.
		if colon := strings.Index(user, ":"); colon != -1 {
			user = user[:colon]
		}
		return strings.ToLower(user + server)
	}
	return strings.ToLower(jid)
}

// baseNumber returns just the numeric part of a JID, for fuzzy matching against
// the forms that appear in the model's prompt and in legacy hardcoded strings.
func baseNumber(jid string) string {
	jid = normalizeJID(jid)
	if at := strings.Index(jid, "@"); at != -1 {
		return jid[:at]
	}
	return jid
}

// ---------- schema ----------

const createContactsTableSQL = `
CREATE TABLE IF NOT EXISTS contacts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	jid TEXT NOT NULL UNIQUE,
	lid TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL,
	aliases TEXT NOT NULL DEFAULT '',
	email TEXT NOT NULL DEFAULT '',
	tier TEXT NOT NULL DEFAULT 'unknown',
	notes TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL
);
`

// ensureContactsSchema creates the contacts table and its lookup index.
func (b *Bot) ensureContactsSchema() error {
	if _, err := b.SqlDB.Exec(createContactsTableSQL); err != nil {
		return fmt.Errorf("failed to create contacts table: %w", err)
	}
	if _, err := b.SqlDB.Exec("CREATE INDEX IF NOT EXISTS idx_contacts_name ON contacts(name)"); err != nil {
		log.Warn().Err(err).Msg("migration: failed to create index on contacts(name)")
	}
	if _, err := b.SqlDB.Exec("CREATE INDEX IF NOT EXISTS idx_contacts_lid ON contacts(lid)"); err != nil {
		log.Warn().Err(err).Msg("migration: failed to create index on contacts(lid)")
	}
	return nil
}

// seedContactsFromEnv inserts the owner and any legacy JIDs from the environment
// exactly once. This migrates the previously hardcoded identities into the table
// so behaviour is preserved while removing the literals from the code path.
//
// Seeding is guarded by an existence check per row (INSERT OR IGNORE on the
// unique jid), so repeated startups are idempotent and never clobber edits the
// operator made via !contact.
func (b *Bot) seedContactsFromEnv() error {
	now := time.Now()

	type seed struct {
		jid, name, aliases, email string
		tier                      ContactTier
	}
	var seeds []seed

	// The owner. HUMAN_ASSISTANT_JID is required for the bot to start at all.
	if b.humanAssistantJID != "" {
		seeds = append(seeds, seed{
			jid:   normalizeJID(b.humanAssistantJID),
			name:  "Max",
			email: strings.TrimSpace(getenvDefault("OWNER_EMAIL", "")),
			tier:  TierOwner,
		})
	}

	// Family / known contacts, migrated from the legacy hardcoded literals.
	// Format: "jid:Name:Tier,jid:Name:Tier"
	if raw := strings.TrimSpace(getenvDefault("KNOWN_CONTACTS", "")); raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			parts := strings.Split(strings.TrimSpace(entry), ":")
			if len(parts) < 2 {
				log.Warn().Msgf("KNOWN_CONTACTS entry malformed (want jid:Name[:tier]): %q", entry)
				continue
			}
			jid := normalizeJID(parts[0])
			name := strings.TrimSpace(parts[1])
			tier := TierKnown
			if len(parts) >= 3 {
				switch strings.ToLower(strings.TrimSpace(parts[2])) {
				case "owner":
					tier = TierOwner
				case "family":
					tier = TierFamily
				case "known":
					tier = TierKnown
				case "unknown":
					tier = TierUnknown
				default:
					log.Warn().Msgf("KNOWN_CONTACTS unknown tier %q for %s; using 'known'", parts[2], name)
				}
			}
			if jid == "" || name == "" {
				continue
			}
			seeds = append(seeds, seed{jid: jid, name: name, tier: tier})
		}
	}

	inserted := 0
	for _, s := range seeds {
		res, err := b.SqlDB.Exec(
			`INSERT OR IGNORE INTO contacts (jid, name, aliases, email, tier, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?)`,
			s.jid, s.name, s.aliases, s.email, string(s.tier), now, now,
		)
		if err != nil {
			log.Warn().Err(err).Msgf("seedContacts: failed to insert %s", s.name)
			continue
		}
		if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
			inserted++
		}
	}
	if inserted > 0 {
		log.Info().Msgf("seedContacts: inserted %d contact(s) from environment", inserted)
	}
	return nil
}

// getenvDefault returns the environment value or a fallback when unset/empty.
func getenvDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// ---------- lookup ----------

// contactIndex caches normalised-JID -> Contact and lowercased-name -> Contact,
// refreshed at most once per contactCacheTTL. Contacts change rarely (only via
// !contact), so a short TTL keeps lookups off the hot path without risking
// meaningful staleness.
type contactIndex struct {
	byJID  map[string]Contact
	byName map[string]Contact
	built  time.Time
}

// loadContactIndex returns the cached index, rebuilding it if the TTL expired.
func (b *Bot) loadContactIndex() *contactIndex {
	b.contactMu.RLock()
	if b.contactIdx != nil && time.Since(b.contactIdx.built) < contactCacheTTL {
		idx := b.contactIdx
		b.contactMu.RUnlock()
		return idx
	}
	b.contactMu.RUnlock()

	idx := &contactIndex{
		byJID:  make(map[string]Contact),
		byName: make(map[string]Contact),
		built:  time.Now(),
	}

	rows, err := b.SqlDB.Query(
		`SELECT id, jid, lid, name, aliases, email, tier, notes, created_at, updated_at FROM contacts`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("loadContactIndex: query failed; returning empty index")
		return idx
	}
	defer rows.Close()

	for rows.Next() {
		var c Contact
		var tier string
		if err := rows.Scan(&c.ID, &c.JID, &c.LID, &c.Name, &c.Aliases, &c.Email,
			&tier, &c.Notes, &c.CreatedAt, &c.UpdatedAt); err != nil {
			continue
		}
		c.Tier = ContactTier(tier)
		c.JID = normalizeJID(c.JID)
		if c.JID != "" {
			idx.byJID[c.JID] = c
			if num := baseNumber(c.JID); num != "" {
				idx.byJID[num] = c // allow matching bare numbers
			}
		}
		if c.LID != "" {
			idx.byJID[normalizeJID(c.LID)] = c
		}
		if c.Name != "" {
			idx.byName[strings.ToLower(c.Name)] = c
		}
		for _, alias := range strings.Split(c.Aliases, ",") {
			if a := strings.ToLower(strings.TrimSpace(alias)); a != "" {
				idx.byName[a] = c
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("loadContactIndex: rows iteration error")
	}

	b.contactMu.Lock()
	b.contactIdx = idx
	b.contactMu.Unlock()
	return idx
}

// invalidateContactIndex forces the next lookup to rebuild from the database.
func (b *Bot) invalidateContactIndex() {
	b.contactMu.Lock()
	b.contactIdx = nil
	b.contactMu.Unlock()
}

// lookupContactByJID resolves a WhatsApp address to a Contact.
// Returns ok=false when the address is not a known contact.
func (b *Bot) lookupContactByJID(jid string) (Contact, bool) {
	if jid == "" {
		return Contact{}, false
	}
	idx := b.loadContactIndex()
	if c, ok := idx.byJID[normalizeJID(jid)]; ok {
		return c, true
	}
	if num := baseNumber(jid); num != "" {
		if c, ok := idx.byJID[num]; ok {
			return c, true
		}
	}
	return Contact{}, false
}

// lookupContactByName resolves a human name (or alias) to a Contact.
// Matching is case-insensitive; an exact name/alias match wins over a substring.
func (b *Bot) lookupContactByName(name string) (Contact, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return Contact{}, false
	}
	idx := b.loadContactIndex()
	if c, ok := idx.byName[name]; ok {
		return c, true
	}
	// Fall back to substring matching for things like "wilma" vs "Wilma K".
	for key, c := range idx.byName {
		if strings.Contains(key, name) || strings.Contains(name, key) {
			return c, true
		}
	}
	return Contact{}, false
}

// tierForJID returns the permission tier for an address, defaulting to unknown.
func (b *Bot) tierForJID(jid string) ContactTier {
	if c, ok := b.lookupContactByJID(jid); ok {
		return c.Tier
	}
	// The owner is recognised even if the contacts row is somehow missing.
	if b.humanAssistantJID != "" && baseNumber(jid) == baseNumber(b.humanAssistantJID) {
		return TierOwner
	}
	return TierUnknown
}

// displayNameForJID returns the canonical name for an address, falling back to
// the provided push name, then to "User".
func (b *Bot) displayNameForJID(jid, pushName string) string {
	if c, ok := b.lookupContactByJID(jid); ok && c.Name != "" {
		return c.Name
	}
	if strings.TrimSpace(pushName) != "" {
		return pushName
	}
	return "User"
}

// ---------- management ----------

// upsertContact inserts or updates a contact by JID and invalidates the cache.
func (b *Bot) upsertContact(c Contact) error {
	if c.JID == "" {
		return fmt.Errorf("contact requires a jid")
	}
	c.JID = normalizeJID(c.JID)
	if c.Name == "" {
		return fmt.Errorf("contact requires a name")
	}
	if c.Tier == "" {
		c.Tier = TierKnown
	}
	now := time.Now()
	_, err := b.SqlDB.Exec(
		`INSERT INTO contacts (jid, lid, name, aliases, email, tier, notes, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(jid) DO UPDATE SET
			lid = excluded.lid,
			name = excluded.name,
			aliases = excluded.aliases,
			email = excluded.email,
			tier = excluded.tier,
			notes = excluded.notes,
			updated_at = excluded.updated_at`,
		c.JID, c.LID, c.Name, c.Aliases, c.Email, string(c.Tier), c.Notes, now, now,
	)
	if err != nil {
		return err
	}
	b.invalidateContactIndex()
	return nil
}

// listContacts returns all contacts ordered by tier then name.
func (b *Bot) listContacts() ([]Contact, error) {
	rows, err := b.SqlDB.Query(
		`SELECT id, jid, lid, name, aliases, email, tier, notes, created_at, updated_at
		 FROM contacts ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Contact
	for rows.Next() {
		var c Contact
		var tier string
		if err := rows.Scan(&c.ID, &c.JID, &c.LID, &c.Name, &c.Aliases, &c.Email,
			&tier, &c.Notes, &c.CreatedAt, &c.UpdatedAt); err != nil {
			continue
		}
		c.Tier = ContactTier(tier)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// ---------- commands ----------

// handleContactCommand implements:
//
//	!contact                          list all contacts
//	!contact <name>                   show one contact
//	!contact add <jid> <Name> [tier]  create/update a contact
//	!contact email <name> <email>     set a contact's email
//	!contact tier <name> <tier>       change a contact's permission tier
//
// Owner-only; enforced by the caller before dispatch.
func (b *Bot) handleContactCommand(chat wtypes.JID, raw string) {
	args := strings.Fields(strings.TrimSpace(raw))
	if len(args) == 0 {
		b.printContacts(chat)
		return
	}

	switch strings.ToLower(args[0]) {
	case "add":
		if len(args) < 3 {
			b.sendAcknowledgment(chat, "Usage: !contact add <jid> <Name> [owner|family|known|unknown]")
			return
		}
		jid := args[1]
		name := args[2]
		tier := TierKnown
		if len(args) >= 4 {
			tier = ContactTier(strings.ToLower(args[3]))
		}
		if err := b.upsertContact(Contact{JID: jid, Name: name, Tier: tier}); err != nil {
			b.sendAcknowledgment(chat, "❌ Couldn't save contact: "+err.Error())
			return
		}
		b.sendAcknowledgment(chat, fmt.Sprintf("✅ Contact saved: %s (%s, %s)", name, jid, tier))

	case "email":
		if len(args) < 3 {
			b.sendAcknowledgment(chat, "Usage: !contact email <name> <email>")
			return
		}
		c, ok := b.lookupContactByName(args[1])
		if !ok {
			b.sendAcknowledgment(chat, fmt.Sprintf("❌ No contact named %q.", args[1]))
			return
		}
		c.Email = args[2]
		if err := b.upsertContact(c); err != nil {
			b.sendAcknowledgment(chat, "❌ Couldn't update email: "+err.Error())
			return
		}
		b.sendAcknowledgment(chat, fmt.Sprintf("✅ %s's email set to %s", c.Name, args[2]))

	case "tier":
		if len(args) < 3 {
			b.sendAcknowledgment(chat, "Usage: !contact tier <name> <owner|family|known|unknown>")
			return
		}
		c, ok := b.lookupContactByName(args[1])
		if !ok {
			b.sendAcknowledgment(chat, fmt.Sprintf("❌ No contact named %q.", args[1]))
			return
		}
		c.Tier = ContactTier(strings.ToLower(args[2]))
		if err := b.upsertContact(c); err != nil {
			b.sendAcknowledgment(chat, "❌ Couldn't update tier: "+err.Error())
			return
		}
		b.sendAcknowledgment(chat, fmt.Sprintf("✅ %s is now tier '%s'.", c.Name, c.Tier))

	default:
		// Treat the whole argument as a name lookup.
		c, ok := b.lookupContactByName(strings.Join(args, " "))
		if !ok {
			b.sendAcknowledgment(chat, fmt.Sprintf("❌ No contact matching %q. Use `!contact` to list.", raw))
			return
		}
		b.sendAcknowledgment(chat, fmt.Sprintf("👤 *%s*\nJID: %s\nTier: %s\nEmail: %s\nAliases: %s",
			c.Name, c.JID, c.Tier, emptyDash(c.Email), emptyDash(c.Aliases)))
	}
}

// printContacts renders the contact list grouped by tier.
func (b *Bot) printContacts(chat wtypes.JID) {
	contacts, err := b.listContacts()
	if err != nil {
		b.sendAcknowledgment(chat, "❌ Couldn't load contacts.")
		return
	}
	if len(contacts) == 0 {
		b.sendAcknowledgment(chat, "No contacts yet. Add one with `!contact add <jid> <Name> family`.")
		return
	}
	var sb strings.Builder
	sb.WriteString("👥 *Contacts:*\n\n")
	for _, c := range contacts {
		fmt.Fprintf(&sb, "- *%s* [%s]", c.Name, c.Tier)
		if c.Email != "" {
			fmt.Fprintf(&sb, " — %s", c.Email)
		}
		sb.WriteString("\n")
	}
	b.sendAcknowledgment(chat, sb.String())
}

// emptyDash renders an empty string as a dash for display.
func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// contactsForPrompt renders the contact roster for the system prompt so the model
// knows who it can refer to and act on behalf of.
func (b *Bot) contactsForPrompt() string {
	contacts, err := b.listContacts()
	if err != nil || len(contacts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n### PEOPLE YOU KNOW (use exact names when acting on someone's behalf):\n")
	for _, c := range contacts {
		fmt.Fprintf(&sb, "- %s", c.Name)
		if c.Aliases != "" {
			fmt.Fprintf(&sb, " (also called: %s)", c.Aliases)
		}
		if c.Email != "" {
			fmt.Fprintf(&sb, " <%s>", c.Email)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
