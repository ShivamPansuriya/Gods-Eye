// Package agent runs Claude in a tool loop over the PR workspace.
//
// Every agent in a review (mapper, bug hunters, verifiers) is sent the same
// tools, the same system prompt and the same first content block (the PR
// context and annotated diff). Only the task text after it differs. That makes
// the expensive part of every request a prompt-cache read: the first request
// writes the cache, and every agent after it reads it at a fraction of the price.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/shared/constant"

	"github.com/ShivamPansuriya/gods-eye/internal/workspace"
)

// Tool is a tool definition. Submit tools end the loop and return their input.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any // JSON schema of the input object
	Submit      bool
	// Strict guarantees schema-valid input. The API caps the combined grammar
	// of all strict tools, so only small, critical schemas should set it.
	Strict bool
}

// Runner holds everything agents share.
type Runner struct {
	Client  anthropic.Client
	Model   string
	Effort  string
	System  string
	Shared  string // PR context: identical for every agent, cached
	WS      *workspace.Workspace
	Tools   []Tool
	Meter   *Meter
	Sem     chan struct{} // limits concurrent API calls
	Verbose func(string)

	once  sync.Once
	tools []anthropic.BetaToolUnionParam
}

func (r *Runner) toolParams() []anthropic.BetaToolUnionParam {
	r.once.Do(func() {
		ts := append([]Tool{}, r.Tools...)
		sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name }) // deterministic for caching
		for _, t := range ts {
			props := t.Schema["properties"]
			var req []string
			if rr, ok := t.Schema["required"].([]string); ok {
				req = rr
			}
			extra := map[string]any{"additionalProperties": false}
			if defs, ok := t.Schema["$defs"]; ok {
				extra["$defs"] = defs
			}
			tp := anthropic.BetaToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				Strict:      anthropic.Bool(t.Strict),
				InputSchema: anthropic.BetaToolInputSchemaParam{Properties: props, Required: req, ExtraFields: extra},
			}
			r.tools = append(r.tools, anthropic.BetaToolUnionParam{OfTool: &tp})
		}
	})
	return r.tools
}

// Task is one agent run.
type Task struct {
	Stage    string // for the meter: map | hunt | verify
	Label    string // for logs
	Context  string // optional per-task context (e.g. this agent's slice of the diff), cached
	Prompt   string // the task text, sent after the shared block
	Submit   string // the submit tool that ends this task
	MaxTurns int
	Effort   string        // overrides Runner.Effort for this task
	Started  chan struct{} // closed when the first response starts streaming (cache is written)
	// Over reports that this task's share of the budget is spent; the agent is
	// then told to submit what it has instead of exploring further.
	Over func() bool
}

// ErrNoSubmit is returned when the agent never called its submit tool.
var ErrNoSubmit = errors.New("agent finished without submitting a result")

// Run executes a task and returns the submit tool's input JSON.
func (r *Runner) Run(ctx context.Context, t Task) (json.RawMessage, error) {
	if t.MaxTurns <= 0 {
		t.MaxTurns = 10
	}
	shared := anthropic.BetaTextBlockParam{Text: r.Shared, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}
	blocks := []anthropic.BetaContentBlockParamUnion{{OfText: &shared}}
	if t.Context != "" {
		ctxBlock := anthropic.BetaTextBlockParam{Text: t.Context, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}
		blocks = append(blocks, anthropic.BetaContentBlockParamUnion{OfText: &ctxBlock})
	}
	blocks = append(blocks, anthropic.NewBetaTextBlock(t.Prompt))
	msgs := []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(blocks...)}
	nudges := 0
	overAt := -1
	signalled := false
	signal := func() {
		if !signalled && t.Started != nil {
			close(t.Started)
		}
		signalled = true
	}
	defer signal()

	for turn := 0; ; turn++ {
		msg, err := r.call(ctx, t, msgs, signal)
		if err != nil {
			return nil, err
		}
		switch msg.StopReason {
		case anthropic.BetaStopReasonRefusal:
			return nil, fmt.Errorf("%s: model declined (%s)", t.Label, msg.StopDetails.Category)
		case anthropic.BetaStopReasonMaxTokens:
			return nil, fmt.Errorf("%s: hit the output limit", t.Label)
		}
		msgs = append(msgs, msg.ToParam())

		var results []anthropic.BetaContentBlockParamUnion
		for _, b := range msg.Content {
			tu, ok := b.AsAny().(anthropic.BetaToolUseBlock)
			if !ok {
				continue
			}
			raw := json.RawMessage(tu.JSON.Input.Raw())
			if tu.Name == t.Submit {
				r.log(fmt.Sprintf("%s: submitted after %d turns", t.Label, turn+1))
				return raw, nil
			}
			out, isErr := r.exec(tu.Name, raw, t.Submit)
			results = append(results, anthropic.NewBetaToolResultBlock(tu.ID, out, isErr))
		}

		if len(results) == 0 {
			nudges++
			if nudges > 2 {
				return nil, fmt.Errorf("%s: %w", t.Label, ErrNoSubmit)
			}
			msgs = append(msgs, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(
				fmt.Sprintf("Call the %s tool now with your result. Do not answer in plain text.", t.Submit))))
			continue
		}
		if overAt < 0 && t.Over != nil && t.Over() {
			overAt = turn
			r.log(fmt.Sprintf("%s: budget share spent, asking it to submit", t.Label))
		}
		if turn >= t.MaxTurns-1 || overAt >= 0 {
			results = append(results, anthropic.NewBetaTextBlock(
				fmt.Sprintf("You have used your exploration budget. Call %s now with what you have; do not call other tools.", t.Submit)))
		}
		if turn >= t.MaxTurns+2 || (overAt >= 0 && turn >= overAt+3) {
			return nil, fmt.Errorf("%s: %w (turn limit)", t.Label, ErrNoSubmit)
		}
		msgs = append(msgs, anthropic.NewBetaUserMessage(results...))
	}
}

