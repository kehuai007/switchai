package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const (
	ServiceName        = "SwitchAI"
	ServiceDisplayName = "SwitchAI API Proxy Service"
	ServiceDescription = "Claude API aggregation and proxy service"
)

// Install installs the service
func Install(port string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %v", err)
	}

	exePath, err = filepath.Abs(exePath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %v", err)
	}

	if runtime.GOOS == "windows" {
		return installWindows(exePath, port)
	}
	return installLinux(exePath, port)
}

// Uninstall uninstalls the service
func Uninstall() error {
	if runtime.GOOS == "windows" {
		return uninstallWindows()
	}
	return uninstallLinux()
}

func installWindows(exePath, port string) error {
	installPath := `C:\Program Files\SwitchAI`

	// Create installation directory
	if err := os.MkdirAll(installPath, 0755); err != nil {
		return fmt.Errorf("failed to create install directory: %v", err)
	}

	targetPath := filepath.Join(installPath, "switchai.exe")

	// Check if service exists and stop it
	fmt.Println("Checking for existing service...")
	queryCmd := exec.Command("sc", "query", ServiceName)
	if queryCmd.Run() == nil {
		fmt.Println("Stopping existing service...")
		stopCmd := exec.Command("sc", "stop", ServiceName)
		stopCmd.Run()

		// Wait for service to stop
		time.Sleep(3 * time.Second)

		// Kill process if still running
		killCmd := exec.Command("taskkill", "/F", "/IM", "switchai.exe")
		killCmd.Run()

		time.Sleep(1 * time.Second)
	}

	// Check if target file exists and compare MD5
	if _, err := os.Stat(targetPath); err == nil {
		fmt.Println("Removing old binary...")
		// Try to remove old file
		for i := 0; i < 5; i++ {
			if err := os.Remove(targetPath); err == nil {
				break
			}
			time.Sleep(1 * time.Second)
		}
	}

	// Copy binary to installation directory
	fmt.Printf("Copying binary to %s...\n", installPath)
	input, err := os.ReadFile(exePath)
	if err != nil {
		return fmt.Errorf("failed to read source file: %v", err)
	}

	if err := os.WriteFile(targetPath, input, 0755); err != nil {
		return fmt.Errorf("failed to write target file: %v", err)
	}

	// Delete existing service
	deleteCmd := exec.Command("sc", "delete", ServiceName)
	deleteCmd.Run()
	time.Sleep(1 * time.Second)

	// Create service
	fmt.Println("Creating service...")
	binPath := fmt.Sprintf(`"%s" -p %s`, targetPath, port)
	createCmd := exec.Command("sc", "create", ServiceName,
		"binPath=", binPath,
		"DisplayName=", ServiceDisplayName,
		"start=", "auto")

	if output, err := createCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create service: %v\nOutput: %s", err, output)
	}

	// Set service description
	descCmd := exec.Command("sc", "description", ServiceName, ServiceDescription)
	descCmd.Run()

	// Configure service to restart on failure
	recoveryCmd := exec.Command("sc", "failure", ServiceName, "reset=", "86400", "actions=", "restart/5000/restart/5000/restart/5000")
	recoveryCmd.Run()

	// Start service
	fmt.Println("Starting service...")
	startCmd := exec.Command("sc", "start", ServiceName)
	if output, err := startCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to start service: %v\nOutput: %s", err, output)
	}

	fmt.Printf("\n✅ Service installed successfully!\n")
	fmt.Printf("Installation path: %s\n", installPath)
	fmt.Printf("Service name: %s\n", ServiceName)
	fmt.Printf("Port: %s\n", port)
	fmt.Println("\nService management commands:")
	fmt.Printf("  Start:   sc start %s\n", ServiceName)
	fmt.Printf("  Stop:    sc stop %s\n", ServiceName)
	fmt.Printf("  Status:  sc query %s\n", ServiceName)

	return nil
}

