package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/torana-edge/torana-edge/internal/plugin"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

type agentAPIOperation struct {
	ID           string          `json:"id"`
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	Description  string          `json:"description"`
	Risk         string          `json:"risk"`
	Idempotent   bool            `json:"idempotent"`
	ContentType  string          `json:"content_type"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Plugin       string          `json:"plugin,omitempty"`
	PluginDigest string          `json:"plugin_digest,omitempty"`
}

type agentAPIDocument struct {
	Name        string              `json:"name"`
	APIVersion  string              `json:"api_version"`
	Description string              `json:"description"`
	BasePath    string              `json:"base_path"`
	Security    agentAPISecurity    `json:"security"`
	Operations  []agentAPIOperation `json:"operations"`
}

type agentAPISecurity struct {
	Scope               string `json:"scope"`
	MutationHeader      string `json:"mutation_header"`
	MutationHeaderValue string `json:"mutation_header_value"`
	Note                string `json:"note"`
}

type agentAPIErrorEnvelope struct {
	Error agentAPIError `json:"error"`
}

type agentAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type pluginHTTPDispatcher interface {
	RunOnHTTPRequest(context.Context, uint64, string, *pb.HttpRequest, map[string][]string) (*pb.HttpResponse, error)
	EndRequest(uint64)
}

// dispatchPluginHTTPRequest runs one HTTP dispatch through the dispatcher
// (the pinned pipeline in production) and guarantees request cleanup. It is
// deliberately small: the routes only know how to build the request, and
// dispatch only knows how to run it. rawHeaders is the caller's incoming
// header map; the dispatch boundary (PluginPipeline.RunOnHTTPRequest)
// snapshots and filters it.
func dispatchPluginHTTPRequest(ctx context.Context, dispatcher pluginHTTPDispatcher, pluginName string, request *pb.HttpRequest, rawHeaders map[string][]string) (*pb.HttpResponse, error) {
	requestID := reqCounter.Add(1)
	defer dispatcher.EndRequest(requestID)
	return dispatcher.RunOnHTTPRequest(ctx, requestID, pluginName, request, rawHeaders)
}

type bufferedAgentResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedAgentResponse() *bufferedAgentResponse {
	return &bufferedAgentResponse{header: make(http.Header)}
}

func (response *bufferedAgentResponse) Header() http.Header {
	return response.header
}

func (response *bufferedAgentResponse) WriteHeader(status int) {
	if response.status == 0 {
		response.status = status
	}
}

func (response *bufferedAgentResponse) Write(body []byte) (int, error) {
	if response.status == 0 {
		response.status = http.StatusOK
	}
	return response.body.Write(body)
}

func flushAgentResponse(w http.ResponseWriter, captured *bufferedAgentResponse) {
	status := captured.status
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 400 {
		code := "request_failed"
		switch status {
		case http.StatusBadRequest:
			code = "invalid_request"
		case http.StatusForbidden:
			code = "forbidden"
		case http.StatusNotFound:
			code = "not_found"
		case http.StatusMethodNotAllowed:
			code = "method_not_allowed"
		case http.StatusConflict:
			code = "conflict"
		case http.StatusRequestEntityTooLarge:
			code = "body_too_large"
		case http.StatusServiceUnavailable:
			code = "service_unavailable"
		}
		message := strings.TrimSpace(captured.body.String())
		if message == "" {
			message = http.StatusText(status)
		}
		if allow := captured.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		writeAgentError(w, status, code, message)
		return
	}
	for name, values := range captured.header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(captured.body.Bytes())
}

var arbitraryObjectSchema = json.RawMessage(`{"type":"object","additionalProperties":true}`)
var discoveryDocumentSchema = json.RawMessage(`{"type":"object","required":["name","api_version","description","base_path","security","operations"],"properties":{"name":{"type":"string"},"api_version":{"const":"v1"},"description":{"type":"string"},"base_path":{"type":"string"},"security":{"type":"object","additionalProperties":true},"operations":{"type":"array","items":{"type":"object","additionalProperties":true}}},"additionalProperties":false}`)
var pluginListSchema = json.RawMessage(`{"type":"object","required":["dir","plugins"],"properties":{"dir":{"type":"string"},"plugins":{"type":"array","items":{"type":"object","required":["name","enabled"],"properties":{"id":{"type":"string"},"name":{"type":"string"},"version":{"type":"string"},"digest":{"type":"string"},"enabled":{"type":"boolean"},"hooks":{"type":"array","items":{"type":"string"}},"permissions":{"type":"array","items":{"type":"string"}}},"additionalProperties":true}}},"additionalProperties":false}`)
var statsSchema = json.RawMessage(`{"type":"object","required":["total_requests","total_bytes_in","total_bytes_out","total_tokens_in","total_tokens_out"],"properties":{"total_requests":{"type":"integer"},"total_bytes_in":{"type":"integer"},"total_bytes_out":{"type":"integer"},"total_tokens_in":{"type":"integer"},"total_tokens_out":{"type":"integer"},"total_cache_read_tokens":{"type":"integer"},"total_cache_write_tokens":{"type":"integer"},"compactions":{"type":"integer"},"bytes_saved":{"type":"integer"}},"additionalProperties":true}`)
var feedSchema = json.RawMessage(`{"type":"array","items":{"type":"object","required":["timestamp","provider","model","status","latency_ms","tokens_in","tokens_out","bytes_in","bytes_out"],"properties":{"timestamp":{"type":"string"},"provider":{"type":"string"},"model":{"type":"string"},"status":{"type":"integer"},"latency_ms":{"type":"number"},"tokens_in":{"type":"integer"},"tokens_out":{"type":"integer"},"bytes_in":{"type":"integer"},"bytes_out":{"type":"integer"}},"additionalProperties":true}}`)

var conversationsSchema = json.RawMessage(`{"type":"object","required":["conversations"],"properties":{"conversations":{"type":"array","items":{"type":"object","required":["id","cache_prefix_key","provider","model","last_active","turns"],"properties":{"id":{"type":"string"},"cache_prefix_key":{"type":"string"},"provider":{"type":"string"},"model":{"type":"string"},"format":{"type":"string"},"path":{"type":"string"},"first_seen":{"type":"string"},"last_active":{"type":"string"},"turns":{"type":"integer"},"last_cache_read":{"type":"integer"},"last_cache_write":{"type":"integer"}},"additionalProperties":true}}},"additionalProperties":true}`)

func writeAgentJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAgentError(w http.ResponseWriter, status int, code, message string) {
	writeAgentJSON(w, status, agentAPIErrorEnvelope{
		Error: agentAPIError{Code: code, Message: message},
	})
}

// validateAgentResponseHeaders deliberately accepts only a JSON content type.
// Agent routes contain loopback control-plane data, so plugin-supplied CORS,
// caching, framing, encoding, and length headers must never reach the client.
func validateAgentResponseHeaders(encoded []byte) error {
	if len(encoded) == 0 {
		return nil
	}
	if len(encoded) > maxPluginResponseHeaderBytes {
		return fmt.Errorf("encoded agent response headers too large")
	}
	var headers map[string][]string
	if err := json.Unmarshal(encoded, &headers); err != nil {
		return err
	}
	if len(headers) > 1 {
		return fmt.Errorf("agent responses may only declare Content-Type")
	}
	for name, values := range headers {
		if !strings.EqualFold(name, "Content-Type") {
			return fmt.Errorf("header %q is not allowed on agent responses", name)
		}
		if len(values) != 1 {
			return fmt.Errorf("agent Content-Type must have one value")
		}
		mediaType, _, err := mime.ParseMediaType(values[0])
		if err != nil || mediaType != "application/json" {
			return fmt.Errorf("agent Content-Type must be application/json")
		}
	}
	return nil
}

func builtInAgentOperations() []agentAPIOperation {
	return []agentAPIOperation{
		{
			ID: "torana.control_plane.discover", Method: http.MethodGet,
			Path: "/_torana/api/v1/", Description: "Discover built-in and enabled plugin-contributed operations.",
			Risk: "read", Idempotent: true, ContentType: "application/json",
			OutputSchema: discoveryDocumentSchema,
		},
		{
			ID: "torana.config.get", Method: http.MethodGet,
			Path: "/_torana/api/v1/config", Description: "Read the effective configuration with secrets redacted.",
			Risk: "read", Idempotent: true, ContentType: "application/json",
			OutputSchema: arbitraryObjectSchema,
		},
		{
			ID: "torana.config.update", Method: http.MethodPut,
			Path: "/_torana/api/v1/config", Description: "Validate, apply, and persist provider, cache, limit, and ingress settings.",
			Risk: "write", Idempotent: true, ContentType: "application/json",
			InputSchema: arbitraryObjectSchema, OutputSchema: arbitraryObjectSchema,
		},
		{
			ID: "torana.plugins.list", Method: http.MethodGet,
			Path: "/_torana/api/v1/plugins", Description: "List discovered plugins, digests, approvals, configuration, and enabled order.",
			Risk: "read", Idempotent: true, ContentType: "application/json",
			OutputSchema: pluginListSchema,
		},
		{
			ID: "torana.plugins.update", Method: http.MethodPut,
			Path: "/_torana/api/v1/plugins", Description: "Atomically update plugin order, configuration, and digest-bound approvals.",
			Risk: "write", Idempotent: true, ContentType: "application/json",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"order":{"type":"array","items":{"type":"string"}},"config":{"type":"object"},"approvals":{"type":"object"}},"additionalProperties":false}`),
			OutputSchema: arbitraryObjectSchema,
		},
		{
			ID: "torana.plugin.config.update", Method: http.MethodPost,
			Path: "/_torana/api/v1/plugins/{name}/config", Description: "Update one installed plugin's JSON configuration and rebuild the pipeline.",
			Risk: "write", Idempotent: true, ContentType: "application/json",
			InputSchema: arbitraryObjectSchema, OutputSchema: arbitraryObjectSchema,
		},
		{
			ID: "torana.stats.get", Method: http.MethodGet,
			Path: "/_torana/api/v1/stats", Description: "Read current aggregate proxy and plugin statistics.",
			Risk: "read", Idempotent: true, ContentType: "application/json",
			OutputSchema: statsSchema,
		},
		{
			ID: "torana.feed.list", Method: http.MethodGet,
			Path: "/_torana/api/v1/feed", Description: "Read the bounded recent request-event snapshot, newest first.",
			Risk: "read", Idempotent: true, ContentType: "application/json",
			OutputSchema: feedSchema,
		},
		{
			ID: "torana.conversations.list", Method: http.MethodGet,
			Path: "/_torana/api/v1/conversations", Description: "List conversations seen recently, most recently active first. Metadata only — no message content.",
			Risk: "read", Idempotent: true, ContentType: "application/json",
			OutputSchema: conversationsSchema,
		},
	}
}

