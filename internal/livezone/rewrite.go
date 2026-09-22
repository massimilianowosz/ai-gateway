package livezone

import (
	"encoding/json"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/promptcache"
)

// frozenTracker floors every rewriter's start index at how much of the
// client's resent history matched the previous turn for its conversation,
// regardless of LiveTurns. See internal/promptcache for why: LiveTurns is a
// turn-count guess, and a wide one (live_turns=9999, a real deployed setting)
// mutated messages a previous turn had already forwarded and the provider had
// already cached, on every request.
var frozenTracker = promptcache.NewTracker(4096, 30*time.Minute)

// applyFrozenFloor raises start to the tracker's floor for this conversation
// and splices in the exact bytes forwarded for that many leading messages
// last turn — not the client's raw resend, which differs from what is cached
// whenever last turn compressed them. Leaving the raw resend in place would
// itself change the wire bytes, which busts the cache exactly as surely as a
// fresh, different recompression.
//
// Returns the (possibly raised) start, the hashes of the untouched input, and
// whether anything was actually spliced — which the caller must fold into its
// own "did I change the body" accounting: a splice changes the wire bytes
// even when nothing else in the pipeline found anything new to compress.
func applyFrozenFloor(pol Policy, msgs []json.RawMessage, start int) (int, []string, bool) {
	if pol.Scope == "" {
		return start, nil, false
	}
	hashes := hashesOf(msgs)
	floor, forwarded := frozenTracker.Frozen(pol.Scope, hashes)
	spliced := false
	for i := 0; i < floor && i < len(msgs) && i < len(forwarded); i++ {
		if string(msgs[i]) != string(forwarded[i]) {
			spliced = true
		}
		msgs[i] = json.RawMessage(forwarded[i])
	}
	if floor > start {
		start = floor
	}
	return start, hashes, spliced
}

// confirmFrozen records this turn's client-sent hashes against what is
// actually being forwarded, for reuse on the next turn.
//
// alignedUpTo bounds how many leading pairs the caller can prove are still
// positionally aligned — its own floor, since nothing after applyFrozenFloor
// is allowed to insert or remove a message below it. A transform that removes
// messages beyond that point (pruning stale reasoning) shifts every later
// position, so those pairs are not safe to remember: doing so anyway would
// let a later turn splice one message's bytes into a different message's
// slot.
func confirmFrozen(pol Policy, clientHashes []string, final []json.RawMessage, alignedUpTo int) {
	if pol.Scope == "" || clientHashes == nil {
		return
	}
	raw := make([][]byte, len(final))
	for i, m := range final {
		raw[i] = []byte(m)
	}
	frozenTracker.Confirm(pol.Scope, clientHashes, raw, alignedUpTo)
}

func hashesOf(msgs []json.RawMessage) []string {
	raw := make([][]byte, len(msgs))
	for i, m := range msgs {
		raw[i] = []byte(m)
	}
	return promptcache.HashAll(raw)
}

// Policy controls which blocks a rewrite may touch.
type Policy struct {
	Options Options
	// ProtectedTools extends the built-in never-compress list.
	ProtectedTools []string
	// LiveTurns is how many trailing user turns count as the live zone. 1 is
	// the newest turn only. Everything earlier is left alone, whether or not a
	// provider cache currently covers it: history that changes between turns
	// defeats caching even when the cache is cold today.
	LiveTurns int
	// DedupeRepeats replaces a tool result that repeats one already in the
	// request with a reference to it. It applies to protected tools too — the
	// first copy is what carries the offsets an agent quotes back, and it is
	// never touched.
	DedupeRepeats bool
	// DeltaRepeats extends that to a result which is a small edit of an earlier
	// one: the later copy states what changed instead of repeating the whole
	// payload. Same safety argument — the first copy stays intact — but it
	// rewrites content rather than only recognising it, so it is separately
	// opt-in.
	DeltaRepeats bool
	// PruneReasoning drops the reasoning items of turns already answered, on
	// the Responses API. Only the chain since the last user message survives.
	PruneReasoning bool
	// Vault holds the originals of content a lossy transform removed, and Scope
	// is the caller's partition within it. Both empty means a lossy transform
	// discards for good, which is the honest default when nothing can serve the
	// content back.
	Vault *Vault
	Scope string
}

