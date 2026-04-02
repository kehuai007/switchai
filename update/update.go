package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"switchai/logger"
)

// Version 信息
type Version struct {
	Major int
	Minor int
	Patch int
}

const (
	// GitHub API 地址
	GitHubAPIURL = "https://api.github.com/repos/kehuai007/switchai/releases/latest"

	// 检查间隔（1小时）
	CheckInterval = time.Hour
)

// CheckResult 检查结果
type CheckResult struct {
	HasUpdate          bool
	Current            Version
	Latest             Version
	CurrentCommit     string // 当前 commit hash
	LatestCommit      string // 最新 commit hash
	DownloadURL       string
	ReleaseNotes      string
}

var (
	// 当前版本（编译时注入）
	CurrentVersion = Version{Major: 0, Minor: 0, Patch: 0}

	// 版本字符串（编译时注入）
	VersionString = "v0.0.0"

	// 当前 git commit hash（编译时注入）
	CurrentCommit = ""

	// 检查间隔
	checkInterval = CheckInterval
)

// Init 初始化版本信息
func Init(major, minor, patch int) {
	CurrentVersion = Version{Major: major, Minor: minor, Patch: patch}
	VersionString = fmt.Sprintf("v%d.%d.%d", major, minor, patch)
}

// InitWithCommit 初始化版本和 commit 信息
func InitWithCommit(major, minor, patch int, commit string) {
	CurrentVersion = Version{Major: major, Minor: minor, Patch: patch}
	VersionString = fmt.Sprintf("v%d.%d.%d", major, minor, patch)
	CurrentCommit = commit
	if len(CurrentCommit) > 7 {
		CurrentCommit = CurrentCommit[:7] // 截取前7位（短 hash）
	}
}

// InitWithCommitStr 初始化版本和 commit 信息（字符串版本，用于 ldflags 注入）
func InitWithCommitStr(major, minor, patch, commit string) {
	maj, _ := strconv.Atoi(major)
	min, _ := strconv.Atoi(minor)
	pat, _ := strconv.Atoi(patch)
	InitWithCommit(maj, min, pat, commit)
}

// SetCheckInterval 设置检查间隔
func SetCheckInterval(interval time.Duration) {
	checkInterval = interval
}

// ParseVersion 解析版本字符串
func ParseVersion(s string) Version {
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	v := Version{}
	if len(parts) >= 1 {
		v.Major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) >= 2 {
		v.Minor, _ = strconv.Atoi(parts[1])
	}
	if len(parts) >= 3 {
		v.Patch, _ = strconv.Atoi(parts[2])
	}
	return v
}

// String 返回版本字符串
func (v Version) String() string {
	return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare 比较版本，返回 0 相等，1 大于，-1 小于
func (v Version) Compare(other Version) int {
	if v.Major != other.Major {
		return v.Major - other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor - other.Minor
	}
	return v.Patch - other.Patch
}

// GitHubRelease GitHub release 结构
type GitHubRelease struct {
	TagName         string `json:"tag_name"`
	Name            string `json:"name"`
	Body            string `json:"body"`
	HTMLURL         string `json:"html_url"`
	TargetCommitish string `json:"target_commitish"` // release 目标 commit/branch
	Assets          []Asset `json:"assets"`
}

// Asset release 资源
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size              int64  `json:"size"`
}

// CheckForUpdate 检查更新
func CheckForUpdate() (*CheckResult, error) {
	result := &CheckResult{
		Current:       CurrentVersion,
		CurrentCommit: CurrentCommit,
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest("GET", GitHubAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %v", err)
	}

	req.Header.Set("User-Agent", "SwitchAI-Update-Checker")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("检查更新失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 返回错误状态码: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %v", err)
	}

	var release GitHubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("解析响应失败: %v", err)
	}

	result.Latest = ParseVersion(release.TagName)
	result.LatestCommit = release.TargetCommitish
	result.ReleaseNotes = release.Body

	platform := getPlatformAssetName()
	for _, asset := range release.Assets {
		if asset.Name == platform {
			result.DownloadURL = asset.BrowserDownloadURL
			break
		}
	}

	// 判断是否有更新：版本号更新 或 commit hash 不同
	if result.Latest.Compare(result.Current) > 0 {
		result.HasUpdate = true
	}
	// 如果版本号相同但 commit 不同，也认为有更新（热修复）
	if !result.HasUpdate && CurrentCommit != "" && result.LatestCommit != "" && CurrentCommit != result.LatestCommit {
		result.HasUpdate = true
	}

	return result, nil
}

func getPlatformAssetName() string {
	switch runtime.GOOS {
	case "windows":
		return "switchai-windows-amd64.exe"
	case "linux":
		return "switchai-linux-amd64"
	case "darwin":
		return "switchai-darwin-amd64"
	default:
		return "switchai-" + runtime.GOOS + "-amd64"
	}
}

// IsRunningAsService 检测是否以服务方式运行
func IsRunningAsService() bool {
	switch runtime.GOOS {
	case "windows":
		return isWindowsService()
	case "linux":
		return isLinuxService()
	default:
		return false
	}
}

func isWindowsService() bool {
	ppid := os.Getppid()
	ppidStr := fmt.Sprintf("%d", ppid)

	cmd := exec.Command("tasklist", "/FI", "PID eq "+ppidStr, "/FO", "CSV", "/NH")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	return strings.Contains(strings.ToLower(string(output)), "services.exe")
}