func (r *Runner) call(ctx context.Context, t Task, msgs []anthropic.BetaMessageParam, onStart func()) (*anthropic.BetaMessage, error) {
	select {
	case r.Sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-r.Sem }()

	params := anthropic.BetaMessageNewParams{
		Model:        r.Model,
		MaxTokens:    32000,
		System:       []anthropic.BetaTextBlockParam{{Text: r.System}},
		Tools:        r.toolParams(),
		Messages:     msgs,
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(), // also cache the growing conversation
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(effortOf(r, t))},
		Fallbacks:    anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	}
	t0 := time.Now()
	stream := r.Client.Beta.Messages.NewStreaming(ctx, params)
	msg := anthropic.BetaMessage{}
	first := true
	for stream.Next() {
		if first {
			onStart()
			first = false
		}
		if err := msg.Accumulate(stream.Current()); err != nil {
			return nil, fmt.Errorf("%s: stream: %w", t.Label, err)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", t.Label, err)
	}
	r.Meter.Add(t.Stage, string(msg.Model), msg.Usage, time.Since(t0))
	return &msg, nil
}

func (r *Runner) exec(name string, raw json.RawMessage, submit string) (string, bool) {
	for _, tl := range r.Tools {
		if tl.Name == name && tl.Submit {
			return fmt.Sprintf("Wrong tool: this task ends with %s, not %s.", submit, name), true
		}
	}
	if r.WS == nil {
		return "No repository checkout is available; work from the diff only.", true
	}
	var in struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Pattern   string `json:"pattern"`
		Glob      string `json:"glob"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "invalid input: " + err.Error(), true
	}
	var out string
	switch name {
	case "read_file":
		out = r.WS.Read(in.Path, in.StartLine, in.EndLine)
	case "grep":
		out = r.WS.Grep(in.Pattern, in.Glob)
	case "list_files":
		out = r.WS.List(in.Glob)
	default:
		return "unknown tool " + name, true
	}
	return out, strings.HasPrefix(out, "error:")
}

func (r *Runner) log(s string) {
	if r.Verbose != nil {
		r.Verbose(s)
	}
}

// ReadTools are the workspace tools every agent gets.
func ReadTools() []Tool {
	return []Tool{
		{Name: "read_file", Description: "Read numbered lines of a file in the repository at the PR head. Max 250 lines per call; read only the range you need.",
			Schema: obj(map[string]any{"path": str("Repository-relative path"), "start_line": integer("First line, 1-based"), "end_line": integer("Last line, inclusive")})},
		{Name: "grep", Description: "Search tracked files at the PR head with an extended regex (git grep -E). Returns path:line:text, max 60 hits. Use it to find callers, other uses of a pattern, tests, and config.",
			Schema: obj(map[string]any{"pattern": str("Extended regular expression"), "glob": str(`Path glob to limit the search, e.g. "src/**/*.java"; "" for all files`)})},
		{Name: "list_files", Description: "List tracked files matching a glob, e.g. \"**/*Test.java\" or \"src/api/*\". Max 200 entries.",
			Schema: obj(map[string]any{"glob": str("Path glob")})},
	}
}

func obj(props map[string]any) map[string]any {
	req := make([]string, 0, len(props))
	for k := range props {
		req = append(req, k)
	}
	sort.Strings(req)
	return map[string]any{"type": "object", "properties": props, "required": req}
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func effortOf(r *Runner, t Task) string {
	if t.Effort != "" {
		return t.Effort
	}
	return r.Effort
}
