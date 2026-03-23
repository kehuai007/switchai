package appdata

import (
	"os"
	"path/filepath"
	"strings"
)

// 数据目录路径
var dataDir string

// Init 初始化数据目录，使用进程名作为目录名
func Init() error {
	// 获取可执行文件名称
	execName := filepath.Base(os.Args[0])

	// 移除扩展名（Windows 上是 .exe）
	execName = strings.TrimSuffix(execName, ".exe")

	// 如果名称为空，使用默认名称
	if execName == "" || execName == "." {
		execName = "switchai"
	}

	// 创建 .xxx 格式的目录
	dataDir = filepath.Join("." + execName)

	// 创建目录
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}

	return nil
}

// GetDataDir 返回数据目录路径
func GetDataDir() string {
	return dataDir
}

// GetConfigPath 返回配置文件路径
func GetConfigPath(filename string) string {
	return filepath.Join(dataDir, filename)
}

// GetLogDir 返回日志目录路径
func GetLogDir() string {
	return filepath.Join(dataDir, "logs")
}

// EnsureDir 确保目录存在
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0755)
}