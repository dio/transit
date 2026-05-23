package mcpprofilegateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
	"github.com/stretchr/testify/require"
)

func TestGateway_forwardsPublicCatalogServerToOwningL2(t *testing.T) {
	var gotPath, gotCredRef, gotCredEnvelope string
	l2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCredRef = r.Header.Get("x-mcp-credential-ref")
		gotCredEnvelope = r.Header.Get("x-mcp-credential-envelope")
		w.Header().Set(mcpprofilerouter.SessionIDHeader, "l2-session")
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Result: mcpprofilerouter.InitializeResult{
				ProtocolVersion: mcpprofilerouter.ProtocolVersion,
				Capabilities:    map[string]any{"tools": map[string]any{}},
				ServerInfo:      mcpprofilerouter.Implementation{Name: "l2-aws"},
			},
		})
	}))
	defer l2.Close()

	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{
			"aws-knowledge": {URL: l2.URL},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/s/aws-knowledge", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(t, mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "gateway-test"},
		}),
	}, map[string]string{
		"x-mcp-credential-ref":      "profile/aws/user",
		"x-mcp-credential-envelope": "opaque",
	})
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/mcp/s/aws-knowledge", gotPath)
	require.Empty(t, gotCredRef)
	require.Empty(t, gotCredEnvelope)
	require.Equal(t, "l2-session", resp.Header.Get(mcpprofilerouter.SessionIDHeader))
}

func TestGateway_catalogCalloutRequestBlanksProfileCredentialHeaders(t *testing.T) {
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{
			"aws-knowledge": {URL: "http://l2-catalog.local", Cluster: "l2"},
		},
	})
	req, err := gateway.catalogCalloutRequest(transitRequest{
		path: "/mcp/s/aws-knowledge",
		headers: [][2]string{
			{"content-type", "application/json"},
			{"x-mcp-credential-ref", "client-supplied"},
			{"x-mcp-credential-envelope", "client-supplied-envelope"},
		},
	}, []byte(`{}`), "aws-knowledge", CatalogServer{URL: "http://l2-catalog.local", Cluster: "l2"})
	require.NoError(t, err)

	require.Equal(t, "", headerValue(req.Headers, "x-mcp-credential-ref"))
	require.Equal(t, "", headerValue(req.Headers, "x-mcp-credential-envelope"))
}

func TestGateway_profileToolsListFansOutAndNamespaces(t *testing.T) {
	var awsCredRef, githubEnvelope string
	aws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		awsCredRef = r.Header.Get("x-mcp-credential-ref")
		require.Equal(t, "/mcp/s/aws-knowledge", r.URL.Path)
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Result: mcpprofilerouter.ListToolsResult{Tools: []mcpprofilerouter.Tool{
				{Name: "read", Description: "read docs", InputSchema: map[string]any{"type": "object"}},
				{Name: "disabled", Description: "disabled", InputSchema: map[string]any{"type": "object"}},
			}},
		})
	}))
	defer aws.Close()
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		githubEnvelope = r.Header.Get("x-mcp-credential-envelope")
		require.Equal(t, "/mcp/s/github", r.URL.Path)
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Result: mcpprofilerouter.ListToolsResult{Tools: []mcpprofilerouter.Tool{
				{Name: "search", Description: "search repos", InputSchema: map[string]any{"type": "object"}},
			}},
		})
	}))
	defer github.Close()

	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{
			"aws-knowledge": {URL: aws.URL},
			"github":        {URL: github.URL},
		},
		Profiles: map[string]Profile{
			"profile": {
				Name:   "profile",
				APIKey: "profile-key",
				Servers: map[string]ProfileServer{
					"aws-knowledge": {
						URL:           aws.URL,
						Prefix:        "aws",
						CredentialRef: "profile/aws/user",
						EnabledTools:  map[string]bool{"read": true},
					},
					"github": {
						URL:                github.URL,
						Prefix:             "github",
						CredentialEnvelope: "opaque",
					},
				},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, map[string]string{"authorization": "Bearer profile-key"})
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, "profile/aws/user", awsCredRef)
	require.Equal(t, "opaque", githubEnvelope)

	var rpc mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.Unmarshal(body, &rpc))
	raw, err := json.Marshal(rpc.Result)
	require.NoError(t, err)
	var list mcpprofilerouter.ListToolsResult
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Tools, 2)
	require.Equal(t, []string{"aws.read", "github.search"}, []string{list.Tools[0].Name, list.Tools[1].Name})
}

