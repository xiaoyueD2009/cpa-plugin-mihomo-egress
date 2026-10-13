package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	pluginName     = "CPA Mihomo Router"
	pluginAuthor   = "xiaoyueD2009"
	pluginRepo     = "https://github.com/xiaoyueD2009/cpa-plugin-mihomo-egress"
	pluginID       = "mihomo-egress"
	resourceOpen   = "/open"
	managementBase = "/v0/management/plugins/" + pluginID
	statePath      = managementBase + "/state"
	switchPath     = managementBase + "/switch"
	resourcePath   = "/v0/resource/plugins/" + pluginID + resourceOpen
	defaultGroup   = "AI-EXIT"
	maxAPIResponse = 4 << 20
	abiVersion     = 1
	schemaVersion  = 6
)

const (
	methodPluginRegister     = "plugin.register"
	methodPluginReconfigure  = "plugin.reconfigure"
	methodManagementRegister = "management.register"
	methodManagementHandle   = "management.handle"
)

var pluginVersion = "0.1.1"

//go:embed web/index.html
var pageTemplate string

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *pluginError    `json:"error,omitempty"`
}

type pluginError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginMetadata           `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

// These fields mirror CPA v8.0.4's pluginapi.Metadata and ConfigField JSON shape.
type pluginMetadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Description string `json:"Description"`
}

type registrationCapabilities struct {
	ManagementAPI bool `json:"management_api"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes,omitempty"`
	Resources []managementResource `json:"resources,omitempty"`
}

type managementRegistrationRequest struct {
	BasePath         string
	ResourceBasePath string
}

