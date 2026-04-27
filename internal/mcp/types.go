// Package mcp implements a minimal Model Context Protocol server over stdio.
//
// MCP is JSON-RPC 2.0, newline-delimited on stdio. We implement just enough
// of it to expose shipd CLI verbs as agent-callable tools — initialize,
// tools/list, tools/call, plus the standard initialized notification.
//
// Spec reference: https://spec.modelcontextprotocol.io/specification/
package mcp

import (
	"encoding/json"
)

const protocolVersion = "2024-11-05"

// Envelope is a JSON-RPC 2.0 message. Either Method or (Result/Error) is set,
// never both. ID is omitted for notifications.
type Envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// --- MCP method payloads ---

type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ClientInfo      ClientInfo     `json:"clientInfo,omitempty"`
}

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      ServerInfo     `json:"serverInfo"`
}

type ListToolsResult struct {
	Tools []ToolSpec `json:"tools"`
}

// ToolSpec is the schema description an MCP client uses to render the tool to the LLM.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallToolResult is what the LLM sees as the tool's output.
type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func textResult(s string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: s}}}
}

func textError(s string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: s}}, IsError: true}
}
