package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"switchai/appdata"
	"switchai/config"
	"switchai/history"
	"switchai/quota"
	"testing"
	"time"

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

// setupQuotaTestEnv 在临时目录初始化 appdata + config，供 quota-block 测试复用。
// 也初始化 history（proxy handler 会写历史），避免上游被命中后 AddRecord 报空指针。
func setupQuotaTestEnv(t *testing.T) (cfg *config.Config, rawKey string) {
	t.Helper()
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	if err := appdata.Init(); err != nil {
		t.Fatalf("appdata.Init: %v", err)
	}
	if err := config.Init(); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := history.Init(); err != nil {
		t.Fatalf("history.Init: %v", err)
	}

	cfg = config.GetConfig()
	key, err := cfg.GenerateServerKey()
	if err != nil {
		t.Fatalf("GenerateServerKey: %v", err)
	}

	t.Cleanup(func() {
		history.Shutdown()
		config.Shutdown()
		_ = os.Chdir(origCwd)
	})
	return cfg, key
}

// buildQuotaTestRequest 给定 key/user_model 构造一个会走通 resolveRouteTarget 的
// /v1/chat/completions 请求。OpenAI 格式；provider 的 IsOpenAIFormat 由调用方控制。
func buildQuotaTestRequest(t *testing.T, rawKey, userModel string) *http.Request {
	t.Helper()
	body := `{"model":"` + userModel + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestProxy_QuotaBlocked_ToggleOn 验证：当 provider 的配额达到上限且 toggle ON，
// 代理应在 resolveRouteTarget 之后、URL 构建之前返回 403，并带上 window / used_percent。
func TestProxy_QuotaBlocked_ToggleOn(t *testing.T) {
	cfg, rawKey := setupQuotaTestEnv(t)

	// 上游探测服务器——若 quota gate 没生效，请求就会到这里并返回 200。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	if err := cfg.AddProvider(config.Provider{
		ID: "blocked-provider", Name: "Blocked", BaseURL: upstream.URL, APIKey: "k",
		Model: "m", IsActive: true, CreatedAt: "2026-01-01T00:00:00Z", Order: 1,
		IsOpenAIFormat: true,
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	keyID := findKeyIDByKey(cfg, rawKey)
	if _, err := cfg.AddMapping(keyID, config.ModelMapping{
		UserModel: "alias-blocked", ProviderID: "blocked-provider", ProviderModel: "m",
		CreatedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}

	quota.SetBlockEnabled("blocked-provider", true)
	quota.SetSnapshotForTest("blocked-provider", &quota.Snapshot{
		ProviderID: "blocked-provider",
		Interval: quota.IntervalWindow{
			Enabled:      true,
			UsedPercent:  99.5,
			EndTime:      time.Now().Add(time.Hour),
			ResetInHuman: "1h 0m",
			ResetInSec:   3600,
		},
		Weekly: quota.IntervalWindow{
			Enabled:     true,
			UsedPercent: 50,
		},
	})
	defer quota.PurgeProvider("blocked-provider")

	r := newTestEngine()
	req := buildQuotaTestRequest(t, rawKey, "alias-blocked")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when quota tripped, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json...", got)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, w.Body.String())
	}
	if body["window"] != "interval" {
		t.Errorf("body.window = %v, want interval", body["window"])
	}
	if body["used_percent"] == nil {
		t.Errorf("response missing used_percent; body=%s", w.Body.String())
	} else if v, ok := body["used_percent"].(float64); !ok || v < 99.0 {
		t.Errorf("used_percent = %v (type %T), want >= 99 float", body["used_percent"], body["used_percent"])
	}
	if body["error"] == nil {
		t.Errorf("response missing error message; body=%s", w.Body.String())
	}
}

// TestProxy_QuotaBlocked_ToggleOff 验证：即使配额达到上限，只要 toggle OFF，
// 代理仍应照常转发到上游（绝不应因 quota gate 触发 403）。
func TestProxy_QuotaBlocked_ToggleOff(t *testing.T) {
	cfg, rawKey := setupQuotaTestEnv(t)

	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	if err := cfg.AddProvider(config.Provider{
		ID: "free-provider", Name: "Free", BaseURL: upstream.URL, APIKey: "k",
		Model: "m", IsActive: true, CreatedAt: "2026-01-01T00:00:00Z", Order: 1,
		IsOpenAIFormat: true,
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	keyID := findKeyIDByKey(cfg, rawKey)
	if _, err := cfg.AddMapping(keyID, config.ModelMapping{
		UserModel: "alias-free", ProviderID: "free-provider", ProviderModel: "m",
		CreatedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}

	// Toggle OFF + 同样爆表的配额——关键差异：不应触发 403。
	quota.SetBlockEnabled("free-provider", false)
	quota.SetSnapshotForTest("free-provider", &quota.Snapshot{
		ProviderID: "free-provider",
		Interval:   quota.IntervalWindow{Enabled: true, UsedPercent: 99.5},
	})
	defer quota.PurgeProvider("free-provider")

	r := newTestEngine()
	req := buildQuotaTestRequest(t, rawKey, "alias-free")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("toggle off should not block, got 403; body=%s", w.Body.String())
	}
	if hits == 0 {
		t.Errorf("toggle off: request should have reached upstream, hits=0")
	}
}