func (s *Server) agentAPIDiscovery() agentAPIDocument {
	operations := builtInAgentOperations()
	rawPipeline := s.pluginPipeline.Load()
	if rawPipeline != nil {
		pipeline := rawPipeline.(*plugin.PluginPipeline)
		if pipeline.TryAcquire() {
			for _, loaded := range pipeline.AgentPlugins() {
				for _, operation := range loaded.Descriptor.Operations {
					operations = append(operations, agentAPIOperation{
						ID:           "plugin:" + loaded.Manifest.Name + ":" + operation.ID,
						Method:       operation.Method,
						Path:         "/_torana/api/v1/agent/plugins/" + url.PathEscape(loaded.Manifest.Name) + operation.Path,
						Description:  operation.Description,
						Risk:         operation.Risk,
						Idempotent:   operation.Idempotent,
						ContentType:  "application/json",
						InputSchema:  operation.InputSchema,
						OutputSchema: operation.OutputSchema,
						Plugin:       loaded.Manifest.Name,
						PluginDigest: loaded.Digest,
					})
				}
			}
			pipeline.Release()
		}
	}
	return agentAPIDocument{
		Name:        "torana-control-plane",
		APIVersion:  "v1",
		Description: "Local, JSON-first Torana administration API for operators and software agents.",
		BasePath:    "/_torana/api/v1",
		Security: agentAPISecurity{
			Scope:               "loopback-only",
			MutationHeader:      "X-Torana-Local-Request",
			MutationHeaderValue: "1",
			Note:                "Non-browser mutations must send X-Torana-Local-Request: 1; browsers use a matching loopback Origin.",
		},
		Operations: operations,
	}
}