// DefaultPolicy returns conservative settings: newest turn only, lossless
// transforms only.
func DefaultPolicy() Policy {
	return Policy{Options: DefaultOptions(), LiveTurns: 1}
}

// ByteDelta totals the size of the blocks one transformer changed.
type ByteDelta struct{ Before, After int }

// Stats summarises one rewrite.
type Stats struct {
	BlocksSeen    int
	BlocksChanged int
	// BytesBefore and BytesAfter cover every block seen, changed or not.
	BytesBefore int
	BytesAfter  int
	// Transformers counts changed blocks by transformer, and BytesByTransformer
	// totals those blocks' sizes. The two are reported together so byte savings
	// can be attributed per transformer without re-counting the whole-request
	// aggregate once per block.
	Transformers       map[string]int
	BytesByTransformer map[string]ByteDelta
	SkippedProtect     int
	SkippedNoChange    int
	// SkipReasons counts refusals by cause. "No transformer recognised this"
	// and "a transformer ran and the saving was too small" both leave a block
	// unchanged, and only this tells them apart.
	SkipReasons map[string]int
}

func newStats() *Stats {
	return &Stats{
		Transformers:       map[string]int{},
		BytesByTransformer: map[string]ByteDelta{},
		SkipReasons:        map[string]int{},
	}
}

func (s *Stats) record(res Result, name string) {
	s.BlocksSeen++
	s.BytesBefore += res.BytesBefore
	s.BytesAfter += res.BytesAfter
	if res.Applied {
		s.BlocksChanged++
		s.Transformers[name]++
		d := s.BytesByTransformer[name]
		d.Before += res.BytesBefore
		d.After += res.BytesAfter
		s.BytesByTransformer[name] = d
	} else {
		s.SkippedNoChange++
		reason := res.Reason
		if reason == "" {
			reason = "unspecified"
		}
		// The detected kind rides along: "gain_below_threshold on a table" and
		// "gain_below_threshold on logs" call for different tuning.
		s.SkipReasons[res.Kind.String()+":"+reason]++
	}
}

// RewriteAnthropic compresses tool_result blocks in the live zone of a
// /v1/messages body.
//
// Everything outside a compressed block is copied byte-for-byte: unmodified
// messages and every envelope field other than "messages" stay
// json.RawMessage, so no value is ever re-encoded from a parsed form. That
// matters beyond tidiness — a re-encode changes numeric literals and string
// escapes, which changes the tokens the provider sees. Only the order of the
// envelope's top-level keys is not preserved, which is not part of the prompt.
//
// Returns the original bytes unchanged when nothing was compressed.
func RewriteAnthropic(body []byte, pol Policy) ([]byte, *Stats, error) {
	stats := newStats()

	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return body, stats, nil // not our shape; forward untouched
	}
	rawMsgs, ok := env["messages"]
	if !ok {
		return body, stats, nil
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return body, stats, nil
	}

	names := anthropicToolNames(msgs)
	start := liveZoneStart(msgs, pol.LiveTurns, isAnthropicRealUserTurn)
	start, clientHashes, changed := applyFrozenFloor(pol, msgs, start)

	// Repeats come out first, on the original text: a block rewritten by a
	// transformer would no longer match the earlier copy it duplicates.
	if pol.DedupeRepeats {
		if dedupeRepeatsAnthropic(msgs, start, pol, names, stats) {
			changed = true
		}
	}

	for i := start; i < len(msgs); i++ {
		out, did := rewriteAnthropicMessage(msgs[i], pol, names, stats)
		if did {
			msgs[i] = out
			changed = true
		}
	}
	// Anthropic messages are never inserted or removed here, only mutated in
	// place, so the whole list is safe to confirm.
	confirmFrozen(pol, clientHashes, msgs, len(msgs))
	if !changed {
		return body, stats, nil
	}

	newMsgs, err := json.Marshal(msgs)
	if err != nil {
		return body, stats, err
	}
	env["messages"] = newMsgs
	out, err := json.Marshal(env)
	if err != nil {
		return body, stats, err
	}
	return out, stats, nil
}

