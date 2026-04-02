@echo off
REM SwitchAI Build Script
REM This script builds the SwitchAI binary for Windows and Linux with embedded web assets

echo ========================================
echo SwitchAI Build Script
echo ========================================
echo.

REM Check if Go is installed
where go >nul 2>nul
if %errorlevel% neq 0 (
    echo ERROR: Go is not installed or not in PATH
    exit /b 1
)

REM Get git commit hash and version info
for /f "delims=" %%i in ('git revparse HEAD 2^>nul') do set GIT_COMMIT=%%i
for /f "delims=" %%i in ('git describe --tags --abbrev=0 2^>nul') do set GIT_TAG=%%i

REM Parse version from tag (format: v1.2.3)
set VERSION_MAJOR=0
set VERSION_MINOR=0
set VERSION_PATCH=0
if not "%GIT_TAG%"=="" (
    for /f "tokens=1,2,3 delims=." %%a in ("%GIT_TAG:~1%") do (
        set VERSION_MAJOR=%%a
        set VERSION_MINOR=%%b
        set VERSION_PATCH=%%c
    )
)

echo Version: v%VERSION_MAJOR%.%VERSION_MINOR%.%VERSION_PATCH%
echo Commit: %GIT_COMMIT%
echo.

REM Build ldflags
set LDFLAGS=-s -w -X main.versionMajor=%VERSION_MAJOR% -X main.versionMinor=%VERSION_MINOR% -X main.versionPatch=%VERSION_PATCH% -X main.gitCommit=%GIT_COMMIT%

echo [1/2] Building for Windows (amd64)...
set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64
go build -ldflags "%LDFLAGS%" -o dist\switchai-windows-amd64.exe
if %errorlevel% neq 0 (
    echo ERROR: Windows build failed
    exit /b 1
)
echo Windows build completed: dist\switchai-windows-amd64.exe

echo.
echo [2/2] Building for Linux (amd64)...
set GOOS=linux
set GOARCH=amd64
go build -ldflags "%LDFLAGS%" -o dist\switchai-linux-amd64
if %errorlevel% neq 0 (
    echo ERROR: Linux build failed
    exit /b 1
)
echo Linux build completed: dist\switchai-linux-amd64

echo.
echo ========================================
echo Build completed successfully!
echo ========================================
echo.
echo Output files:
echo   - dist\switchai-windows-amd64.exe (web assets embedded)
echo   - dist\switchai-linux-amd64 (web assets embedded)
echo.
echo Usage:
echo   Windows: switchai-windows-amd64.exe -p 7777
echo   Linux:   ./switchai-linux-amd64 -p 7777
echo.
echo Service management:
echo   Install:   switchai-windows-amd64.exe -install
echo   Uninstall: switchai-windows-amd64.exe -uninstall
echo.

pause