type managementRoute struct {
	Method      string
	Path        string
	Menu        string `json:",omitempty"`
	Description string `json:",omitempty"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type managementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

type pluginConfig struct {
	ControllerURL    string `yaml:"controller_url"`
	ControllerSecret string `yaml:"controller_secret"`
	GroupNames       string `yaml:"group_names"`
}

type runtimeState struct {
	config      pluginConfig
	controller  *url.URL
	httpClient  *http.Client
	groups      []string
	configError string
	configured  bool
}

type auditEvent struct {
	Group     string `json:"group"`
	Previous  string `json:"previous"`
	Selected  string `json:"selected"`
	ChangedAt string `json:"changed_at"`
}

var (
	stateMu sync.RWMutex
	state   = runtimeState{configError: "请在 CPA 插件管理页中配置 Mihomo Controller 地址后使用。"}
	auditMu sync.RWMutex
	audit   []auditEvent
)

type proxyCollection struct {
	Proxies map[string]proxyInfo `json:"proxies"`
}

type proxyInfo struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Now     string         `json:"now"`
	Fixed   string         `json:"fixed"`
	All     []string       `json:"all"`
	Server  string         `json:"server"`
	Port    int            `json:"port"`
	Alive   *bool          `json:"alive"`
	History []proxyHistory `json:"history"`
}

type proxyHistory struct {
	Delay int `json:"delay"`
}

type nodeView struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Address string `json:"address,omitempty"`
	Delay   int    `json:"delay,omitempty"`
	Alive   *bool  `json:"alive,omitempty"`
	IsGroup bool   `json:"is_group"`
}

type groupView struct {
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Current    string     `json:"current"`
	Fixed      string     `json:"fixed,omitempty"`
	Switchable bool       `json:"switchable"`
	Candidates []nodeView `json:"candidates"`
}

type statePayload struct {
	Groups []groupView  `json:"groups"`
	Audit  []auditEvent `json:"audit"`
	At     string       `json:"at"`
}

type switchRequest struct {
	Group string `json:"group"`
	Node  string `json:"node"`
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		applyConfig(raw)
		return okEnvelope(pluginRegistration())
	case methodManagementRegister:
		return okEnvelope(registerManagement(raw))
	case methodManagementHandle:
		return handleManagement(raw)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			ConfigFields: []configField{
				{Name: "controller_url", Type: "string", Description: "Mihomo Controller 内网地址；CPA 与 Mihomo 同一 Docker 网络时通常为 http://mihomo:9090。"},
				{Name: "controller_secret", Type: "string", Description: "Mihomo 配置中的 secret；未设置时留空。仅由 CPA 服务端使用，不会返回给浏览器。"},
				{Name: "group_names", Type: "string", Description: "允许查看和手动切换的组名，英文逗号分隔；默认 AI-EXIT。"},
			},
		},
		Capabilities: registrationCapabilities{ManagementAPI: true},
	}
}

func registerManagement(raw []byte) managementRegistration {
	var req managementRegistrationRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	basePath := strings.TrimRight(strings.TrimSpace(req.BasePath), "/")
	if basePath == "" {
		basePath = "/v0/management"
	}
	return managementRegistration{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: basePath + "/plugins/" + pluginID + "/state"},
			{Method: http.MethodPost, Path: basePath + "/plugins/" + pluginID + "/switch"},
		},
		Resources: []managementResource{
			{Path: resourceOpen, Menu: "出口节点", Description: "只读查看 Mihomo 代理组与节点，并手动切换已配置节点。"},
		},
	}
}

func applyConfig(raw []byte) {
	var req lifecycleRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		setState(runtimeState{configError: "插件配置请求无法解析。"})
		return
	}
	var cfg pluginConfig
	if len(bytes.TrimSpace(req.ConfigYAML)) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			setState(runtimeState{configError: "Mihomo 出口插件 YAML 配置无效。"})
			return
		}
	}
	if strings.TrimSpace(cfg.ControllerURL) == "" {
		cfg.ControllerURL = "http://mihomo:9090"
	}
	controller, err := normalizeControllerURL(cfg.ControllerURL)
	if err != nil {
		setState(runtimeState{config: cfg, configError: err.Error()})
		return
	}
	next := runtimeState{
		config:     cfg,
		controller: controller,
		groups:     parseGroupNames(cfg.GroupNames),
		configured: true,
	}
	setState(next)
}

func setState(next runtimeState) {
	stateMu.Lock()
	state = next
	stateMu.Unlock()
}

func currentState() runtimeState {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return state
}

func parseGroupNames(raw string) []string {
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	groups := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		groups = append(groups, name)
	}
	if len(groups) == 0 {
		return []string{defaultGroup}
	}
	return groups
}

func normalizeControllerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("controller_url 必须是有效的 http:// 或 https:// URL。")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("controller_url 不得包含用户名、密码、查询参数或片段。")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("controller_url 仅支持 Controller 根地址，不支持路径前缀。")
	}
	u.Path = ""
	u.RawPath = ""
	return u, nil
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
	}
	switch path.Clean(req.Path) {
	case resourcePath:
		if req.Method != http.MethodGet {
			return okEnvelope(jsonResponse(http.StatusMethodNotAllowed, apiError{Error: "页面仅支持 GET。"}))
		}
		page := string(pageTemplate)
		if cfg := currentState(); !cfg.configured {
			page = configErrorPage(cfg.configError)
		}
		return okEnvelope(htmlResponse(http.StatusOK, page))
	case statePath:
		if req.Method != http.MethodGet {
			return okEnvelope(jsonResponse(http.StatusMethodNotAllowed, apiError{Error: "仅支持 GET。"}))
		}
		return handleState()
	case switchPath:
		if req.Method != http.MethodPost {
			return okEnvelope(jsonResponse(http.StatusMethodNotAllowed, apiError{Error: "仅支持 POST。"}))
		}
		cfg := currentState()
		if !sameOriginRequest(req.Headers) {
			return okEnvelope(jsonResponse(http.StatusForbidden, apiError{Error: "拒绝跨站切换请求。"}))
		}
		return handleSwitch(req.Body, cfg)
	default:
		return okEnvelope(jsonResponse(http.StatusNotFound, apiError{Error: "未知的 Mihomo 出口资源。"}))
	}
}

type apiError struct {
	Error string `json:"error"`
}

func sameOriginRequest(headers http.Header) bool {
	site := strings.ToLower(strings.TrimSpace(headerValue(headers, "Sec-Fetch-Site")))
	return site == "" || site == "same-origin"
}

func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func handleState() ([]byte, error) {
	cfg := currentState()
	if !cfg.configured {
		return okEnvelope(jsonResponse(http.StatusServiceUnavailable, apiError{Error: cfg.configError}))
	}
	proxies, err := fetchProxies(context.Background(), cfg)
	if err != nil {
		return okEnvelope(jsonResponse(http.StatusBadGateway, apiError{Error: err.Error()}))
	}
	groups := make([]groupView, 0, len(cfg.groups))
	for _, name := range cfg.groups {
		group, ok := proxies[name]
		if !ok {
			continue
		}
		members := make([]nodeView, 0, len(group.All))
		for _, memberName := range group.All {
			proxy, exists := proxies[memberName]
			if !exists {
				members = append(members, nodeView{Name: memberName, Type: "未知", IsGroup: false})
				continue
			}
			members = append(members, projectNode(memberName, proxy))
		}
		groups = append(groups, groupView{
			Name:       name,
			Type:       group.Type,
			Current:    group.Now,
			Fixed:      group.Fixed,
			Switchable: isSwitchableGroup(group.Type),
			Candidates: members,
		})
	}
	if len(groups) == 0 {
		return okEnvelope(jsonResponse(http.StatusNotFound, apiError{Error: "没有找到配置的代理组；请检查 group_names 与 Mihomo 组名是否一致。"}))
	}
	return okEnvelope(jsonResponse(http.StatusOK, statePayload{Groups: groups, Audit: recentAudit(), At: time.Now().UTC().Format(time.RFC3339)}))
}

func projectNode(name string, proxy proxyInfo) nodeView {
	address := ""
	if strings.TrimSpace(proxy.Server) != "" {
		address = strings.TrimSpace(proxy.Server)
		if proxy.Port > 0 && proxy.Port <= 65535 {
			address = net.JoinHostPort(address, fmt.Sprint(proxy.Port))
		}
	}
	delay := 0
	if len(proxy.History) > 0 {
		delay = proxy.History[len(proxy.History)-1].Delay
	}
	return nodeView{
		Name:    name,
		Type:    proxy.Type,
		Address: address,
		Delay:   delay,
		Alive:   proxy.Alive,
		IsGroup: isGroupType(proxy.Type),
	}
}

func isGroupType(proxyType string) bool {
	switch strings.ToLower(proxyType) {
	case "selector", "urltest", "fallback", "loadbalance":
		return true
	default:
		return false
	}
}

func isSwitchableGroup(proxyType string) bool {
	switch strings.ToLower(proxyType) {
	case "selector", "urltest", "fallback":
		return true
	default:
		return false
	}
}

func handleSwitch(raw []byte, cfg runtimeState) ([]byte, error) {
	if len(raw) == 0 || len(raw) > 4096 {
		return okEnvelope(jsonResponse(http.StatusBadRequest, apiError{Error: "切换请求内容无效。"}))
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input switchRequest
	if err := decoder.Decode(&input); err != nil {
		return okEnvelope(jsonResponse(http.StatusBadRequest, apiError{Error: "切换请求必须包含 group 和 node。"}))
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return okEnvelope(jsonResponse(http.StatusBadRequest, apiError{Error: "切换请求包含多余内容。"}))
	}
	input.Group = strings.TrimSpace(input.Group)
	input.Node = strings.TrimSpace(input.Node)
	if input.Group == "" || input.Node == "" || len(input.Group) > 512 || len(input.Node) > 512 || !contains(cfg.groups, input.Group) {
		return okEnvelope(jsonResponse(http.StatusBadRequest, apiError{Error: "代理组或节点无效，或该组未加入允许列表。"}))
	}
	proxies, err := fetchProxies(context.Background(), cfg)
	if err != nil {
		return okEnvelope(jsonResponse(http.StatusBadGateway, apiError{Error: err.Error()}))
	}
	group, ok := proxies[input.Group]
	if !ok || !isSwitchableGroup(group.Type) {
		return okEnvelope(jsonResponse(http.StatusConflict, apiError{Error: "该代理组不存在或不支持手动切换。"}))
	}
	if !contains(group.All, input.Node) {
		return okEnvelope(jsonResponse(http.StatusBadRequest, apiError{Error: "所选节点不属于该代理组。"}))
	}
	if _, ok := proxies[input.Node]; !ok {
		return okEnvelope(jsonResponse(http.StatusBadRequest, apiError{Error: "Mihomo 当前未返回所选节点。"}))
	}
	if err := selectProxy(context.Background(), cfg, input.Group, input.Node); err != nil {
		return okEnvelope(jsonResponse(http.StatusBadGateway, apiError{Error: err.Error()}))
	}
	event := auditEvent{
		Group:     input.Group,
		Previous:  group.Now,
		Selected:  input.Node,
		ChangedAt: time.Now().UTC().Format(time.RFC3339),
	}
	appendAudit(event)
	return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"ok": true, "event": event}))
}

func fetchProxies(ctx context.Context, cfg runtimeState) (map[string]proxyInfo, error) {
	body, status, err := requestMihomo(ctx, cfg, http.MethodGet, "proxies", "", nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("Mihomo Controller 返回 HTTP %d，请检查地址与密钥。", status)
	}
	var collection proxyCollection
	if err := json.Unmarshal(body, &collection); err != nil {
		return nil, errors.New("Mihomo Controller 返回了无法解析的代理数据。")
	}
	if collection.Proxies == nil {
		return nil, errors.New("Mihomo Controller 响应缺少 proxies 列表。")
	}
	return collection.Proxies, nil
}

func selectProxy(ctx context.Context, cfg runtimeState, group, node string) error {
	payload, err := json.Marshal(map[string]string{"name": node})
	if err != nil {
		return errors.New("无法生成节点切换请求。")
	}
	_, status, err := requestMihomo(ctx, cfg, http.MethodPut, "proxies", group, payload)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Mihomo 拒绝切换，Controller 返回 HTTP %d。", status)
	}
	return nil
}

func requestMihomo(ctx context.Context, cfg runtimeState, method, collection, name string, body []byte) ([]byte, int, error) {
	endpoint := *cfg.controller
	endpoint.Path = "/" + collection
	if name != "" {
		endpoint.Path += "/" + name
		endpoint.RawPath = "/" + collection + "/" + url.PathEscape(name)
	} else {
		endpoint.RawPath = ""
	}
	var requestBody io.Reader
	if len(body) > 0 {
		requestBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), requestBody)
	if err != nil {
		return nil, 0, errors.New("无法构造 Mihomo Controller 请求。")
	}
	if cfg.config.ControllerSecret != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.config.ControllerSecret)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	client := cfg.httpClient
	if client == nil {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if ok {
			transport = transport.Clone()
		} else {
			transport = &http.Transport{}
		}
		transport.Proxy = nil
		client = &http.Client{
			Transport: transport,
			Timeout:   6 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		defer transport.CloseIdleConnections()
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, 0, errors.New("无法连接 Mihomo Controller；请检查 CPA 容器网络、Controller 地址和端口。")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxAPIResponse+1))
	if err != nil {
		return nil, response.StatusCode, errors.New("读取 Mihomo Controller 响应失败。")
	}
	if len(responseBody) > maxAPIResponse {
		return nil, response.StatusCode, errors.New("Mihomo Controller 响应超过大小限制。")
	}
	return responseBody, response.StatusCode, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func appendAudit(event auditEvent) {
	auditMu.Lock()
	defer auditMu.Unlock()
	audit = append([]auditEvent{event}, audit...)
	if len(audit) > 20 {
		audit = audit[:20]
	}
}

func recentAudit() []auditEvent {
	auditMu.RLock()
	defer auditMu.RUnlock()
	return append([]auditEvent(nil), audit...)
}

func htmlResponse(status int, body string) managementResponse {
	return managementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":            []string{"text/html; charset=utf-8"},
			"Cache-Control":           []string{"no-store"},
			"X-Content-Type-Options":  []string{"nosniff"},
			"Referrer-Policy":         []string{"no-referrer"},
			"Content-Security-Policy": []string{"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
		},
		Body: []byte(body),
	}
}

func jsonResponse(status int, value any) managementResponse {
	body, _ := json.Marshal(value)
	return managementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           []string{"application/json; charset=utf-8"},
			"Cache-Control":          []string{"no-store"},
			"X-Content-Type-Options": []string{"nosniff"},
			"Referrer-Policy":        []string{"no-referrer"},
		},
		Body: body,
	}
}

func okEnvelope(value any) ([]byte, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &pluginError{Code: code, Message: message}})
	return raw
}

func configErrorPage(message string) string {
	const fallback = "Mihomo 出口插件尚未配置完成。"
	if strings.TrimSpace(message) == "" {
		message = fallback
	}
	page, err := template.New("config").Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>CPA Mihomo Router</title><style>body{margin:0;background:#f3f4ef;color:#172521;font:16px system-ui,sans-serif;display:grid;min-height:100vh;place-items:center}.panel{max-width:580px;padding:36px;border:1px solid #d8ded7;border-radius:18px;background:#fff;box-shadow:0 18px 60px #16271b14}h1{margin:0 0 12px;font-size:24px}p{line-height:1.65;color:#53605a}code{font-family:ui-monospace,monospace;background:#f0f3ee;padding:2px 5px;border-radius:5px}</style><main class="panel"><h1>CPA Mihomo Router</h1><p>{{.}}</p><p>配置项位于 <code>plugins.configs.mihomo-egress</code>。Mihomo Controller 密钥只保存在 CPA 服务端；页面使用 CPA 自身的管理接口认证。</p></main></html>`)
	if err != nil {
		return "Mihomo 出口插件配置错误。"
	}
	var out bytes.Buffer
	if err := page.Execute(&out, message); err != nil {
		return fallback
	}
	return out.String()
}
