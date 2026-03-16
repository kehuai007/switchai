package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"switchai/config"
	"switchai/history"
	"switchai/logger"
	"switchai/proxy"
	"switchai/service"
	"switchai/stats"
	"switchai/web"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func main() {
	// Parse command line flags
	port := flag.String("p", "7777", "Port to listen on")
	install := flag.Bool("install", false, "Install as system service")
	uninstall := flag.Bool("uninstall", false, "Uninstall system service")
	flag.Parse()

	// Handle service installation/uninstallation
	if *install {
		if err := service.Install(*port); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to install service: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *uninstall {
		if err := service.Uninstall(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to uninstall service: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Normal startup
	startServer(*port)
}

func startServer(port string) {
	// 初始化日志系统
	if err := logger.Init(); err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}

	// 初始化配置
	if err := config.Init(); err != nil {
		logger.Error("Failed to initialize config: %v", err)
		log.Fatalf("Failed to initialize config: %v", err)
	}

	// 初始化统计
	stats.Init()

	// 初始化历史记录
	if err := history.Init(); err != nil {
		logger.Error("Failed to initialize history: %v", err)
	}

	// 创建 Gin 路由（不使用 Default，手动添加中间件）
	r := gin.New()

	// 添加恢复中间件
	r.Use(gin.Recovery())

	// 添加请求日志中间件
	r.Use(logger.RequestLogger())

	// 配置 CORS
	r.Use(cors.Default())

	// 管理界面路由
	web.RegisterRoutes(r)

	// Claude API 代理路由
	proxy.RegisterRoutes(r)

	// 启动服务
	addr := ":" + port
	logger.Info("Starting SwitchAI service on %s", addr)
	fmt.Printf("\n🚀 SwitchAI is running on http://localhost:%s\n\n", port)
	if err := r.Run(addr); err != nil {
		logger.Error("Failed to start server: %v", err)
		log.Fatalf("Failed to start server: %v", err)
	}
}