func TestGateway_profileInitializePartialFailureCreatesSessionForHealthyBackends(t *testing.T) {
	var goodToolsListSession string
	var badToolsListReached bool
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/mcp/s/good", r.URL.Path)
		var req mcpprofilerouter.JSONRPCRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		switch req.Method {
		case mcpprofilerouter.MethodInitialize:
			w.Header().Set(mcpprofilerouter.SessionIDHeader, "good-backend-session")
			writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  syntheticInitializeResult(),
			})
		case mcpprofilerouter.MethodToolsList:
			goodToolsListSession = r.Header.Get(mcpprofilerouter.SessionIDHeader)
			writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: mcpprofilerouter.ListToolsResult{Tools: []mcpprofilerouter.Tool{
					{Name: "search", InputSchema: map[string]any{"type": "object"}},
				}},
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mcpprofilerouter.JSONRPCRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		if req.Method == mcpprofilerouter.MethodToolsList {
			badToolsListReached = true
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{
			"bad":  {URL: bad.URL},
			"good": {URL: good.URL},
		},
		Profiles: map[string]Profile{
			"profile": {
				Name: "profile",
				Servers: map[string]ProfileServer{
					"bad":  {URL: bad.URL},
					"good": {URL: good.URL, Prefix: "good"},
				},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	initResp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(t, mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "gateway-test"},
		}),
	}, nil)
	defer func() { _ = initResp.Body.Close() }()
	initBody, err := io.ReadAll(initResp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, initResp.StatusCode, string(initBody))
	profileSessionID := initResp.Header.Get(mcpprofilerouter.SessionIDHeader)
	require.NotEmpty(t, profileSessionID)
	require.NotEqual(t, "good-backend-session", profileSessionID)

	listResp := postRPCRaw(t, server.URL+"/mcp/profile", profileSessionID, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = listResp.Body.Close() }()
	listBody, err := io.ReadAll(listResp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, listResp.StatusCode, string(listBody))
	require.Equal(t, "good-backend-session", goodToolsListSession)
	require.False(t, badToolsListReached)

	var rpc mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.Unmarshal(listBody, &rpc))
	raw, err := json.Marshal(rpc.Result)
	require.NoError(t, err)
	var list mcpprofilerouter.ListToolsResult
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Equal(t, []string{"good.search"}, []string{list.Tools[0].Name})
}

func TestGateway_profileSessionDoesNotForwardL1EnvelopeToStatelessBackend(t *testing.T) {
	var toolsListSession string
	l2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mcpprofilerouter.JSONRPCRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		switch req.Method {
		case mcpprofilerouter.MethodInitialize:
			writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  syntheticInitializeResult(),
			})
		case mcpprofilerouter.MethodToolsList:
			toolsListSession = r.Header.Get(mcpprofilerouter.SessionIDHeader)
			writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  mcpprofilerouter.ListToolsResult{},
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer l2.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{"stateless": {URL: l2.URL}},
		Profiles: map[string]Profile{
			"profile": {
				Name:    "profile",
				Servers: map[string]ProfileServer{"stateless": {URL: l2.URL}},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	initResp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
	}, nil)
	defer func() { _ = initResp.Body.Close() }()
	require.Equal(t, http.StatusOK, initResp.StatusCode)
	profileSessionID := initResp.Header.Get(mcpprofilerouter.SessionIDHeader)
	require.NotEmpty(t, profileSessionID)

	listResp := postRPCRaw(t, server.URL+"/mcp/profile", profileSessionID, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = listResp.Body.Close() }()

	require.Equal(t, http.StatusOK, listResp.StatusCode)
	require.Empty(t, toolsListSession)
}

