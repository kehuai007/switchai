package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	InfoLogger  *log.Logger
	ErrorLogger *log.Logger
	currentDate string
	logMutex    sync.Mutex
	infoFile    *os.File
	errorFile   *os.File
)

// Init 初始化日志系统
func Init() error {
	// 创建 logs 目录
	logsDir := "logs"
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return err
	}

	// 初始化日志文件
	if err := rotateLogFiles(); err != nil {
		return err
	}

	// 启动日志轮转检查协程
	go checkLogRotation()

	log.Println("Logger initialized successfully")
	return nil
}

// rotateLogFiles 轮转日志文件
func rotateLogFiles() error {
	logMutex.Lock()
	defer logMutex.Unlock()

	// 获取当前日期时间
	now := time.Now()
	dateStr := now.Format("2006-01-02")
	timeStr := now.Format("2006-01-02_15")

	// 如果日期没变，不需要轮转
	if currentDate == dateStr && infoFile != nil && errorFile != nil {
		return nil
	}

	// 关闭旧文件
	if infoFile != nil {
		infoFile.Close()
	}
	if errorFile != nil {
		errorFile.Close()
	}

	// 创建新的日志文件
	logsDir := "logs"
	infoFilename := filepath.Join(logsDir, fmt.Sprintf("info_%s.log", timeStr))
	errorFilename := filepath.Join(logsDir, fmt.Sprintf("error_%s.log", timeStr))

	var err error
	infoFile, err = os.OpenFile(infoFilename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	errorFile, err = os.OpenFile(errorFilename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	// 创建多输出：同时输出到文件和控制台
	infoWriter := io.MultiWriter(os.Stdout, infoFile)
	errorWriter := io.MultiWriter(os.Stderr, errorFile)

	// 初始化日志记录器
	InfoLogger = log.New(infoWriter, "[INFO] ", log.LstdFlags|log.Lmicroseconds|log.Lshortfile)
	ErrorLogger = log.New(errorWriter, "[ERROR] ", log.LstdFlags|log.Lmicroseconds|log.Lshortfile)

	// 设置默认日志输出
	log.SetOutput(infoWriter)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	currentDate = dateStr
	return nil
}

// checkLogRotation 检查是否需要轮转日志
func checkLogRotation() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		dateStr := time.Now().Format("2006-01-02")
		if dateStr != currentDate {
			if err := rotateLogFiles(); err != nil {
				log.Printf("Failed to rotate log files: %v", err)
			}
		}
	}
}

// Info 记录信息日志
func Info(format string, v ...interface{}) {
	if InfoLogger != nil {
		InfoLogger.Printf(format, v...)
	}
}

// Error 记录错误日志
func Error(format string, v ...interface{}) {
	if ErrorLogger != nil {
		ErrorLogger.Printf(format, v...)
	}
}

// LogRequest 记录请求信息
func LogRequest(method, path, clientIP, provider string, statusCode int, latency time.Duration) {
	Info("Request: method=%s path=%s client=%s provider=%s status=%d latency=%v",
		method, path, clientIP, provider, statusCode, latency)
}
