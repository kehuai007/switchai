package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"switchai/appdata"
	"switchai/config"
	"switchai/history"
	"switchai/logger"
	"switchai/proxy"
	"switchai/service"
	"switchai/stats"
	"switchai/web"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func main() {
	// Parse command line flags
	port := flag.String("p", "7777", "Port to listen on")
	install := flag.Bool("install", false, "Install as system service")
	uninstall := flag.Bool("uninstall", false, "Uninstall system service")
	skipAuth := flag.Bool("skip", false, "Skip authentication (for internal network deployment)")
	flag.Parse()

	// Set skip auth mode in config
	if *skipAuth {
		config.SetSkipAuth(true)
	}

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
	// 初始化数据目录（使用进程名作为目录名）
	if err := appdata.Init(); err != nil {
		log.Fatalf("Failed to initialize data directory: %v", err)
	}

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
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	// 启动服务器
	go func() {
		logger.Info("Starting SwitchAI service on %s", addr)
		fmt.Printf("\n🚀 SwitchAI is running on http://localhost:%s\n\n", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Failed to start server: %v", err)
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server...")
	fmt.Println("\n🛑 正在关闭服务器...")

	// 关闭数据库连接
	config.Shutdown()

	// 立即保存统计数据
	stats.Shutdown()

	// 关闭历史记录后台保存
	history.Shutdown()

	// 优雅关闭服务器
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("Server forced to shutdown: %v", err)
		log.Fatal("Server forced to shutdown:", err)
	}

	logger.Info("Server exited")
	fmt.Println("✅ 服务器已安全退出")
}
