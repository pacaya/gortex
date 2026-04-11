package lsp

// LSP protocol types — minimal subset needed for semantic enrichment.
// Based on LSP 3.17.

// InitializeParams is sent as the first request to the server.
type InitializeParams struct {
	ProcessID    int                `json:"processId"`
	RootURI      string             `json:"rootUri"`
	Capabilities ClientCapabilities `json:"capabilities"`
}

// ClientCapabilities declares what the client supports.
type ClientCapabilities struct {
	TextDocument TextDocumentClientCapabilities `json:"textDocument,omitempty"`
}

// TextDocumentClientCapabilities declares text document capabilities.
type TextDocumentClientCapabilities struct {
	Implementation *ImplementationCapability `json:"implementation,omitempty"`
	References     *ReferencesCapability     `json:"references,omitempty"`
	Definition     *DefinitionCapability     `json:"definition,omitempty"`
	Hover          *HoverCapability          `json:"hover,omitempty"`
}

// ImplementationCapability declares implementation request support.
type ImplementationCapability struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
}

// ReferencesCapability declares references request support.
type ReferencesCapability struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
}

// DefinitionCapability declares definition request support.
type DefinitionCapability struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
}

// HoverCapability declares hover request support.
type HoverCapability struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
	ContentFormat       []string `json:"contentFormat,omitempty"`
}

// InitializeResult is the server's response to initialize.
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
}

// ServerCapabilities declares what the server supports.
type ServerCapabilities struct {
	ImplementationProvider any  `json:"implementationProvider,omitempty"`
	ReferencesProvider     any  `json:"referencesProvider,omitempty"`
	DefinitionProvider     any  `json:"definitionProvider,omitempty"`
	HoverProvider          any  `json:"hoverProvider,omitempty"`
	CallHierarchyProvider  any  `json:"callHierarchyProvider,omitempty"`
	TypeHierarchyProvider  any  `json:"typeHierarchyProvider,omitempty"`
}

// TextDocumentIdentifier identifies a text document.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// TextDocumentPositionParams identifies a position in a text document.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// Position in a text document (0-indexed).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Location represents a location in a document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Range in a text document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// DidOpenTextDocumentParams is sent when a document is opened.
type DidOpenTextDocumentParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// TextDocumentItem is a text document with content.
type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// ReferenceParams extends TextDocumentPositionParams with reference context.
type ReferenceParams struct {
	TextDocumentPositionParams
	Context ReferenceContext `json:"context"`
}

// ReferenceContext controls what references are returned.
type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// ImplementationParams is the params for textDocument/implementation.
type ImplementationParams struct {
	TextDocumentPositionParams
}

// HoverParams is the params for textDocument/hover.
type HoverParams struct {
	TextDocumentPositionParams
}

// HoverResult is the response for textDocument/hover.
type HoverResult struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

// MarkupContent represents hover content.
type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// CallHierarchyPrepareParams is the params for callHierarchy/prepare.
type CallHierarchyPrepareParams struct {
	TextDocumentPositionParams
}

// CallHierarchyItem represents an item in the call hierarchy.
type CallHierarchyItem struct {
	Name           string `json:"name"`
	Kind           int    `json:"kind"`
	URI            string `json:"uri"`
	Range          Range  `json:"range"`
	SelectionRange Range  `json:"selectionRange"`
}

// CallHierarchyIncomingCallsParams is the params for callHierarchy/incomingCalls.
type CallHierarchyIncomingCallsParams struct {
	Item CallHierarchyItem `json:"item"`
}

// CallHierarchyIncomingCall represents an incoming call.
type CallHierarchyIncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

// CallHierarchyOutgoingCallsParams is the params for callHierarchy/outgoingCalls.
type CallHierarchyOutgoingCallsParams struct {
	Item CallHierarchyItem `json:"item"`
}

// CallHierarchyOutgoingCall represents an outgoing call.
type CallHierarchyOutgoingCall struct {
	To       CallHierarchyItem `json:"to"`
	FromRanges []Range         `json:"fromRanges"`
}
