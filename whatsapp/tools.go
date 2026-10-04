package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"whatsapp-gpt-bot/ai"

	"github.com/rs/zerolog/log"
)

// ToolContext carries everything a tool handler needs to act safely.
// It is deliberately explicit: a handler must never reach for ambient state,
// because that is how a permission check gets bypassed.
type ToolContext struct {
	CallerJID  string      // the WhatsApp address that triggered this turn
	CallerName string      // resolved display name
	CallerTier ContactTier // resolved permission tier
	ChatID     string      // the conversation this turn belongs to
	IsGroup    bool
	RawMessage string // the user's original text, for guardrails
	Bot        *Bot
}

// Tool is one capability the model may invoke.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object for the tool's arguments.
	Parameters map[string]interface{}
	// RequiredTier is the minimum permission tier allowed to invoke this tool.
	// An unknown contact is TierUnknown, so a tool with RequiredTier=TierOwner
	// is owner-only and a tool with TierUnknown is open to anyone.
	RequiredTier ContactTier
	// Handler executes the tool. It returns a JSON-serialisable result on
	// success, or an error whose message is fed back to the model so it can
	// explain the failure rather than fabricate success.
	Handler func(ctx context.Context, tc ToolContext, args map[string]interface{}) (interface{}, error)
}

// ToolRegistry holds the registered tools and enforces permissions.
type ToolRegistry struct {
	tools map[string]Tool
}

// NewToolRegistry creates an empty registry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]Tool)}
}

// Register adds a tool, rejecting duplicates and invalid definitions so a
// misconfiguration surfaces at startup rather than as a silent no-op.
func (r *ToolRegistry) Register(t Tool) error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("tool requires a name")
	}
	if t.Handler == nil {
		return fmt.Errorf("tool %q requires a handler", t.Name)
	}
	if _, exists := r.tools[t.Name]; exists {
		return fmt.Errorf("tool %q already registered", t.Name)
	}
	if t.RequiredTier == "" {
		t.RequiredTier = TierOwner // fail closed: unlabelled tools are owner-only
	}
	r.tools[t.Name] = t
	return nil
}

// MustRegister is Register for static setup, panicking on programmer error.
func (r *ToolRegistry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Get returns a tool by name.
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names returns all registered tool names, sorted (for logging/tests).
func (r *ToolRegistry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SpecsForTier returns the tool specs visible to a given permission tier.
// This is the security boundary: a caller never sees a tool they may not use,
// so the model cannot request it and no denial path needs to exist.
func (r *ToolRegistry) SpecsForTier(tier ContactTier) []ai.ToolSpec {
	var specs []ai.ToolSpec
	for _, t := range r.tools {
		if !tier.AtLeast(t.RequiredTier) {
			continue
		}
		specs = append(specs, ai.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}

// ToolResult is the outcome of executing one tool call.
type ToolResult struct {
	Name     string
	OK       bool
	Denied   bool
	Content  string // JSON fed back to the model
	Duration time.Duration
}

// Execute runs a tool call with full permission enforcement.
//
// Every failure mode returns a model-readable JSON payload rather than a Go
// error, so the model can explain what went wrong. Crucially, a permission
// denial is reported as a denial — the model is told it is not allowed, so it
// cannot claim the action succeeded.
func (r *ToolRegistry) Execute(ctx context.Context, tc ToolContext, call ai.ToolCall) ToolResult {
	start := time.Now()

	fail := func(format string, args ...interface{}) ToolResult {
		msg := fmt.Sprintf(format, args...)
		payload, _ := json.Marshal(map[string]interface{}{
			"ok":    false,
			"error": msg,
		})
		return ToolResult{Name: call.Name, OK: false, Content: string(payload), Duration: time.Since(start)}
	}

	tool, ok := r.tools[call.Name]
	if !ok {
		log.Warn().Str("tool", call.Name).Msg("Model requested an unregistered tool")
		return fail("unknown tool %q", call.Name)
	}

	// Permission gate. Checked against the caller's resolved tier, not against
	// anything the model said, so prompt injection cannot escalate privileges.
	if !tc.CallerTier.AtLeast(tool.RequiredTier) {
		log.Warn().
			Str("tool", call.Name).
			Str("caller", tc.CallerName).
			Str("tier", string(tc.CallerTier)).
			Str("required", string(tool.RequiredTier)).
			Msg("Tool call denied: insufficient tier")
		payload, _ := json.Marshal(map[string]interface{}{
			"ok":    false,
			"error": fmt.Sprintf("permission denied: %q requires tier '%s' but caller is '%s'. Tell the user you are not able to do that for them.", call.Name, tool.RequiredTier, tc.CallerTier),
		})
		return ToolResult{Name: call.Name, OK: false, Denied: true, Content: string(payload), Duration: time.Since(start)}
	}

	// Parse arguments. A malformed payload is reported back so the model can retry
	// with valid JSON rather than the call silently vanishing.
	var args map[string]interface{}
	if strings.TrimSpace(call.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return fail("could not parse arguments as JSON: %v", err)
		}
	}
	if args == nil {
		args = map[string]interface{}{}
	}

	// Enforce the declared schema's required fields. Measured model behaviour is
	// that required fields are sometimes omitted or filled with hallucinated
	// values, so this catches the omission before the handler acts on it.
	if missing := missingRequired(tool.Parameters, args); len(missing) > 0 {
		return fail("missing required argument(s): %s", strings.Join(missing, ", "))
	}

	// Bound execution so a wedged tool cannot stall the conversation.
	toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := tool.Handler(toolCtx, tc, args)
	dur := time.Since(start)
	if err != nil {
		log.Warn().Err(err).Str("tool", call.Name).Msg("Tool handler returned an error")
		payload, _ := json.Marshal(map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		})
		return ToolResult{Name: call.Name, OK: false, Content: string(payload), Duration: dur}
	}

	// Wrap the handler's result in a consistent envelope.
	payload, merr := json.Marshal(map[string]interface{}{
		"ok":     true,
		"result": res,
	})
	if merr != nil {
		return fail("tool returned an unserialisable result: %v", merr)
	}
	log.Info().Str("tool", call.Name).Dur("duration", dur).Msg("Tool executed")
	return ToolResult{Name: call.Name, OK: true, Content: string(payload), Duration: dur}
}

// missingRequired reports which of the schema's required properties are absent
// or empty in the supplied arguments.
func missingRequired(schema map[string]interface{}, args map[string]interface{}) []string {
	if schema == nil {
		return nil
	}
	raw, ok := schema["required"]
	if !ok {
		return nil
	}
	var missing []string
	switch req := raw.(type) {
	case []string:
		for _, name := range req {
			if isEmptyArg(args[name]) {
				missing = append(missing, name)
			}
		}
	case []interface{}:
		for _, item := range req {
			if name, ok := item.(string); ok && isEmptyArg(args[name]) {
				missing = append(missing, name)
			}
		}
	}
	return missing
}

// isEmptyArg treats absent, null, and blank strings as missing.
func isEmptyArg(v interface{}) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

// ---------- schema helpers ----------

// objSchema builds a JSON Schema object declaration.
func objSchema(props map[string]interface{}, required ...string) map[string]interface{} {
	s := map[string]interface{}{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// strProp / intProp / boolProp build typed property declarations.
func strProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func intProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}

func boolProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}
