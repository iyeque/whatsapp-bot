package whatsapp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"whatsapp-gpt-bot/ai"

	"github.com/rs/zerolog/log"
)

// maxToolRounds bounds the agent loop. Each round is one model turn plus the
// tool executions it requested. The cap prevents a model that keeps calling
// tools from looping forever and burning tokens.
const maxToolRounds = 4

// ToolLoopResult is the outcome of an agentic turn.
type ToolLoopResult struct {
	Text      string
	Rounds    int
	ToolsUsed []string
	Denied    []string
	Tokens    int
	Latency   time.Duration
	UsedTools bool
}

// runToolLoop drives an agentic conversation turn.
//
// The model is given only the tools its caller's permission tier allows, so it
// cannot request a capability the caller lacks. Tool results are fed back as
// observations and the model is re-prompted until it produces a final text
// answer or the round cap is reached.
//
// This generalises the bespoke [SEARCH] loop: same act/observe/act shape, but
// driven by a registry rather than a hardcoded tag.
func (b *Bot) runToolLoop(ctx context.Context, tc ToolContext, systemPrompt string, userMsg string, history []string) (ToolLoopResult, error) {
	start := time.Now()
	var result ToolLoopResult

	if b.tools == nil {
		return result, fmt.Errorf("tool registry not initialised")
	}

	// Gate on model capability. Advertising tools to a model that cannot call
	// them produces fabricated success messages, which is worse than not having
	// the feature — so if nothing can call tools, report that plainly.
	if !ai.ToolsAvailable() {
		return result, fmt.Errorf("no tool-capable AI provider configured")
	}

	specs := b.tools.SpecsForTier(tc.CallerTier)
	if len(specs) == 0 {
		return result, fmt.Errorf("no tools available for tier %q", tc.CallerTier)
	}

	// Assemble the conversation. History is passed as prior turns so the model
	// has the same context it would have had in the plain-text path.
	msgs := []ai.ToolMessage{{Role: "system", Content: systemPrompt}}
	for _, h := range history {
		if strings.TrimSpace(h) == "" {
			continue
		}
		msgs = append(msgs, ai.ToolMessage{Role: "user", Content: h})
	}
	msgs = append(msgs, ai.ToolMessage{Role: "user", Content: userMsg})

	timeout := getMaxTimeout()
	seenCalls := map[string]bool{}

	for round := 0; round < maxToolRounds; round++ {
		res, err := ai.MakeToolChat(msgs, specs, timeout)
		if err != nil {
			return result, fmt.Errorf("tool chat failed on round %d: %w", round+1, err)
		}
		result.Rounds = round + 1
		result.Tokens += res.Tokens
		result.Latency += res.Latency

		// No tool calls: this is the final answer.
		if len(res.ToolCalls) == 0 {
			result.Text = res.Text
			result.Latency = time.Since(start)
			return result, nil
		}

		// Record the assistant turn that requested the tools.
		msgs = append(msgs, ai.ToolMessage{
			Role:      "assistant",
			Content:   res.Text,
			ToolCalls: res.ToolCalls,
		})

		// Execute each requested call and feed the observations back.
		for _, call := range res.ToolCalls {
			// Guard against a model repeating an identical call indefinitely:
			// on a repeat, tell it the result is unchanged instead of re-running
			// the side effect.
			sig := call.Name + "|" + call.Arguments
			if seenCalls[sig] {
				payload := `{"ok":false,"error":"this exact call was already made this turn; do not repeat it, use the previous result or answer the user"}`
				msgs = append(msgs, ai.ToolMessage{Role: "tool", ToolCallID: call.ID, Content: payload})
				log.Warn().Str("tool", call.Name).Msg("Suppressed duplicate tool call")
				continue
			}
			seenCalls[sig] = true

			exec := b.tools.Execute(ctx, tc, call)
			result.ToolsUsed = append(result.ToolsUsed, call.Name)
			result.UsedTools = true
			if exec.Denied {
				result.Denied = append(result.Denied, call.Name)
			}
			msgs = append(msgs, ai.ToolMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    exec.Content,
			})
		}
	}

	// Round cap reached. Rather than leaving the user with silence, ask the model
	// for a final answer with no tools available so it must respond in prose.
	log.Warn().Int("rounds", maxToolRounds).Msg("Tool loop hit round cap; requesting final answer without tools")
	final, err := ai.MakeToolChat(msgs, nil, timeout)
	if err != nil {
		result.Latency = time.Since(start)
		return result, fmt.Errorf("tool loop exhausted and final answer failed: %w", err)
	}
	result.Text = final.Text
	result.Tokens += final.Tokens
	result.Latency = time.Since(start)
	return result, nil
}

// buildToolSystemPrompt extends the normal system prompt with the tool-usage
// contract: how to behave, and critically what to do when a tool is denied or
// fails, so the model never claims a denied action succeeded.
func (b *Bot) buildToolSystemPrompt(base string, _ ContactTier) string {
	var sb strings.Builder
	sb.WriteString(base)
	sb.WriteString("\n\n### TOOLS YOU CAN USE:\n")
	sb.WriteString("You have tools available for actions that change something (saving notes, scheduling, sending messages). Use them when the user asks for an action — do not just describe what you would do.\n")
	sb.WriteString("CRITICAL RULES:\n")
	sb.WriteString("- If a tool returns an error or says it is not configured, you MUST tell the user the action did NOT happen. Never claim success you did not receive.\n")
	sb.WriteString("- If a tool is denied for permissions, tell the user plainly that you are not able to do that for them. Do not offer workarounds.\n")
	sb.WriteString("- Never invent details a tool needs (dates, email addresses, people). Ask the user instead.\n")
	sb.WriteString("- Prefer one tool call over several. Do not repeat a call you already made this turn.\n")
	return sb.String()
}