// rewriteAnthropicMessage compresses the tool_result blocks of one message.
func rewriteAnthropicMessage(raw json.RawMessage, pol Policy, names map[string]string, stats *Stats) (json.RawMessage, bool) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return raw, false
	}
	rawContent, ok := msg["content"]
	if !ok {
		return raw, false
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(rawContent, &blocks); err != nil {
		return raw, false // string content carries no tool_result
	}

	changed := false
	for i, b := range blocks {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(b, &block); err != nil {
			continue
		}
		var typ string
		if err := json.Unmarshal(block["type"], &typ); err != nil || typ != "tool_result" {
			continue
		}
		tool := resultToolName(anthropicToolName(block), rawString(block, "tool_use_id"), names)
		if IsProtected(tool, pol.ProtectedTools) {
			stats.SkippedProtect++
			continue
		}
		newContent, did := rewriteAnthropicToolResult(block["content"], pol, stats)
		if !did {
			continue
		}
		block["content"] = newContent
		nb, err := json.Marshal(block)
		if err != nil {
			continue
		}
		blocks[i] = nb
		changed = true
	}
	if !changed {
		return raw, false
	}
	nc, err := json.Marshal(blocks)
	if err != nil {
		return raw, false
	}
	msg["content"] = nc
	out, err := json.Marshal(msg)
	if err != nil {
		return raw, false
	}
	return out, true
}

// rewriteAnthropicToolResult handles the two shapes a tool_result content
// field takes: a bare string, or an array of content parts.
func rewriteAnthropicToolResult(raw json.RawMessage, pol Policy, stats *Stats) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return raw, false
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		res := Transform(text, pol.Options)
		if !res.Applied {
			stats.record(res, res.Transformer)
			return raw, false
		}
		content, _ := reversible(pol, res, text)
		res.BytesAfter = len(content)
		stats.record(res, res.Transformer)
		out, err := json.Marshal(content)
		if err != nil {
			return raw, false
		}
		return out, true
	}

	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw, false
	}
	changed := false
	for i, p := range parts {
		var part map[string]json.RawMessage
		if err := json.Unmarshal(p, &part); err != nil {
			continue
		}
		var typ string
		if err := json.Unmarshal(part["type"], &typ); err != nil || typ != "text" {
			// Images and other non-text parts stay byte-identical.
			continue
		}
		var t string
		if err := json.Unmarshal(part["text"], &t); err != nil {
			continue
		}
		res := Transform(t, pol.Options)
		if !res.Applied {
			stats.record(res, res.Transformer)
			continue
		}
		content, _ := reversible(pol, res, t)
		res.BytesAfter = len(content)
		stats.record(res, res.Transformer)
		nt, err := json.Marshal(content)
		if err != nil {
			continue
		}
		part["text"] = nt
		np, err := json.Marshal(part)
		if err != nil {
			continue
		}
		parts[i] = np
		changed = true
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(parts)
	if err != nil {
		return raw, false
	}
	return out, true
}

