package web

import (
	"log"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type ConfigEvent struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Source string `json:"source"`
}

type ConfigBroadcaster struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
}

var globalBroadcaster = &ConfigBroadcaster{
	clients: make(map[*websocket.Conn]bool),
}

func (cb *ConfigBroadcaster) AddClient(conn *websocket.Conn) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.clients[conn] = true
}

func (cb *ConfigBroadcaster) RemoveClient(conn *websocket.Conn) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	delete(cb.clients, conn)
}

func (cb *ConfigBroadcaster) Broadcast(event ConfigEvent) {
	cb.mu.RLock()
	var dead []*websocket.Conn
	for conn := range cb.clients {
		if err := conn.WriteJSON(event); err != nil {
			log.Printf("ws_config broadcast error: %v", err)
			dead = append(dead, conn)
		}
	}
	cb.mu.RUnlock()

	if len(dead) > 0 {
		cb.mu.Lock()
		for _, conn := range dead {
			delete(cb.clients, conn)
		}
		cb.mu.Unlock()
		for _, conn := range dead {
			_ = conn.Close()
		}
	}
}

// broadcastConfigChange 从 gin.Context 提取 X-Client-Token header 并发送事件。
func broadcastConfigChange(c *gin.Context, eventType, id string) {
	source := c.GetHeader("X-Client-Token")
	globalBroadcaster.Broadcast(ConfigEvent{
		Type:   eventType,
		ID:     id,
		Source: source,
	})
}

var configUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleConfigWebSocket(c *gin.Context) {
	conn, err := configUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("ws_config upgrade error: %v", err)
		return
	}
	globalBroadcaster.AddClient(conn)
	defer globalBroadcaster.RemoveClient(conn)

	for {
		// 客户端不发消息；只靠服务端推送。读取用于检测断连。
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}