func TestGateway_profileInitializeAllFailuresReturnsError(t *testing.T) {
	var calls int
	l2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer l2.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{"bad": {URL: l2.URL}},
		Profiles: map[string]Profile{
			"profile": {
				Name:    "profile",
				Servers: map[string]ProfileServer{"bad": {URL: l2.URL}},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
	}, nil)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	require.Empty(t, resp.Header.Get(mcpprofilerouter.SessionIDHeader))
	require.Equal(t, 1, calls)
}

func TestGateway_profileToolsListInvalidSessionReachesNoL2(t *testing.T) {
	var reached bool
	l2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer l2.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{"github": {URL: l2.URL}},
		Profiles: map[string]Profile{
			"profile": {
				Name:    "profile",
				Servers: map[string]ProfileServer{"github": {URL: l2.URL}},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/profile", "not-a-profile-session", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.False(t, reached)
}

func TestProfileSessionEnvelopeRoundTrip(t *testing.T) {
	sessionID, err := encodeProfileSession("profile", map[string]string{
		"stateless": "",
		"stateful":  "backend-session",
	})
	require.NoError(t, err)
	require.NotContains(t, sessionID, "backend-session")

	session, ok := decodeProfileSession("profile", sessionID)
	require.True(t, ok)
	require.Equal(t, map[string]string{
		"stateless": "",
		"stateful":  "backend-session",
	}, session.Backends)

	_, ok = decodeProfileSession("other-profile", sessionID)
	require.False(t, ok)
}

func TestGateway_profileToolsListAuthFailureReachesNoL2(t *testing.T) {
	var reached bool
	l2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer l2.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{"github": {URL: l2.URL}},
		Profiles: map[string]Profile{
			"profile": {
				Name:    "profile",
				APIKey:  "profile-key",
				Servers: map[string]ProfileServer{"github": {URL: l2.URL}},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.False(t, reached)
}

func TestGateway_profileToolsListPartialFailureReturnsHealthyTools(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Result: mcpprofilerouter.ListToolsResult{Tools: []mcpprofilerouter.Tool{
				{Name: "search", InputSchema: map[string]any{"type": "object"}},
			}},
		})
	}))
	defer good.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{
			"bad":  {URL: bad.URL},
			"good": {URL: good.URL},
		},
		Profiles: map[string]Profile{
			"profile": {
				Name: "profile",
				Servers: map[string]ProfileServer{
					"bad":  {URL: bad.URL},
					"good": {URL: good.URL, Prefix: "github"},
				},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var rpc mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.Unmarshal(body, &rpc))
	raw, err := json.Marshal(rpc.Result)
	require.NoError(t, err)
	var list mcpprofilerouter.ListToolsResult
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Tools, 1)
	require.Equal(t, "github.search", list.Tools[0].Name)
}

func TestGateway_profileToolsListAllFailuresReturnsEmptyList(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{"bad": {URL: bad.URL}},
		Profiles: map[string]Profile{
			"profile": {
				Name:    "profile",
				Servers: map[string]ProfileServer{"bad": {URL: bad.URL}},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var rpc mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.Unmarshal(body, &rpc))
	raw, err := json.Marshal(rpc.Result)
	require.NoError(t, err)
	var list mcpprofilerouter.ListToolsResult
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Empty(t, list.Tools)
}

func TestGateway_profileToolsListAllToolsDisabledReturnsEmptyList(t *testing.T) {
	l2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Result: mcpprofilerouter.ListToolsResult{Tools: []mcpprofilerouter.Tool{
				{Name: "read", InputSchema: map[string]any{"type": "object"}},
				{Name: "search", InputSchema: map[string]any{"type": "object"}},
			}},
		})
	}))
	defer l2.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{"aws-knowledge": {URL: l2.URL}},
		Profiles: map[string]Profile{
			"profile": {
				Name: "profile",
				Servers: map[string]ProfileServer{
					"aws-knowledge": {URL: l2.URL, Prefix: "aws", EnabledTools: map[string]bool{}},
				},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var rpc mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.Unmarshal(body, &rpc))
	raw, err := json.Marshal(rpc.Result)
	require.NoError(t, err)
	var list mcpprofilerouter.ListToolsResult
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Empty(t, list.Tools)
}

