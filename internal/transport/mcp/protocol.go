// Package mcp serves LAC over the Model Context Protocol, so an AI tool such as Claude Code or
// Codex discovers the coordinator by itself instead of being taught a command line.
//
// It speaks JSON-RPC 2.0 over stdin and stdout, as MCP requires, and it is a thin adapter: every
// tool call goes through the same client the CLI uses, so there is one implementation of LAC's
// behaviour, not two.
package mcp

import (
	"encoding/json"
	"fmt"
)

// jsonrpcVersion is the only protocol version MCP uses.
const jsonrpcVersion = "2.0"

// supportedProtocolVersions are the MCP revisions this server knows how to speak, newest first.
//
// Version negotiation matters here: the client states what it wants, and a server that answers
// with something the client cannot read is simply broken. We echo back whatever the client asked
// for when we know it, and otherwise offer our newest and let the client decide.
var supportedProtocolVersions = []string{
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

// preferredProtocolVersion is what we offer a client whose request we do not recognise.
const preferredProtocolVersion = "2025-06-18"

// negotiateProtocol returns the version to answer with.
func negotiateProtocol(requested string) string {
	for _, known := range supportedProtocolVersions {
		if requested == known {
			return requested
		}
	}

	return preferredProtocolVersion
}

// request is an incoming MCP call. A request without an id is a notification and gets no reply.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r request) isNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

// response is a reply. Exactly one of Result and Error is set.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a protocol-level failure: a method that does not exist, parameters that do not
// parse. A tool that runs and fails is not this — that is reported inside the tool result, which
// is what lets the model read the failure and try something else.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// The JSON-RPC codes MCP uses.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

func errorf(code int, format string, arguments ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, arguments...)}
}

// initializeParams is what the client says about itself.
type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// initializeResult tells the client what this server is and what it offers.
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfo         `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

type serverCapabilities struct {
	Tools     *toolsCapability     `json:"tools,omitempty"`
	Resources *resourcesCapability `json:"resources,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type resourcesCapability struct {
	Subscribe   bool `json:"subscribe"`
	ListChanged bool `json:"listChanged"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// tool is one thing the model can do.
type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolListResult struct {
	Tools []tool `json:"tools"`
}

// callToolParams is a tool invocation.
type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// callToolResult is what the model gets back.
//
// A tool that fails sets IsError and describes the failure in the content, rather than returning a
// protocol error: the model is supposed to read it and decide what to do, and a protocol error
// never reaches the model at all.
type callToolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textResult(text string) callToolResult {
	return callToolResult{Content: []content{{Type: "text", Text: text}}}
}

func errorResult(format string, arguments ...any) callToolResult {
	return callToolResult{
		Content: []content{{Type: "text", Text: fmt.Sprintf(format, arguments...)}},
		IsError: true,
	}
}

// resource is a read-only view the client can fetch.
type resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

type resourceListResult struct {
	Resources []resource `json:"resources"`
}

type readResourceParams struct {
	URI string `json:"uri"`
}

type readResourceResult struct {
	Contents []resourceContents `json:"contents"`
}

type resourceContents struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}
