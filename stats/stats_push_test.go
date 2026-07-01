package stats

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestHandleBroadcast_PushesQuotaWhenIdle 守护本次修复的核心：
// 即使没有任何新的 usage 记录（页面空闲），quota 快照也必须被周期性
// 推送给 WebSocket 客户端。历史 bug 是 provider_quotas 只搭 RecordUsage
// 广播的便车，空闲时额度重置采到了却推不出去。
func TestHandleBroadcast_PushesQuotaWhenIdle(t *testing.T) {
	defer setupTestDB(t)()

	// 缩短推送间隔以加速测试；关闭 ping 干扰（设很大）。
	prevPush, prevPing := pushInterval, pingInterval
	pushInterval = 50 * time.Millisecond
	pingInterval = time.Hour
	defer func() { pushInterval, pingInterval = prevPush, prevPing }()

	s := &Stats{
		clients:   make(map[*websocket.Conn]bool),
		broadcast: make(chan UsageRecord, 100),
	}
	prev := stats
	stats = s
	defer func() { stats = prev }()

	loopDone := make(chan struct{})
	go func() { s.handleBroadcast(); close(loopDone) }()
	// 关键清理：关闭 broadcast 让 handleBroadcast 退出，并等待其真正返回，
	// 否则残留的 push ticker 会在 setupTestDB 的 defer 还原全局 db 时并发读
	// db（data race）。此 defer 必须晚于 setupTestDB 的 defer 注册，
	// LIFO 下先执行，确保 goroutine 先停、db 再还原。
	defer func() {
		close(s.broadcast)
		<-loopDone
	}()

	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.AddClient(c)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer client.Close()

	// 关键：不产生任何 usage 记录。断言仍能连续收到两条含
	// provider_quotas 的周期推送。当前实现下客户端收不到任何消息，
	// 第一次 ReadMessage 会因超时失败（红）。
	for i := 0; i < 2; i++ {
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("read message %d (idle push missing): %v", i, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal message %d: %v", i, err)
		}
		if _, ok := m["provider_quotas"]; !ok {
			t.Fatalf("message %d missing provider_quotas field; got keys: %v", i, keysOf(m))
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