func TestGateway_profileToolsListXAPIKeyAuth(t *testing.T) {
	var reached bool
	l2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Result:  mcpprofilerouter.ListToolsResult{},
		})
	}))
	defer l2.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{"github": {URL: l2.URL}},
		Profiles: map[string]Profile{
			"profile": {
				Name:    "profile",
				APIKey:  "profile-key",
				Servers: map[string]ProfileServer{"github": {URL: l2.URL}},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, map[string]string{"x-api-key": "profile-key"})
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, reached)
}

func TestGateway_profileUnsupportedMethodReachesNoL2(t *testing.T) {
	var reached bool
	l2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer l2.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{"github": {URL: l2.URL}},
		Profiles: map[string]Profile{
			"profile": {
				Name:    "profile",
				Servers: map[string]ProfileServer{"github": {URL: l2.URL}},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsCall,
	}, nil)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.False(t, reached)
}

func TestGateway_unknownProfileReachesNoL2(t *testing.T) {
	var reached bool
	l2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer l2.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{"github": {URL: l2.URL}},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/missing", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.False(t, reached)
}

func TestGateway_profileToolsListBackendJSONRPCErrorIsPartialFailure(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Error:   &mcpprofilerouter.JSONRPCError{Code: -32000, Message: "backend failed"},
		})
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Result: mcpprofilerouter.ListToolsResult{Tools: []mcpprofilerouter.Tool{
				{Name: "search", InputSchema: map[string]any{"type": "object"}},
			}},
		})
	}))
	defer good.Close()
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{
			"bad":  {URL: bad.URL},
			"good": {URL: good.URL},
		},
		Profiles: map[string]Profile{
			"profile": {
				Name: "profile",
				Servers: map[string]ProfileServer{
					"bad":  {URL: bad.URL},
					"good": {URL: good.URL},
				},
			},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/profile", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var rpc mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.Unmarshal(body, &rpc))
	raw, err := json.Marshal(rpc.Result)
	require.NoError(t, err)
	var list mcpprofilerouter.ListToolsResult
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Equal(t, []string{"good.search"}, []string{list.Tools[0].Name})
}

func TestGateway_unknownCatalogServer(t *testing.T) {
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{
			"github": {URL: "http://127.0.0.1:1"},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/s/aws-knowledge", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestValidateConfig(t *testing.T) {
	require.NoError(t, ValidateConfig(Config{
		CatalogServers: map[string]CatalogServer{
			"aws-knowledge": {URL: "http://127.0.0.1:8080"},
			"github":        {URL: "http://127.0.0.1:8081"},
		},
		Profiles: map[string]Profile{
			"profile": {
				Name: "profile",
				Servers: map[string]ProfileServer{
					"aws-knowledge": {URL: "http://127.0.0.1:8080", Prefix: "aws"},
					"github":        {URL: "http://127.0.0.1:8081", Prefix: "github"},
				},
			},
		},
	}))
	require.Error(t, ValidateConfig(Config{}))
	require.Error(t, ValidateConfig(Config{CatalogServers: map[string]CatalogServer{"bad/id": {URL: "http://127.0.0.1:8080"}}}))
	require.Error(t, ValidateConfig(Config{CatalogServers: map[string]CatalogServer{"github": {URL: "127.0.0.1:8080"}}}))
	require.Error(t, ValidateConfig(Config{
		CatalogServers: map[string]CatalogServer{"github": {URL: "http://127.0.0.1:8080"}},
		Profiles: map[string]Profile{
			"profile": {
				Name: "profile",
				Servers: map[string]ProfileServer{
					"a": {URL: "http://127.0.0.1:8080", Prefix: "dup"},
					"b": {URL: "http://127.0.0.1:8081", Prefix: "dup"},
				},
			},
		},
	}))
}

func postRPCRaw(t *testing.T, url, sessionID string, req mcpprofilerouter.JSONRPCRequest, headers map[string]string) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body)) //nolint:noctx
	require.NoError(t, err)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	if sessionID != "" {
		httpReq.Header.Set(mcpprofilerouter.SessionIDHeader, sessionID)
	}
	for name, value := range headers {
		httpReq.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	return resp
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}
