//go:build windows

package mcp

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Protocol revisions. 2026-07-28 is stateless: every request names its revision in params._meta. The older ones
// ("legacy") negotiate it once with initialize. One process serves both (the spec's dual-era server), because Claude
// Code probes 2026-07-28 first while Claude Desktop and older clients only know initialize.
const modernVersion = "2026-07-28"

var (
	modernVersions = []string{modernVersion}
	// Newest first; the first is offered when initialize asks for anything else. Revisions before 2025-06-18 lack
	// title, outputSchema and structuredContent, which the tools rely on.
	legacyVersions = []string{"2025-11-25", "2025-06-18"}
)

const (
	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaServerInfo         = "io.modelcontextprotocol/serverInfo"
	metaSubscriptionID     = "io.modelcontextprotocol/subscriptionId"
	metaProgressToken      = "progressToken"

	resultComplete = "complete"
	// The tool list and server description are fixed for the life of the binary, so clients may cache them for an
	// hour and share the cache (2026-07-28 caching).
	listTTLMs  = 3600000
	cacheScope = "public"
)

// JSON-RPC error codes. -32020..-32099 are reserved by MCP; only the ones it defines may be used.
const (
	codeParseError                 = -32700
	codeInvalidRequest             = -32600
	codeMethodNotFound             = -32601
	codeInvalidParams              = -32602
	codeInternalError              = -32603
	codeUnsupportedProtocolVersion = -32022
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// response: ID is written back exactly as received; nil marshals as null (an id that could not be read).
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// serverCapabilities is {"tools":{}}: no listChanged (the list is static), no logging (deprecated in 2026-07-28;
// diagnostics go to stderr), no resources / prompts.
type serverCapabilities struct {
	Tools struct{} `json:"tools"`
}

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

type discoverResult struct {
	ResultType        string             `json:"resultType"`
	SupportedVersions []string           `json:"supportedVersions"`
	Capabilities      serverCapabilities `json:"capabilities"`
	Instructions      string             `json:"instructions,omitempty"`
	TTLMs             int                `json:"ttlMs"`
	CacheScope        string             `json:"cacheScope"`
	Meta              map[string]any     `json:"_meta,omitempty"`
}

// listToolsResult: the 2026-07-28 fields are empty (omitted) for legacy requests.
type listToolsResult struct {
	ResultType string         `json:"resultType,omitempty"`
	Tools      []*toolDef     `json:"tools"`
	TTLMs      int            `json:"ttlMs,omitempty"`
	CacheScope string         `json:"cacheScope,omitempty"`
	Meta       map[string]any `json:"_meta,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// callToolResult: StructuredContent is always a JSON object (all that 2025-06-18 / 2025-11-25 allow) and the text
// block carries the same JSON for clients that only read text. Error results have text only.
type callToolResult struct {
	ResultType        string          `json:"resultType,omitempty"`
	Content           []textContent   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError"`
	Meta              map[string]any  `json:"_meta,omitempty"`
}

type progressParams struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      float64         `json:"progress"`
	Total         float64         `json:"total,omitempty"`
	Message       string          `json:"message,omitempty"`
}

// idKey is the in-flight table key of a request id. MCP allows strings and integers only (no null, no fractions);
// the tag keeps 7 and "7" apart, and decoding the string makes "7" and "7" the same id.
func idKey(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	switch c := raw[0]; {
	case c == '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return "s:" + s, true
	case c == '-' || (c >= '0' && c <= '9'):
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return "", false
		}
		return "n:" + strconv.FormatInt(n, 10), true
	}
	return "", false
}

// progressTokenOf returns the token when it is a string or a number (anything else means no progress), as received.
func progressTokenOf(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if c := raw[0]; c == '"' || c == '-' || (c >= '0' && c <= '9') {
		return raw
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || string(raw) == "null"
}

func isObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

// encodeJSON is compact JSON without HTML escaping (shell output stays readable), no trailing newline. Marshal
// escapes control characters inside strings, so the result never contains a raw newline.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