// anthropicToolName recovers the tool name for a tool_result block. Anthropic
// carries it on the matching tool_use, not on the result, so this is usually
// empty — which IsProtected treats as protected. Clients that echo a name get
// the benefit of the list.
func anthropicToolName(block map[string]json.RawMessage) string {
	for _, key := range []string{"name", "tool_name"} {
		if raw, ok := block[key]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

// isAnthropicRealUserTurn matches a genuine user turn, not the envelope that
// carries tool output back to the model.
//
// Anthropic delivers tool_result blocks inside a role="user" message, so a bare
// role check counts every tool response as a new turn. With LiveTurns=1 that
// walks the live zone forward on every request: a tool result is compressed
// once, the client re-sends it verbatim on the next turn, and the prompt then
// diverges from the previous one at that position — invalidating the provider
// prefix cache from there on, which is the exact cost this package exists to
// avoid. A user message carrying only tool_result blocks is a continuation of
// the assistant's turn, not the start of a new one.
func isAnthropicRealUserTurn(raw json.RawMessage) bool {
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	if m.Role != "user" {
		return false
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return true // string content is always a real turn
	}
	if len(blocks) == 0 {
		return true // nothing to identify it as a tool-result envelope
	}
	for _, b := range blocks {
		var t struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &t); err != nil || t.Type != "tool_result" {
			return true
		}
	}
	return false
}

// liveZoneStart returns the index of the first message in the live zone: the
// start of the Nth-from-last real user turn. Returns 0 when there are fewer
// turns than requested, which makes the whole conversation live — correct for
// a short conversation, where there is no compressed history to protect.
//
// The zone runs from that turn to the end of the request, so in an agentic
// loop — one human turn followed by many assistant/tool pairs — it covers
// every tool result in the loop, not just the newest. That is deliberate: the
// transforms are deterministic, so a block compressed on turn N compresses
// identically on turn N+1 and the prefix stays byte-stable. The cost is that
// per-request transform work grows with the length of the loop; Options.MaxBytes
// bounds any single block, not the total.
func liveZoneStart(msgs []json.RawMessage, turns int, isTurn func(json.RawMessage) bool) int {
	if turns <= 0 {
		turns = 1
	}
	var starts []int
	for i, m := range msgs {
		if isTurn(m) {
			starts = append(starts, i)
		}
	}
	if len(starts) <= turns {
		return 0
	}
	return starts[len(starts)-turns]
}

// RewriteOpenAI compresses role="tool" messages in the live zone of a
// /v1/chat/completions body. Same byte-preservation contract as
// RewriteAnthropic: untouched messages stay json.RawMessage.
func RewriteOpenAI(body []byte, pol Policy) ([]byte, *Stats, error) {
	stats := newStats()

	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return body, stats, nil
	}
	rawMsgs, ok := env["messages"]
	if !ok {
		return body, stats, nil
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return body, stats, nil
	}

	names := openAIToolNames(msgs)
	start := liveZoneStart(msgs, pol.LiveTurns, isOpenAIRealUserTurn)
	start, clientHashes, changed := applyFrozenFloor(pol, msgs, start)

	// Repeats come out first, on the original text: a block rewritten by a
	// transformer would no longer match the earlier copy it duplicates.
	if pol.DedupeRepeats {
		if dedupeRepeatsOpenAI(msgs, start, pol, names, stats) {
			changed = true
		}
	}

	for i := start; i < len(msgs); i++ {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(msgs[i], &msg); err != nil {
			continue
		}
		var role string
		if err := json.Unmarshal(msg["role"], &role); err != nil || role != "tool" {
			continue
		}
		tool := resultToolName(openAIToolName(msg), rawString(msg, "tool_call_id"), names)
		if IsProtected(tool, pol.ProtectedTools) {
			stats.SkippedProtect++
			continue
		}
		var text string
		if err := json.Unmarshal(msg["content"], &text); err != nil {
			continue // structured content: leave alone
		}
		res := Transform(text, pol.Options)
		if !res.Applied {
			stats.record(res, res.Transformer)
			continue
		}
		content, _ := reversible(pol, res, text)
		res.BytesAfter = len(content)
		stats.record(res, res.Transformer)
		nc, err := json.Marshal(content)
		if err != nil {
			continue
		}
		msg["content"] = nc
		out, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		msgs[i] = out
		changed = true
	}
	// OpenAI chat messages are never inserted or removed here, only mutated in
	// place, so the whole list is safe to confirm.
	confirmFrozen(pol, clientHashes, msgs, len(msgs))
	if !changed {
		return body, stats, nil
	}

	newMsgs, err := json.Marshal(msgs)
	if err != nil {
		return body, stats, err
	}
	env["messages"] = newMsgs
	out, err := json.Marshal(env)
	if err != nil {
		return body, stats, err
	}
	return out, stats, nil
}

