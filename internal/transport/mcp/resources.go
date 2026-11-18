package mcp

import (
	"context"
	"encoding/json"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"centrext/internal/application"
	"centrext/internal/domain"
)

func jsonResource(uri string, v any) (*sdk.ReadResourceResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(b)}}}, nil
}

func textResource(uri, text string) *sdk.ReadResourceResult {
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: "text/plain", Text: text}}}
}

func (s *Server) registerResources(srv *sdk.Server) {
	srv.AddResource(&sdk.Resource{URI: "centrext://namespaces", Name: "namespaces",
		Description: "Namespaces this agent can access", MIMEType: "application/json"},
		func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			nss, err := s.svc.ListNamespaces(ctx, s.actor)
			if err != nil {
				return nil, toolError(err)
			}
			out := []NamespaceRecord{}
			for _, n := range nss {
				out = append(out, nsRecord(n))
			}
			return jsonResource(req.Params.URI, out)
		})

	srv.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: "centrext://namespace/{id}/overview", Name: "namespace-overview",
		Description: "Session summary: key decisions and conventions", MIMEType: "text/plain"},
		func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			id, ok := between(req.Params.URI, "centrext://namespace/", "/overview")
			if !ok {
				return nil, sdk.ResourceNotFoundError(req.Params.URI)
			}
			sum, err := s.svc.Summary(ctx, s.actor, id, 1000)
			if err != nil {
				return nil, toolError(err)
			}
			return textResource(req.Params.URI, sum), nil
		})

	srv.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: "centrext://namespace/{id}/decisions", Name: "namespace-decisions",
		Description: "All active decisions, architecture and conventions", MIMEType: "application/json"},
		func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			id, ok := between(req.Params.URI, "centrext://namespace/", "/decisions")
			if !ok {
				return nil, sdk.ResourceNotFoundError(req.Params.URI)
			}
			views, err := s.svc.ListContexts(ctx, s.actor, application.ListQuery{NamespaceID: id,
				Types: []domain.ContextType{domain.TypeDecision, domain.TypeArchitecture, domain.TypeConvention}})
			if err != nil {
				return nil, toolError(err)
			}
			out := []ContextRecord{}
			for _, v := range views {
				out = append(out, record(v))
			}
			return jsonResource(req.Params.URI, out)
		})

	srv.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: "centrext://context/{id}", Name: "context",
		Description: "One saved context", MIMEType: "application/json"},
		func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			id := strings.TrimPrefix(req.Params.URI, "centrext://context/")
			v, err := s.svc.GetContext(ctx, s.actor, id)
			if err != nil {
				return nil, sdk.ResourceNotFoundError(req.Params.URI)
			}
			return jsonResource(req.Params.URI, record(v))
		})
}

func between(s, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
		return "", false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(s, prefix), suffix)
	return mid, mid != "" && !strings.Contains(mid, "/")
}

func (s *Server) registerPrompts(srv *sdk.Server) {
	srv.AddPrompt(&sdk.Prompt{Name: "project-onboarding", Description: "Load this project's shared decisions and conventions from Centrext"},
		func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
			r, err := s.namespace(ctx, req.Session)
			if err != nil {
				return nil, toolError(err)
			}
			sum, err := s.svc.Summary(ctx, s.actor, r.Namespace.ID, 1500)
			if err != nil {
				return nil, toolError(err)
			}
			return &sdk.GetPromptResult{Description: "Centrext onboarding for " + r.Namespace.Name,
				Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: sum + "\n" + Guide}}}}, nil
		})
	srv.AddPrompt(&sdk.Prompt{Name: "context-capture", Description: "How to save knowledge to Centrext in this session"},
		func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
			return &sdk.GetPromptResult{Description: "Centrext capture rules",
				Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: Guide}}}}, nil
		})
}
