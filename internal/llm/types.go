// Package llm talks to a model provider. The wire types below are the OpenAI
// chat/completions shapes, which is not a preference so much as an observation:
// almost every vendor and every local server speaks them, so they are the
// closest thing to a neutral interchange format that exists. A provider with a
// wire format of its own — Anthropic's messages API is the one that matters —
// becomes a second implementation of API translating to and from these, rather
// than a second set of types leaking into the agent.
package llm

import "context"

// API is one provider's wire protocol. The agent loop makes exactly this one
// call, which is what keeps a new provider from reaching any further into the
// codebase than this package.
type API interface {
	Chat(ctx context.Context, messages []MsgBlock, opts ChatOptions) (*Result, error)
}

// Catalog is a provider that can describe the models it serves. It is separate
// from API because most cannot: a plain OpenAI-compatible /v1/models answers
// with bare ids and no context length, tool support, or pricing, and a provider
// that has nothing to say should say nothing rather than guess.
type Catalog interface {
	ListModels(ctx context.Context) ([]Model, error)
}

// Model is one entry of a provider's catalog, trimmed to what the picker needs.
type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Free          bool   `json:"free"`
	// Tools reports whether the model advertises native tool calling. Models
	// without it fall back to the JSON protocol, which is less reliable.
	Tools bool `json:"tools"`
}

type MsgBlock struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID and Name belong to a role:"tool" message, and name the call it
	// answers. A tool result delivered without them is just another user turn,
	// which is what the model reads it as.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// ToolCall is a native tool call returned by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name string `json:"name"`
	// Arguments is a JSON object encoded as a string, per the OpenAI schema.
	Arguments string `json:"arguments"`
}

// Tool advertises a callable function to the model.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// Result is one model turn: free text, native tool calls, or both.
type Result struct {
	Content   string
	ToolCalls []ToolCall
}

// ChatOptions selects how the model should shape its reply. Tools and ForceJSON
// are mutually exclusive: several providers suppress tool calls when a JSON
// response format is set, so ForceJSON is meant as a fallback, not an addition.
type ChatOptions struct {
	Tools []Tool
	// ForceJSON constrains the reply to a single JSON object via response_format.
	ForceJSON bool
	// Model overrides the client default for this request. Empty uses the default.
	Model string
}