// openAIToolName recovers the tool name from a tool message. OpenAI puts it on
// the assistant's tool_calls rather than the result, so this is usually empty
// and the block is treated as protected unless the client echoes a name.
func openAIToolName(msg map[string]json.RawMessage) string {
	if raw, ok := msg["name"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return s
		}
	}
	return ""
}

// isOpenAIRealUserTurn matches a genuine user turn, not a tool response.
func isOpenAIRealUserTurn(raw json.RawMessage) bool {
	var m struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return m.Role == "user"
}

// RewriteResponses compresses function_call_output, custom_tool_call_output
// and computer_call_output items in the live zone of a /v1/responses body —
// the wire format Codex CLI/Desktop and the Responses API use in place of
// "messages". custom_tool_call_output is what Codex CLI actually sends for
// its built-in tools (freeform/non-JSON-schema calls), not
// function_call_output, so both must be handled or nothing is ever touched. A
// LocalShellCall's result is not a distinct wire type — it comes back as an
// ordinary function_call_output, so that's covered too.
// Same byte-preservation contract as RewriteOpenAI: untouched items stay
// json.RawMessage, and only "input" is ever replaced in the envelope.
func RewriteResponses(body []byte, pol Policy) ([]byte, *Stats, error) {
	stats := newStats()

	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return body, stats, nil // not our shape; forward untouched
	}
	rawItems, ok := env["input"]
	if !ok {
		return body, stats, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawItems, &items); err != nil {
		return body, stats, nil
	}

	names := responsesToolNames(items)
	start := liveZoneStart(items, pol.LiveTurns, isResponsesUserTurn)
	start, clientHashes, changed := applyFrozenFloor(pol, items, start)

	// Repeats come out first, on the original text: a block rewritten by a
	// transformer would no longer match the earlier copy it duplicates.
	if pol.DedupeRepeats {
		if dedupeRepeatsResponses(items, start, pol, names, stats) {
			changed = true
		}
	}

	for i := start; i < len(items); i++ {
		out, did := rewriteResponsesItem(items[i], pol, names, stats)
		if did {
			items[i] = out
			changed = true
		}
	}

	// Pruning runs last: it removes items, so doing it earlier would shift
	// every index the two passes above work with. Bounded to start: an item
	// below the frozen floor must never be removed, or a later turn's Confirm
	// would pair a client hash with the wrong forwarded item once positions
	// shift — exactly the misalignment the floor exists to prevent.
	aligned := len(items)
	if pol.PruneReasoning {
		if pruned, did := pruneReasoning(items, start, stats); did {
			items = pruned
			changed = true
			aligned = start // nothing after start is provably still aligned
		}
	}
	confirmFrozen(pol, clientHashes, items, aligned)

	if !changed {
		return body, stats, nil
	}

	newItems, err := json.Marshal(items)
	if err != nil {
		return body, stats, err
	}
	env["input"] = newItems
	out, err := json.Marshal(env)
	if err != nil {
		return body, stats, err
	}
	return out, stats, nil
}

