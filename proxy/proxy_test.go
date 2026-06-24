package proxy

import (
	"errors"
	"testing"
)

// 注意：resolveRouteTarget 在 Task 9 中实现，本任务仅定义接口与失败用例。
func TestResolveRouteTarget_NoMapping(t *testing.T) {
	t.Skip("resolveRouteTarget not implemented yet")
}

func TestResolveRouteTarget_InactiveProvider(t *testing.T) {
	t.Skip("resolveRouteTarget not implemented yet")
}

func TestResolveRouteTarget_Success(t *testing.T) {
	t.Skip("resolveRouteTarget not implemented yet")
}

// routeResult 是路由决策返回的中间结构
type routeResult struct {
	ProviderID     string
	ProviderModel  string
	BaseURL        string
	APIKey         string
	IsOpenAIFormat bool
}

var errRouteNoMapping = errors.New("model not allowed for this key")
var errRouteInactive = errors.New("model not supported (provider inactive)")
var errRouteMissing = errors.New("configured provider missing")