func uninstallWindows() error {
	fmt.Println("Stopping service...")
	stopCmd := exec.Command("sc", "stop", ServiceName)
	stopCmd.Run()

	time.Sleep(2 * time.Second)

	fmt.Println("Deleting service...")
	deleteCmd := exec.Command("sc", "delete", ServiceName)
	if output, err := deleteCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete service: %v\nOutput: %s", err, output)
	}

	// Kill any remaining processes
	killCmd := exec.Command("taskkill", "/F", "/IM", "switchai.exe")
	killCmd.Run()

	installPath := `C:\Program Files\SwitchAI`
	fmt.Printf("Removing binary from: %s\n", installPath)

	time.Sleep(1 * time.Second)

	// Only remove the binary, keep data files
	binaryPath := filepath.Join(installPath, "switchai.exe")
	if err := os.Remove(binaryPath); err != nil {
		fmt.Printf("Warning: failed to remove binary: %v\n", err)
	}

	fmt.Println("\n✅ Service uninstalled successfully!")
	fmt.Printf("Data files preserved in: %s\n", installPath)
	fmt.Println("(config.json, history.json, logs/)")
	return nil
}

func installLinux(exePath, port string) error {
	installPath := "/usr/local/bin"
	targetPath := filepath.Join(installPath, "switchai")

	// Check if systemd service exists
	servicePath := "/etc/systemd/system/switchai.service"
	if _, err := os.Stat(servicePath); err == nil {
		fmt.Println("Stopping existing service...")
		stopCmd := exec.Command("systemctl", "stop", "switchai")
		stopCmd.Run()
		time.Sleep(2 * time.Second)
	}

	// Copy binary
	fmt.Printf("Copying binary to %s...\n", installPath)
	input, err := os.ReadFile(exePath)
	if err != nil {
		return fmt.Errorf("failed to read source file: %v", err)
	}

	if err := os.WriteFile(targetPath, input, 0755); err != nil {
		return fmt.Errorf("failed to write target file: %v", err)
	}

	// Create systemd service file
	fmt.Println("Creating systemd service...")
	serviceContent := fmt.Sprintf(`[Unit]
Description=%s
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/usr/local/bin
ExecStart=%s -p %s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, ServiceDescription, targetPath, port)

	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to create service file: %v", err)
	}

	// Reload systemd
	fmt.Println("Reloading systemd...")
	reloadCmd := exec.Command("systemctl", "daemon-reload")
	if err := reloadCmd.Run(); err != nil {
		return fmt.Errorf("failed to reload systemd: %v", err)
	}

	// Enable service
	fmt.Println("Enabling service...")
	enableCmd := exec.Command("systemctl", "enable", "switchai")
	if err := enableCmd.Run(); err != nil {
		return fmt.Errorf("failed to enable service: %v", err)
	}

	// Start service
	fmt.Println("Starting service...")
	startCmd := exec.Command("systemctl", "start", "switchai")
	if err := startCmd.Run(); err != nil {
		return fmt.Errorf("failed to start service: %v", err)
	}

	fmt.Printf("\n✅ Service installed successfully!\n")
	fmt.Printf("Installation path: %s\n", installPath)
	fmt.Printf("Service name: switchai\n")
	fmt.Printf("Port: %s\n", port)
	fmt.Println("\nService management commands:")
	fmt.Println("  Start:   systemctl start switchai")
	fmt.Println("  Stop:    systemctl stop switchai")
	fmt.Println("  Status:  systemctl status switchai")
	fmt.Println("  Logs:    journalctl -u switchai -f")

	return nil
}

func uninstallLinux() error {
	servicePath := "/etc/systemd/system/switchai.service"

	fmt.Println("Stopping service...")
	stopCmd := exec.Command("systemctl", "stop", "switchai")
	stopCmd.Run()

	fmt.Println("Disabling service...")
	disableCmd := exec.Command("systemctl", "disable", "switchai")
	disableCmd.Run()

	fmt.Println("Removing service file...")
	if err := os.Remove(servicePath); err != nil {
		fmt.Printf("Warning: failed to remove service file: %v\n", err)
	}

	fmt.Println("Reloading systemd...")
	reloadCmd := exec.Command("systemctl", "daemon-reload")
	reloadCmd.Run()

	fmt.Println("Removing binary...")
	binaryPath := "/usr/local/bin/switchai"
	if err := os.Remove(binaryPath); err != nil {
		fmt.Printf("Warning: failed to remove binary: %v\n", err)
	}

	fmt.Println("\n✅ Service uninstalled successfully!")
	fmt.Println("Data files preserved in current directory")
	fmt.Println("(config.json, history.json, logs/)")
	return nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		targetPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			return os.MkdirAll(targetPath, info.Mode())
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(targetPath, data, info.Mode())
	})
}