func (s *Server) handlePluginAgentOperation(w http.ResponseWriter, r *http.Request) {
	const prefix = "/_torana/api/v1/agent/plugins/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	separator := strings.IndexByte(rest, '/')
	if separator <= 0 {
		writeAgentError(w, http.StatusNotFound, "operation_not_found", "plugin operation path is required")
		return
	}
	pluginName, err := url.PathUnescape(rest[:separator])
	if err != nil || pluginName == "" {
		writeAgentError(w, http.StatusBadRequest, "invalid_plugin", "plugin name is invalid")
		return
	}
	operationPath := rest[separator:]
	rawPipeline := s.pluginPipeline.Load()
	if rawPipeline == nil {
		writeAgentError(w, http.StatusServiceUnavailable, "pipeline_unavailable", "plugin pipeline is not available")
		return
	}
	pipeline := rawPipeline.(*plugin.PluginPipeline)
	if !pipeline.TryAcquire() {
		writeAgentError(w, http.StatusServiceUnavailable, "pipeline_draining", "plugin pipeline is reloading")
		return
	}
	defer pipeline.Release()

	operation, allowed, found := pipeline.FindAgentOperation(pluginName, r.Method, operationPath)
	if !found {
		writeAgentError(w, http.StatusNotFound, "operation_not_found", "enabled plugin operation was not found")
		return
	}
	if operation == nil {
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed for this plugin operation")
			return
		}
		writeAgentError(w, http.StatusNotFound, "operation_not_found", "plugin operation was not found")
		return
	}

	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
		r.Body.Close()
		if err != nil {
			writeAgentError(w, http.StatusBadRequest, "body_read_failed", "request body could not be read")
			return
		}
		if len(body) > maxBodySize {
			writeAgentError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("request body exceeds %d bytes", maxBodySize))
			return
		}
	}
	if len(body) > 0 && !json.Valid(body) {
		writeAgentError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
		return
	}
	if len(body) > 0 {
		mediaType, _, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if parseErr != nil || mediaType != "application/json" {
			writeAgentError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "request body requires Content-Type: application/json")
			return
		}
	}
	if err := plugin.ValidateAgentPayload(operation.InputSchema, body); err != nil {
		writeAgentError(w, http.StatusBadRequest, "schema_validation_failed", err.Error())
		return
	}

	request := &pb.HttpRequest{
		Method:     r.Method,
		Path:       "/agent" + operation.Path,
		Query:      r.URL.RawQuery,
		Scheme:     requestScheme(r),
		RemoteAddr: r.RemoteAddr,
		Body:       body,
	}
	// Headers are NOT selected here: the raw incoming map travels to the
	// dispatch boundary, which applies the same three-class header policy as
	// the plugin route. The agent route's historical four-header subset is
	// subsumed by that single policy (its X-Torana-Agent entry is never
	// forwarded under the approved contract).
	response, dispatchErr := dispatchPluginHTTPRequest(r.Context(), pipeline, pluginName, request, r.Header)
	if dispatchErr != nil {
		if errors.Is(dispatchErr, plugin.ErrServeHTTPForbidden) {
			writeAgentError(w, http.StatusForbidden, "plugin_permission_denied", "plugin lacks env.serve_http approval")
			return
		}
		log.Printf("[proxy] agent plugin %s operation %s: %v", pluginName, operation.ID, dispatchErr)
		writeAgentError(w, http.StatusServiceUnavailable, "plugin_dispatch_failed", "plugin operation could not be completed")
		return
	}
	if response == nil {
		writeAgentError(w, http.StatusBadGateway, "plugin_did_not_handle", "plugin did not handle its advertised operation")
		return
	}
	if len(response.Body) > maxBodySize {
		writeAgentError(w, http.StatusBadGateway, "plugin_response_too_large", fmt.Sprintf("plugin operation response exceeds %d bytes", maxBodySize))
		return
	}
	status := int(response.Status)
	if status == 0 {
		status = http.StatusOK
	}
	if status < 200 || status > 299 || status == http.StatusNoContent || status == http.StatusResetContent {
		writeAgentError(w, http.StatusBadGateway, "plugin_operation_failed", "plugin operation did not return a JSON success response")
		return
	}
	if !json.Valid(response.Body) {
		writeAgentError(w, http.StatusBadGateway, "plugin_invalid_json", "plugin operation returned a non-JSON body")
		return
	}
	if err := plugin.ValidateAgentPayload(operation.OutputSchema, response.Body); err != nil {
		writeAgentError(w, http.StatusBadGateway, "plugin_schema_violation", "plugin operation response does not match its advertised schema")
		return
	}
	if err := validateAgentResponseHeaders(response.HeadersJson); err != nil {
		writeAgentError(w, http.StatusBadGateway, "plugin_invalid_headers", "plugin operation returned invalid response headers")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(response.Body) > 0 {
		_, _ = w.Write(response.Body)
	}
}
