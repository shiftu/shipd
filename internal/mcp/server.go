package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
)

// Tool is implemented by every tool registered with the server. The handler
// receives raw JSON arguments and returns a tool result that will be shown to
// the LLM.
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, args json.RawMessage) (*CallToolResult, error)
}

// Server speaks MCP over an io.Reader/io.Writer pair (typically stdin/stdout).
// Logs go to a separate logger so they don't pollute the JSON-RPC channel.
//
// Server embeds *Registry so callers can `srv.Register(tool)` directly.
type Server struct {
	*Registry
	name    string
	version string
	log     *log.Logger

	wmu sync.Mutex // serializes writes
}

func NewServer(name, version string, logger *log.Logger) *Server {
	return &Server{
		Registry: NewRegistry(),
		name:     name,
		version:  version,
		log:      logger,
	}
}

// Serve reads JSON-RPC messages from r line-by-line and writes responses to w.
// It returns when r reaches EOF or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			s.handleLine(ctx, line, w)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *Server) handleLine(ctx context.Context, line []byte, w io.Writer) {
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		s.log.Printf("parse error: %v", err)
		s.writeErr(w, nil, codeParseError, "parse error")
		return
	}
	if env.JSONRPC != "2.0" {
		s.writeErr(w, env.ID, codeInvalidRequest, "jsonrpc must be 2.0")
		return
	}

	// Notifications (no ID) get no response.
	isNotification := env.ID == nil

	switch env.Method {
	case "initialize":
		var p InitializeParams
		_ = json.Unmarshal(env.Params, &p)
		s.writeResult(w, env.ID, InitializeResult{
			ProtocolVersion: protocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      ServerInfo{Name: s.name, Version: s.version},
		})
	case "notifications/initialized", "initialized":
		// no-op
	case "tools/list":
		var specs []ToolSpec
		for _, t := range s.List() {
			specs = append(specs, t.Spec())
		}
		s.writeResult(w, env.ID, ListToolsResult{Tools: specs})
	case "tools/call":
		var p CallToolParams
		if err := json.Unmarshal(env.Params, &p); err != nil {
			s.writeErr(w, env.ID, codeInvalidParams, err.Error())
			return
		}
		t, ok := s.Get(p.Name)
		if !ok {
			s.writeResult(w, env.ID, textError(fmt.Sprintf("unknown tool %q", p.Name)))
			return
		}
		res, err := t.Call(ctx, p.Arguments)
		if err != nil {
			// Tool errors come back as a CallToolResult with isError=true so the LLM can read them.
			s.writeResult(w, env.ID, textError(err.Error()))
			return
		}
		s.writeResult(w, env.ID, res)
	case "ping":
		s.writeResult(w, env.ID, map[string]any{})
	default:
		if isNotification {
			return
		}
		s.writeErr(w, env.ID, codeMethodNotFound, "unknown method "+env.Method)
	}
}

func (s *Server) writeResult(w io.Writer, id *json.RawMessage, result any) {
	if id == nil {
		return
	}
	s.writeEnvelope(w, Envelope{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeErr(w io.Writer, id *json.RawMessage, code int, msg string) {
	if id == nil {
		// JSON-RPC says we shouldn't respond to notifications, even with errors.
		s.log.Printf("notification error: %d %s", code, msg)
		return
	}
	s.writeEnvelope(w, Envelope{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}})
}

func (s *Server) writeEnvelope(w io.Writer, env Envelope) {
	env.JSONRPC = "2.0"
	b, err := json.Marshal(env)
	if err != nil {
		s.log.Printf("marshal error: %v", err)
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n"))
}
