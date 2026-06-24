package proxy

import "testing"

// 注：resolveRouteTarget 当前依赖 config.GetConfig() 单例，难以单元测试。
// 集成测试见 Task 21（端到端验证）。
// 此文件保留以便未来添加不依赖全局单例的辅助测试。

func TestProxyPackage_Compiles(t *testing.T) {
	// 占位测试：保证 proxy 包始终有 _test.go 文件，go test ./proxy/... 不会报 "no test files"
}
