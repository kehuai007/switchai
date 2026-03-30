#!/bin/bash
# SwitchAI Build Script
# This script builds the SwitchAI binary for Windows and Linux with embedded web assets

set -e

echo "========================================"
echo "SwitchAI Build Script"
echo "========================================"
echo ""

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "ERROR: Go is not installed or not in PATH"
    exit 1
fi

# Create dist directory
mkdir -p dist

echo "[1/2] Building for Windows (amd64)..."
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dist/switchai-windows-amd64.exe
echo "Windows build completed: dist/switchai-windows-amd64.exe"

echo ""
echo "[2/2] Building for Linux (amd64)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dist/switchai-linux-amd64
echo "Linux build completed: dist/switchai-linux-amd64"

echo ""
echo "========================================"
echo "Build completed successfully!"
echo "========================================"
echo ""
echo "Output files:"
echo "  - dist/switchai-windows-amd64.exe (web assets embedded)"
echo "  - dist/switchai-linux-amd64 (web assets embedded)"
echo ""
echo "Usage:"
echo "  Windows: switchai-windows-amd64.exe -p 7777"
echo "  Linux:   ./switchai-linux-amd64 -p 7777"
echo ""
echo "Service management:"
echo "  Install:   switchai-windows-amd64.exe -install"
echo "  Uninstall: switchai-windows-amd64.exe -uninstall"
echo ""

# Make linux binary executable
chmod +x dist/switchai-linux-amd64 2>/dev/null || true
