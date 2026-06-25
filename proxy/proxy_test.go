package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"switchai/appdata"
	"switchai/config"
	"testing"

	"github.com/gin-gonic/gin"
)

// 注：resolveRouteTarget 当前依赖 config.GetConfig() 单例，难以单元测试。
// 集成测试见 Task 21（端到端验证）。
// 此文件保留以便未来添加不依赖全局单例的辅助测试。

func TestProxyPackage_Compiles(t *testing.T) {
	// 占位测试：保证 proxy 包始终有 _test.go 文件，go test ./proxy/... 不会报 "no test files"
}

// TestBuildModelsListResponse_DedupesAndExposesUserModel 守护 /v1/models 响应：
// 1) 取 UserModel（客户端能直接调用的名字），不是 ProviderModel；
// 2) 同一 user_model 不会重复（DB 唯一约束保证，但仍去重保险）；
// 3) data 按 id 排序，让客户端能依赖稳定顺序。
func TestBuildModelsListResponse_DedupesAndExposesUserModel(t *testing.T) {
	mappings := []config.ModelMapping{
		{UserModel: "fast", ProviderModel: "gpt-4"},
		{UserModel: "fast", ProviderModel: "gpt-4-turbo"},
		{UserModel: "smart", ProviderModel: "claude-sonnet"},
		{UserModel: "opus", ProviderModel: "claude-opus"},
	}

	resp := buildModelsListResponse(mappings)
	wantIDs := []string{"fast", "opus", "smart"}
	gotIDs := make([]string, len(resp.Data))
	for i, e := range resp.Data {
		gotIDs[i] = e.ID
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("data ids = %v, want %v", gotIDs, wantIDs)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
}

// TestBuildModelsListResponse_EmptyInput 返回空 data slice，handler 直接序列化空数组而非 null。
func TestBuildModelsListResponse_EmptyInput(t *testing.T) {
	resp := buildModelsListResponse(nil)
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	if resp.Data == nil {
		t.Errorf("Data is nil; handler would marshal as null instead of []")
	}
	if len(resp.Data) != 0 {
		t.Errorf("got %d data entries, want 0", len(resp.Data))
	}

	// 序列化后必须是 "[]" 而非 "null"
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"object":"list","data":[]}` {
		t.Errorf("got JSON %s, want {\"object\":\"list\",\"data\":[]}", b)
	}
}

// TestListModelsForKey_EndToEnd 端到端验证：路由注册 + 鉴权 + 完整响应。
// 隔离到临时数据目录，避免污染真实 config.db。
func TestListModelsForKey_EndToEnd(t *testing.T) {
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	if err := appdata.Init(); err != nil {
		t.Fatalf("appdata.Init: %v", err)
	}
	if err := config.Init(); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(config.Shutdown)

	cfg := config.GetConfig()
	if err := cfg.AddProvider(config.Provider{
		ID: "p1", Name: "P1", BaseURL: "http://x", APIKey: "k",
		Model: "X;Y", IsActive: true, CreatedAt: "2026-01-01T00:00:00Z", Order: 1,
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	rawKey, err := cfg.GenerateServerKey()
	if err != nil {
		t.Fatalf("GenerateServerKey: %v", err)
	}
	keyID := findKeyIDByKey(cfg, rawKey)

	if _, err := cfg.AddMapping(keyID, config.ModelMapping{
		UserModel: "alias-1", ProviderID: "p1", ProviderModel: "Y", CreatedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	if _, err := cfg.AddMapping(keyID, config.ModelMapping{
		UserModel: "alias-2", ProviderID: "p1", ProviderModel: "X", CreatedAt: "2026-01-02T00:00:00Z",
	}); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}

	r := newTestEngine()
	do := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// 1) 缺 Authorization → 401
	if w := do(""); w.Code != http.StatusUnauthorized {
		t.Errorf("no-auth: got status %d, want 401; body=%s", w.Code, w.Body.String())
	}

	// 2) 错误 key → 401
	if w := do("Bearer sk-wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("bad-key: got status %d, want 401; body=%s", w.Code, w.Body.String())
	}

	// 3) 正确 key → 200 + 正确 JSON 形状
	w := do("Bearer " + rawKey)
	if w.Code != http.StatusOK {
		t.Fatalf("valid-key: got status %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json...", ct)
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, w.Body.String())
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	ids := make([]string, len(resp.Data))
	for i, e := range resp.Data {
		if e.Object != "model" {
			t.Errorf("data[%d].object = %q, want model", i, e.Object)
		}
		if e.OwnedBy != "switchai" {
			t.Errorf("data[%d].owned_by = %q, want switchai", i, e.OwnedBy)
		}
		if e.Created <= 0 {
			t.Errorf("data[%d].created = %d, want positive", i, e.Created)
		}
		ids[i] = e.ID
	}
	// 客户端看到的是它能直接调用的 user_model（不是 provider 的实际 model 名）
	want := []string{"alias-1", "alias-2"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("data ids = %v, want %v", want, ids)
	}
}

// TestListModelsForKey_NoMappings 验证 key 没有映射时返回空 data 数组（而非 null）。
func TestListModelsForKey_NoMappings(t *testing.T) {
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	if err := appdata.Init(); err != nil {
		t.Fatalf("appdata.Init: %v", err)
	}
	if err := config.Init(); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(config.Shutdown)

	rawKey, err := config.GetConfig().GenerateServerKey()
	if err != nil {
		t.Fatalf("GenerateServerKey: %v", err)
	}

	r := newTestEngine()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// 必须是 "data":[] 而非 "data":null
	if !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Errorf("body = %s, want contains \"data\":[] (not null)", w.Body.String())
	}
}

// newTestEngine 复刻 proxy.RegisterRoutes 的路由形态。
// 注意：测试和生产一样，只挂 /v1/* 通配 + 内部特判 /v1/models；不要在这里再
// 加 r.GET("/v1/models", ...) —— gin v1.10 不允许与通配共存，启动会 panic。
func newTestEngine() http.Handler {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/v1/*path", proxyHandler)
	return r
}

// findKeyIDByKey 通过 key 值反查 server key 的 ID（config 包内已有同名 helper，
// 但那个在 _test.go 里，不导出，proxy 测试只能自己写一份）。
func findKeyIDByKey(cfg *config.Config, key string) string {
	for _, k := range cfg.GetServerKeys() {
		if k.Key == key {
			return k.ID
		}
	}
	return ""
}
// TestBuildModelsListResponseJSONShape 守护对外 JSON 字段名兼容 OpenAI /v1/models。
// 任何字段重命名都会让下游 OpenAI 客户端解析失败。
func TestBuildModelsListResponseJSONShape(t *testing.T) {
	mappings := []config.ModelMapping{
		{UserModel: "cc", ProviderModel: "MiniMax-M3"},
	}

	resp := buildModelsListResponse(mappings)
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("response is not valid JSON in expected shape: %v\nbody: %s", err, raw)
	}
	if parsed.Object != "list" {
		t.Errorf("object = %q, want list", parsed.Object)
	}
	if len(parsed.Data) != 1 {
		t.Fatalf("got %d data entries, want 1", len(parsed.Data))
	}
	e := parsed.Data[0]
	if e.ID != "cc" {
		t.Errorf("id = %q, want cc (客户端要拿这个 name 去调网关)", e.ID)
	}
	if e.Object != "model" {
		t.Errorf("data[].object = %q, want model", e.Object)
	}
	if e.OwnedBy != "switchai" {
		t.Errorf("data[].owned_by = %q, want switchai", e.OwnedBy)
	}
	if e.Created <= 0 {
		t.Errorf("data[].created = %d, want positive Unix timestamp", e.Created)
	}
}
