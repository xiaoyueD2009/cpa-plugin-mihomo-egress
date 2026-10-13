package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNormalizeControllerURL(t *testing.T) {
	valid, err := normalizeControllerURL(" http://mihomo:9090/ ")
	if err != nil {
		t.Fatalf("normalizeControllerURL() error = %v", err)
	}
	if got := valid.String(); got != "http://mihomo:9090" {
		t.Fatalf("normalized URL = %q, want http://mihomo:9090", got)
	}
	for _, input := range []string{
		"",
		"ftp://mihomo:9090",
		"http:///proxies",
		"http://user:pass@mihomo:9090",
		"http://mihomo:9090/private-path",
		"http://mihomo:9090/?secret=leak",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := normalizeControllerURL(input); err == nil {
				t.Fatalf("normalizeControllerURL(%q) unexpectedly succeeded", input)
			}
		})
	}
}

func TestParseGroupNames(t *testing.T) {
	got := parseGroupNames(" AI-EXIT, JP-EXIT,AI-EXIT,, ")
	if len(got) != 2 || got[0] != "AI-EXIT" || got[1] != "JP-EXIT" {
		t.Fatalf("parseGroupNames() = %#v", got)
	}
	if got := parseGroupNames(" , "); len(got) != 1 || got[0] != defaultGroup {
		t.Fatalf("empty group list default = %#v", got)
	}
}

func TestStateProjectionDoesNotExposeMihomoCredentials(t *testing.T) {
	preserveRuntime(t)
	secret := "controller-secret-never-return-this"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/proxies" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s, want GET /proxies", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("Authorization header = %q", got)
		}
		return mockHTTPResponse(r, http.StatusOK, `{"proxies":{"AI-EXIT":{"name":"AI-EXIT","type":"Fallback","now":"US-West","fixed":"US-West","all":["US-West"]},"US-West":{"name":"US-West","type":"VLESS","server":"edge.example.net","port":443,"uuid":"uuid-secret","password":"password-secret","alive":true,"history":[{"delay":73}]}}}`), nil
	})}
	setState(runtimeState{
		config:     pluginConfig{ControllerSecret: secret},
		controller: mustURL(t, "http://mihomo:9090"),
		httpClient: client,
		groups:     []string{"AI-EXIT"},
		configured: true,
	})

	raw, err := handleState()
	if err != nil {
		t.Fatalf("handleState() error = %v", err)
	}
	response := decodePluginResponse(t, raw)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
	}
	var result statePayload
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatalf("decode state payload: %v", err)
	}
	if len(result.Groups) != 1 || result.Groups[0].Type != "Fallback" || result.Groups[0].Current != "US-West" {
		t.Fatalf("projected groups = %#v", result.Groups)
	}
	if got := result.Groups[0].Candidates[0].Address; got != "edge.example.net:443" {
		t.Fatalf("node address = %q", got)
	}
	if got := result.Groups[0].Candidates[0].Delay; got != 73 {
		t.Fatalf("node delay = %d", got)
	}
	if strings.Contains(string(response.Body), "uuid-secret") || strings.Contains(string(response.Body), "password-secret") || strings.Contains(string(response.Body), secret) {
		t.Fatalf("state response leaked Mihomo credentials: %s", response.Body)
	}
}

func TestSwitchRouteValidatesGroupMembership(t *testing.T) {
	preserveRuntime(t)
	const controllerSecret = "mihomo-secret"
	var puts int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == "/proxies" {
			return mockHTTPResponse(r, http.StatusOK, `{"proxies":{"AI-EXIT":{"type":"Fallback","now":"US-West","all":["US-West","US-East"]},"US-West":{"type":"VLESS","server":"west.example","port":443},"US-East":{"type":"VLESS","server":"east.example","port":443}}}`), nil
		}
		if r.Method == http.MethodPut && r.URL.Path == "/proxies/AI-EXIT" {
			puts++
			if got := r.Header.Get("Authorization"); got != "Bearer "+controllerSecret {
				t.Errorf("PUT Authorization = %q", got)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode PUT body: %v", err)
			}
			if body["name"] != "US-East" {
				t.Errorf("selected name = %q", body["name"])
			}
			return mockHTTPResponse(r, http.StatusNoContent, ""), nil
		}
		t.Errorf("unexpected Mihomo request: %s %s", r.Method, r.URL.Path)
		return mockHTTPResponse(r, http.StatusNotFound, ""), nil
	})}
	setState(runtimeState{
		config:     pluginConfig{ControllerSecret: controllerSecret},
		controller: mustURL(t, "http://mihomo:9090"),
		httpClient: client,
		groups:     []string{"AI-EXIT"},
		configured: true,
	})

	badMembership, err := callManagement(t, http.MethodPost, switchPath, http.Header{"Sec-Fetch-Site": []string{"same-origin"}}, `{"group":"AI-EXIT","node":"NOT-A-MEMBER"}`)
	if err != nil {
		t.Fatal(err)
	}
	if badMembership.StatusCode != http.StatusBadRequest || puts != 0 {
		t.Fatalf("invalid member switch status=%d PUTs=%d", badMembership.StatusCode, puts)
	}

	response, err := callManagement(t, http.MethodPost, switchPath, http.Header{"Sec-Fetch-Site": []string{"same-origin"}}, `{"group":"AI-EXIT","node":"US-East"}`)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || puts != 1 {
		t.Fatalf("valid switch status=%d PUTs=%d body=%s", response.StatusCode, puts, response.Body)
	}
	if !strings.Contains(string(response.Body), `"selected":"US-East"`) {
		t.Fatalf("switch response missing selected node: %s", response.Body)
	}
	if events := recentAudit(); len(events) == 0 || events[0].Selected != "US-East" || events[0].ChangedAt == "" {
		t.Fatalf("audit events = %#v", events)
	}
}