func isLinuxService() bool {
	cmd := exec.Command("ps", "-p", "1", "-o", "comm=")
	output, err := cmd.Output()
	if err == nil && strings.Contains(string(output), "systemd") {
		return true
	}

	cmd = exec.Command("ps", "-eo", "comm=")
	output, err = cmd.Output()
	if err == nil && strings.Contains(string(output), "systemd") {
		return true
	}

	return false
}

// AutoUpdater 自动更新器
type AutoUpdater struct {
	stopChan       chan struct{}
	updateCallback func(*CheckResult) // 更新回调函数
}

// NewAutoUpdater 创建自动更新器
func NewAutoUpdater() *AutoUpdater {
	return &AutoUpdater{
		stopChan: make(chan struct{}),
	}
}

// SetUpdateCallback 设置更新回调
func (u *AutoUpdater) SetUpdateCallback(callback func(*CheckResult)) {
	u.updateCallback = callback
}

// Start 启动自动更新检查
func (u *AutoUpdater) Start() {
	logger.Info("自动更新服务启动，服务模式: %v, 检查间隔: %s", IsRunningAsService(), checkInterval)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	// 启动后立即检查一次
	u.check()

	for {
		select {
		case <-ticker.C:
			u.check()
		case <-u.stopChan:
			logger.Info("自动更新服务已停止")
			return
		}
	}
}

// Stop 停止自动更新检查
func (u *AutoUpdater) Stop() {
	close(u.stopChan)
}

// check 检查更新
func (u *AutoUpdater) check() {
	result, err := CheckForUpdate()
	if err != nil {
		logger.Error("检查更新失败: %v", err)
		return
	}

	if result.HasUpdate {
		logger.Info("发现新版本: %s (当前: %s)", result.Latest.String(), result.Current.String())
		if u.updateCallback != nil {
			u.updateCallback(result)
		}
	}
}

// DownloadAndInstall 下载并安装更新
func DownloadAndInstall(downloadURL string) error {
	logger.Info("开始下载更新: %s", downloadURL)

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取可执行文件路径失败: %v", err)
	}

	exePath, err = filepath.Abs(exePath)
	if err != nil {
		return fmt.Errorf("获取绝对路径失败: %v", err)
	}

	tempDir := filepath.Dir(exePath)
	newExePath := filepath.Join(tempDir, "switchai_new.exe")

	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	resp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("下载失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回错误状态码: %d", resp.StatusCode)
	}

	tmpFile, err := os.Create(newExePath)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %v", err)
	}

	_, err = io.Copy(tmpFile, resp.Body)
	tmpFile.Close()
	if err != nil {
		os.Remove(newExePath)
		return fmt.Errorf("写入文件失败: %v", err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(newExePath, 0755); err != nil {
			os.Remove(newExePath)
			return fmt.Errorf("设置执行权限失败: %v", err)
		}
	}

	logger.Info("更新下载完成，准备替换二进制文件")

	if err := replaceBinary(exePath, newExePath); err != nil {
		os.Remove(newExePath)
		return fmt.Errorf("替换二进制文件失败: %v", err)
	}

	logger.Info("更新安装成功，新版本将在重启后生效")
	return nil
}

func replaceBinary(oldPath, newPath string) error {
	if runtime.GOOS == "windows" {
		return replaceBinaryWindows(oldPath, newPath)
	}
	return replaceBinaryUnix(oldPath, newPath)
}

func replaceBinaryWindows(oldPath, newPath string) error {
	oldDir := filepath.Dir(oldPath)
	backupPath := filepath.Join(oldDir, "switchai_old.exe")

	os.Remove(backupPath)

	if err := os.Rename(oldPath, backupPath); err != nil {
		return fmt.Errorf("重命名旧文件失败: %v", err)
	}

	if err := os.Rename(newPath, oldPath); err != nil {
		os.Rename(backupPath, oldPath)
		return fmt.Errorf("重命名新文件失败: %v", err)
	}

	cmd := exec.Command(oldPath)
	cmd.Dir = oldDir
	cmd.Start()

	go func() {
		time.Sleep(5 * time.Second)
		os.Remove(backupPath)
	}()

	return nil
}

func replaceBinaryUnix(oldPath, newPath string) error {
	backupPath := oldPath + ".old"

	os.Remove(backupPath)

	if err := os.Rename(oldPath, backupPath); err != nil {
		return fmt.Errorf("重命名旧文件失败: %v", err)
	}

	if err := os.Rename(newPath, oldPath); err != nil {
		os.Rename(backupPath, oldPath)
		return fmt.Errorf("重命名新文件失败: %v", err)
	}

	if err := os.Chmod(oldPath, 0755); err != nil {
		logger.Error("设置执行权限失败: %v", err)
	}

	return nil
}

// RestartService 重启服务
func RestartService() error {
	switch runtime.GOOS {
	case "windows":
		return restartWindowsService()
	case "linux":
		return restartLinuxService()
	default:
		return fmt.Errorf("不支持的平台: %s", runtime.GOOS)
	}
}

func restartWindowsService() error {
	logger.Info("正在重启 Windows 服务...")

	stopCmd := exec.Command("sc", "stop", "SwitchAI")
	stopCmd.Run()

	time.Sleep(3 * time.Second)

	startCmd := exec.Command("sc", "start", "SwitchAI")
	if err := startCmd.Run(); err != nil {
		return fmt.Errorf("启动服务失败: %v", err)
	}

	logger.Info("服务已重启")
	return nil
}

func restartLinuxService() error {
	logger.Info("正在重启 systemd 服务...")

	cmd := exec.Command("systemctl", "restart", "switchai")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("重启服务失败: %v", err)
	}

	logger.Info("服务已重启")
	return nil
}