// rewriteResponsesItem compresses the "output" field of a function_call_output
// item (or its custom-tool/computer-use equivalents). Every other item type —
// message, function_call, reasoning — stays byte-identical: the live zone for
// Responses is the tool output coming back, same as role="tool" for chat
// completions and tool_result for Anthropic.
func rewriteResponsesItem(raw json.RawMessage, pol Policy, names map[string]string, stats *Stats) (json.RawMessage, bool) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return raw, false
	}
	var typ string
	if err := json.Unmarshal(item["type"], &typ); err != nil {
		return raw, false
	}
	switch typ {
	// A LocalShellCall's result travels as an ordinary function_call_output
	// on the wire (see codex-rs's context_manager/normalize.rs) — there is no
	// separate "local_shell_call_output" item type to match here.
	case "function_call_output", "custom_tool_call_output", "computer_call_output":
	default:
		return raw, false
	}

	// Responses function_call_output/custom_tool_call_output items carry only
	// call_id, not the tool's name — resolve it from the matching call item
	// the same way OpenAI's tool_call_id is resolved, since an unnamed result
	// is treated as protected.
	tool := resultToolName("", rawString(item, "call_id"), names)
	if IsProtected(tool, pol.ProtectedTools) {
		stats.SkippedProtect++
		return raw, false
	}

	rawOutput, ok := item["output"]
	if !ok {
		return raw, false
	}
	newOutput, did := rewriteResponsesOutput(rawOutput, pol, stats)
	if !did {
		return raw, false
	}
	item["output"] = newOutput
	out, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return out, true
}

// rewriteResponsesOutput handles the two shapes a function_call_output's
// "output" field takes: a bare string, or an array of content parts. Codex
// CLI tags a tool result's text parts "input_text" — the same content-item
// type it uses for input_image/input_audio, per FunctionCallOutputContentItem
// in openai/codex's protocol crate — not "output_text"/"text", which are the
// tags a message item's own content carries. A single-InputText result
// collapses to the bare-string case above, so this branch is what handles a
// multi-part result: text mixed with an image, say.
func rewriteResponsesOutput(raw json.RawMessage, pol Policy, stats *Stats) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return raw, false
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		res := Transform(text, pol.Options)
		if !res.Applied {
			stats.record(res, res.Transformer)
			return raw, false
		}
		content, _ := reversible(pol, res, text)
		res.BytesAfter = len(content)
		stats.record(res, res.Transformer)
		out, err := json.Marshal(content)
		if err != nil {
			return raw, false
		}
		return out, true
	}

	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw, false
	}
	changed := false
	for i, p := range parts {
		var part map[string]json.RawMessage
		if err := json.Unmarshal(p, &part); err != nil {
			continue
		}
		var typ string
		if err := json.Unmarshal(part["type"], &typ); err != nil || (typ != "output_text" && typ != "text" && typ != "input_text") {
			// Images and other non-text parts stay byte-identical.
			continue
		}
		var t string
		if err := json.Unmarshal(part["text"], &t); err != nil {
			continue
		}
		res := Transform(t, pol.Options)
		if !res.Applied {
			stats.record(res, res.Transformer)
			continue
		}
		content, _ := reversible(pol, res, t)
		res.BytesAfter = len(content)
		stats.record(res, res.Transformer)
		nt, err := json.Marshal(content)
		if err != nil {
			continue
		}
		part["text"] = nt
		np, err := json.Marshal(part)
		if err != nil {
			continue
		}
		parts[i] = np
		changed = true
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(parts)
	if err != nil {
		return raw, false
	}
	return out, true
}

// isResponsesUserTurn matches a genuine user turn — a message item with
// role=user — as opposed to a function_call, function_call_output, or
// assistant message. Mirrors isOpenAIRealUserTurn/isAnthropicRealUserTurn: a
// tool result must not itself count as the start of a new turn, or the live
// zone would walk forward on every request and invalidate the provider's
// prefix cache from that point on.
func isResponsesUserTurn(raw json.RawMessage) bool {
	var m struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return m.Type == "message" && m.Role == "user"
}