func TestStateRouteReturnsFilteredState(t *testing.T) {
	preserveRuntime(t)
	gets := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gets++
		return mockHTTPResponse(r, http.StatusOK, `{"proxies":{"AI-EXIT":{"type":"Fallback","now":"US-West","all":["US-West"]},"US-West":{"type":"VLESS","server":"west.example","port":443}}}`), nil
	})}
	setState(runtimeState{
		controller: mustURL(t, "http://mihomo:9090"),
		httpClient: client,
		groups:     []string{"AI-EXIT"},
		configured: true,
	})

	response, err := callManagement(t, http.MethodGet, statePath, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || gets != 1 {
		t.Fatalf("authorized state status=%d Mihomo reads=%d", response.StatusCode, gets)
	}
}

func TestSwitchRejectsCrossSiteBrowserRequests(t *testing.T) {
	headers := http.Header{"Sec-Fetch-Site": []string{"cross-site"}}
	if sameOriginRequest(headers) {
		t.Fatal("cross-site request unexpectedly accepted")
	}
}

func TestResourceRouteOnlyServesThePage(t *testing.T) {
	preserveRuntime(t)
	setState(runtimeState{configured: true})
	page, err := callManagement(t, http.MethodGet, resourcePath, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.StatusCode != http.StatusOK || !strings.Contains(page.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("resource response status/content type = %d / %q", page.StatusCode, page.Headers.Get("Content-Type"))
	}
	post, err := callManagement(t, http.MethodPost, resourcePath, nil, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("resource POST status = %d, want 405", post.StatusCode)
	}
}

func TestPluginRegistrationUsesCPA804SchemaAndIndependentRoutes(t *testing.T) {
	preserveRuntime(t)
	raw, err := handleMethod("plugin.register", lifecyclePayload("controller_url: http://mihomo:9090\ngroup_names: AI-EXIT\n"))
	if err != nil {
		t.Fatalf("plugin.register: %v", err)
	}
	var result envelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	var reg registration
	if err := json.Unmarshal(result.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != 6 || !reg.Capabilities.ManagementAPI {
		t.Fatalf("registration schema/capabilities = %d / %#v", reg.SchemaVersion, reg.Capabilities)
	}
	if reg.Metadata.Name != pluginName || len(reg.Metadata.ConfigFields) != 3 {
		t.Fatalf("registration metadata = %#v", reg.Metadata)
	}
	if !currentState().configured {
		t.Fatal("valid independent plugin configuration was not applied")
	}
	registerRaw, err := handleMethod("management.register", []byte(`{"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/mihomo-egress"}`))
	if err != nil {
		t.Fatal(err)
	}
	var resourcesEnvelope envelope
	if err := json.Unmarshal(registerRaw, &resourcesEnvelope); err != nil {
		t.Fatal(err)
	}
	var registration managementRegistration
	if err := json.Unmarshal(resourcesEnvelope.Result, &registration); err != nil {
		t.Fatal(err)
	}
	if len(registration.Resources) != 1 || registration.Resources[0].Path != resourceOpen {
		t.Fatalf("management resources = %#v", registration.Resources)
	}
	if len(registration.Routes) != 2 || registration.Routes[0].Method != http.MethodGet || registration.Routes[0].Path != statePath || registration.Routes[1].Method != http.MethodPost || registration.Routes[1].Path != switchPath {
		t.Fatalf("management routes = %#v", registration.Routes)
	}
}

func TestPageUsesCPAKeyButNeverContainsMihomoConfiguration(t *testing.T) {
	preserveRuntime(t)
	setState(runtimeState{configError: "", configured: true})
	page := string(pageTemplate)
	if strings.Contains(page, "controller_secret") || strings.Contains(page, "mihomo:9090") || strings.Contains(page, "ui_access_token") || !strings.Contains(page, "CPA 管理密钥") {
		t.Fatalf("page contains server-side configuration material")
	}
}

func lifecyclePayload(config string) []byte {
	raw, err := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{ConfigYAML: []byte(config)})
	if err != nil {
		panic(err)
	}
	return raw
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func mockHTTPResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func callManagement(t *testing.T, method, route string, headers http.Header, body string) (managementResponse, error) {
	t.Helper()
	request, err := json.Marshal(managementRequest{
		Method:  method,
		Path:    route,
		Headers: headers,
		Body:    []byte(body),
	})
	if err != nil {
		return managementResponse{}, err
	}
	raw, err := handleMethod("management.handle", request)
	if err != nil {
		return managementResponse{}, err
	}
	return decodePluginResponse(t, raw), nil
}

func decodePluginResponse(t *testing.T, raw []byte) managementResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode plugin envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("plugin envelope error: %s", raw)
	}
	var response managementResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	return response
}

func TestRecentAuditIsBounded(t *testing.T) {
	original := recentAudit()
	t.Cleanup(func() {
		auditMu.Lock()
		audit = original
		auditMu.Unlock()
	})
	auditMu.Lock()
	audit = nil
	auditMu.Unlock()
	for i := 0; i < 25; i++ {
		appendAudit(auditEvent{Selected: "node", ChangedAt: time.Now().UTC().Format(time.RFC3339)})
	}
	if events := recentAudit(); len(events) != 20 {
		t.Fatalf("recent audit entries = %d, want 20", len(events))
	}
}

func preserveRuntime(t *testing.T) {
	t.Helper()
	originalState := currentState()
	originalAudit := recentAudit()
	t.Cleanup(func() {
		setState(originalState)
		auditMu.Lock()
		audit = originalAudit
		auditMu.Unlock()
	})
}
