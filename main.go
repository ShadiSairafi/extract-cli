package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/blakesmith/ar"           // Handles the .deb container
	"github.com/klauspost/compress/zstd" // Blazing fast Zstd decompression
	"github.com/ulikunitz/xz"
)

// Global Magic Numbers
var (
	zipMagic  = []byte{0x50, 0x4B, 0x03, 0x04}
	gzipMagic = []byte{0x1F, 0x8B}
	xzMagic   = []byte{0xFD, 0x37, 0x7A, 0x58, 0x5A, 0x00}
	zstdMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}                   // Standard Zstd framing magic
	debMagic  = []byte{0x21, 0x3C, 0x61, 0x72, 0x63, 0x68, 0x3E} // "!<arch>" string
	tarMagic  = []byte{0x75, 0x73, 0x74, 0x61, 0x72}
)

// AppMetadata defines the local manifest structure for tracking updates
type AppMetadata struct {
	SourceURL string `json:"source_url"`
	Format    string `json:"format"`
	DestDir   string `json:"dest_dir"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: extract <archive-file-or-url>  OR  extract --uninstall <app-dir-name>  OR  extract --update-all")
		os.Exit(1)
	}

	// -----------------------------------------------------------------
	// UPDATE WORKFLOW ROUTER
	// -----------------------------------------------------------------
	if os.Args[1] == "--update-all" {
		runUpdateAll()
		os.Exit(0)
	}

	// -----------------------------------------------------------------
	// UNINSTALL WORKFLOW ROUTER
	// -----------------------------------------------------------------
	if os.Args[1] == "--uninstall" {
		if len(os.Args) < 3 {
			fmt.Println("Error: Please specify the application directory name to uninstall.")
			os.Exit(1)
		}
		runCleanUninstall(os.Args[2])
		os.Exit(0)
	}

	targetInput := os.Args[1]
	var filePath string
	var sourceURL string

	// Handle Remote URLs Transparently
	if strings.HasPrefix(targetInput, "http://") || strings.HasPrefix(targetInput, "https://") {
		sourceURL = targetInput
		fmt.Println("Downloading remote archive payload...")
		downloadedPath, err := downloadToTemp(sourceURL)
		if err != nil {
			fmt.Printf("Error downloading target remote: %v\n", err)
			os.Exit(1)
		}
		filePath = downloadedPath
		defer os.Remove(filePath) // Clean up target temporary file on exit
	} else {
		filePath = targetInput
	}

	processExtraction(filePath, sourceURL, "")
}

func processExtraction(filePath string, sourceURL string, forcedDestDir string) {
	file, err := os.Open(filePath)
	if err != nil {
		fmt.Printf("Error opening file: %v\n", err)
		return
	}
	defer file.Close()

	header := make([]byte, 262)
	_, _ = io.ReadFull(file, header)
	_, _ = file.Seek(0, 0)

	var format string
	if bytes.HasPrefix(header, zipMagic) {
		format = "ZIP"
	} else if bytes.HasPrefix(header, gzipMagic) {
		format = "GZIP"
	} else if bytes.HasPrefix(header, xzMagic) {
		format = "XZ"
	} else if bytes.HasPrefix(header, zstdMagic) {
		format = "ZSTD"
	} else if bytes.HasPrefix(header, debMagic) {
		format = "DEB"
	} else if len(header) >= 262 && bytes.Equal(header[257:262], tarMagic) {
		format = "TAR"
	}

	if format == "" {
		fmt.Println("Error: Unknown or unsupported archive format.")
		return
	}

	var destDir string
	if forcedDestDir != "" {
		destDir = forcedDestDir
	} else {
		fmt.Printf("Detected Format: %s\n", format)
		destDir = promptForLocation()
	}

	// Router Execution with Manifest Context Pass-Through
	if format == "ZIP" {
		extractZip(filePath, destDir, sourceURL)
	} else if format == "GZIP" {
		extractTarGz(file, destDir, sourceURL)
	} else if format == "XZ" {
		extractTarXz(file, destDir, sourceURL)
	} else if format == "ZSTD" {
		extractTarZst(file, destDir, sourceURL)
	} else if format == "DEB" {
		extractDeb(file, destDir, sourceURL)
	} else if format == "TAR" {
		extractRawTar(file, destDir, sourceURL)
	}
}

// -----------------------------------------------------------------
// REMOTE NETWORK UTILITIES
// -----------------------------------------------------------------

func downloadToTemp(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad server status validation: %s", resp.Status)
	}

	tmpFile, err := os.CreateTemp("", "extract-payload-*.tmp")
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	_, err = io.Copy(tmpFile, resp.Body)
	if err != nil {
		return "", err
	}

	return tmpFile.Name(), nil
}

func writeManifest(appDir string, sourceURL string, format string, destDir string) {
	if sourceURL == "" {
		return
	}
	meta := AppMetadata{
		SourceURL: sourceURL,
		Format:    format,
		DestDir:   destDir,
	}
	jsonData, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return
	}
	manifestPath := filepath.Join(appDir, ".extract-meta.json")
	_ = os.WriteFile(manifestPath, jsonData, 0644)
}

func runUpdateAll() {
	home, _ := os.UserHomeDir()
	appsDir := filepath.Join(home, "Apps")

	entries, err := os.ReadDir(appsDir)
	if err != nil {
		fmt.Println("Error: Could not scan central Apps directory.")
		return
	}

	fmt.Println("Checking for application updates...")
	for _, entry := range entries {
		if entry.IsDir() {
			appPath := filepath.Join(appsDir, entry.Name())
			manifestPath := filepath.Join(appPath, ".extract-meta.json")

			if _, err := os.Stat(manifestPath); err == nil {
				fileData, err := os.ReadFile(manifestPath)
				if err != nil {
					continue
				}

				var meta AppMetadata
				if err := json.Unmarshal(fileData, &meta); err != nil {
					continue
				}

				fmt.Printf("Updating target binary sequence: %s\n", entry.Name())
				tmpFile, err := downloadToTemp(meta.SourceURL)
				if err != nil {
					fmt.Printf("  Error downloading update source for %s: %v\n", entry.Name(), err)
					continue
				}

				processExtraction(tmpFile, meta.SourceURL, meta.DestDir)
				os.Remove(tmpFile)
			}
		}
	}
	fmt.Println("All targeted applications verified and synchronized.")
}

// -----------------------------------------------------------------
// PROMPT MENU DRIVERS
// -----------------------------------------------------------------

func promptForLocation() string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("\nSelect Destination Method:")
	fmt.Println("  [1] Terminal Input (Type path manually)")
	fmt.Println("  [2] GUI File Manager Window (Visual popup)")
	fmt.Print("Choose option [1-2, default: 1]: ")

	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)

	if choice == "2" {
		return promptGuiLocation()
	}
	return promptTerminalLocation()
}

func promptGuiLocation() string {
	fmt.Println("Launching system file manager window...")
	cmd := exec.Command("zenity", "--file-selection", "--directory", "--title=Select Extraction Destination")
	output, err := cmd.Output()
	if err != nil {
		currentDir, _ := os.Getwd()
		fmt.Println("Notification: GUI cancelled or closed. Defaulting to current terminal path.")
		return currentDir
	}
	return strings.TrimSpace(string(output))
}

func promptTerminalLocation() string {
	currentDir, _ := os.Getwd()
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("Enter destination path [Press Enter for %s]: ", currentDir)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	if input == "" {
		return currentDir
	}
	if strings.HasPrefix(input, "~") {
		home, _ := os.UserHomeDir()
		input = filepath.Join(home, input[1:])
	}
	return filepath.Clean(input)
}

// -----------------------------------------------------------------
// COMPRESSION ROUTERS (.ZST & .DEB)
// -----------------------------------------------------------------

func extractTarZst(fileReader io.Reader, destDir string, sourceURL string) {
	zstdReader, err := zstd.NewReader(fileReader)
	if err != nil {
		fmt.Printf("Error initializing Zstd decompressor: %v\n", err)
		return
	}
	defer zstdReader.Close()

	runTarExtraction(zstdReader, destDir, sourceURL, "ZSTD")
}

func extractDeb(fileReader io.Reader, destDir string, sourceURL string) {
	arReader := ar.NewReader(fileReader)

	for {
		header, err := arReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Printf("Error reading deb/ar header: %v\n", err)
			return
		}

		if bytes.HasPrefix([]byte(header.Name), []byte("data.tar")) {
			fmt.Printf("Found package data stream: %s\n", header.Name)

			innerHeader := make([]byte, 4)
			_, _ = io.ReadFull(arReader, innerHeader)

			combinedReader := io.MultiReader(bytes.NewReader(innerHeader), arReader)

			if bytes.HasPrefix(innerHeader, xzMagic) {
				fmt.Println("Decoding inner data payload using XZ Engine...")
				extractTarXz(combinedReader, destDir, sourceURL)
			} else if bytes.HasPrefix(innerHeader, zstdMagic) {
				fmt.Println("Decoding inner data payload using Zstd Engine...")
				extractTarZst(combinedReader, destDir, sourceURL)
			} else if bytes.HasPrefix(innerHeader, gzipMagic) {
				fmt.Println("Decoding inner data payload using Gzip Engine...")
				extractTarGz(combinedReader, destDir, sourceURL)
			}
		}
	}
}

// -----------------------------------------------------------------
// CORE ENGINES
// -----------------------------------------------------------------

func extractRawTar(fileReader io.Reader, destDir string, sourceURL string) {
	runTarExtraction(fileReader, destDir, sourceURL, "TAR")
}

func extractTarGz(fileReader io.Reader, destDir string, sourceURL string) {
	gzipReader, err := gzip.NewReader(fileReader)
	if err != nil {
		return
	}
	defer gzipReader.Close()
	runTarExtraction(gzipReader, destDir, sourceURL, "GZIP")
}

func extractTarXz(fileReader io.Reader, destDir string, sourceURL string) {
	xzReader, err := xz.NewReader(fileReader)
	if err != nil {
		return
	}
	runTarExtraction(xzReader, destDir, sourceURL, "XZ")
}

func runTarExtraction(reader io.Reader, destDir string, sourceURL string, format string) {
	tarReader := tar.NewReader(reader)
	var topLevelDir string
	var totalBytesWritten int64

	// Global overwrite default config on automated script executions
	globalOverwriteChoice := ""
	if sourceURL != "" {
		globalOverwriteChoice = "y"
	}

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return
		}

		if topLevelDir == "" {
			cleaned := filepath.ToSlash(header.Name)
			elements := bytes.Split([]byte(cleaned), []byte("/"))
			if len(elements) > 0 && string(elements[0]) != "." {
				topLevelDir = string(elements[0])
			}
		}

		targetPath := filepath.Join(destDir, filepath.Clean(header.Name))

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(targetPath, os.FileMode(header.Mode))
		case tar.TypeReg:
			if _, statErr := os.Stat(targetPath); statErr == nil {
				if globalOverwriteChoice == "" {
					fmt.Print("\r\033[K")
					reader := bufio.NewReader(os.Stdin)
					fmt.Printf("Conflict: File '%s' already exists. Overwrite all conflicts? [y/n]: ", targetPath)
					input, _ := reader.ReadString('\n')
					globalOverwriteChoice = strings.TrimSpace(strings.ToLower(input))
				}
				if globalOverwriteChoice != "y" && globalOverwriteChoice != "yes" {
					continue
				}
			}

			_ = os.MkdirAll(filepath.Dir(targetPath), 0755)

			destinationFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				continue
			}
			written, _ := io.Copy(destinationFile, tarReader)
			totalBytesWritten += written
			destinationFile.Close()

			fmt.Printf("\rExtracting: %d MB...", totalBytesWritten/(1024*1024))
		}
	}
	fmt.Println("\nExtraction complete.")

	if topLevelDir != "" {
		appName := topLevelDir
		if len(appName) > 4 && appName[len(appName)-4:] == "-x64" {
			appName = appName[:len(appName)-4]
		}

		trueFullPath := filepath.Join(destDir, topLevelDir)
		absDest, _ := filepath.Abs(trueFullPath)
		fmt.Printf("Target Destination: %s\n", absDest)

		writeManifest(trueFullPath, sourceURL, format, destDir)
		createLinuxShortcut(appName, trueFullPath)
	}
}

// -----------------------------------------------------------------
// NATIVE ZIP ENGINE
// -----------------------------------------------------------------

func extractZip(path string, destDir string, sourceURL string) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		fmt.Printf("Error opening zip: %v\n", err)
		return
	}
	defer reader.Close()

	var topLevelDir string
	var totalBytes int64
	var currentBytes int64
	globalOverwriteChoice := ""
	if sourceURL != "" {
		globalOverwriteChoice = "y"
	}

	for _, file := range reader.File {
		totalBytes += int64(file.UncompressedSize64)
	}

	for _, file := range reader.File {
		if topLevelDir == "" {
			cleaned := filepath.ToSlash(file.Name)
			elements := bytes.Split([]byte(cleaned), []byte("/"))
			if len(elements) > 0 && string(elements[0]) != "." {
				topLevelDir = string(elements[0])
			}
		}

		targetPath := filepath.Join(destDir, filepath.Clean(file.Name))

		if file.FileInfo().IsDir() {
			os.MkdirAll(targetPath, file.Mode())
			continue
		}

		if _, statErr := os.Stat(targetPath); statErr == nil {
			if globalOverwriteChoice == "" {
				fmt.Print("\r\033[K")
				stdinReader := bufio.NewReader(os.Stdin)
				fmt.Printf("Conflict: File '%s' already exists. Overwrite all? [y/n]: ", targetPath)
				input, _ := stdinReader.ReadString('\n')
				globalOverwriteChoice = strings.TrimSpace(strings.ToLower(input))
			}
			if globalOverwriteChoice != "y" && globalOverwriteChoice != "yes" {
				currentBytes += int64(file.UncompressedSize64)
				continue
			}
		}

		_ = os.MkdirAll(filepath.Dir(targetPath), 0755)

		destinationFile, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			fmt.Printf("Error writing file %s: %v\n", targetPath, err)
			continue
		}

		zippedFile, err := file.Open()
		if err == nil {
			written, _ := io.Copy(destinationFile, zippedFile)
			currentBytes += written
			zippedFile.Close()

			printProgressBar(currentBytes, totalBytes)
		}
		destinationFile.Close()
	}
	fmt.Println("\nExtraction complete.")

	if topLevelDir != "" {
		appName := topLevelDir
		if len(appName) > 4 && appName[len(appName)-4:] == "-x64" {
			appName = appName[:len(appName)-4]
		}

		trueFullPath := filepath.Join(destDir, topLevelDir)
		absDest, _ := filepath.Abs(trueFullPath)
		fmt.Printf("Target Destination: %s\n", absDest)

		writeManifest(trueFullPath, sourceURL, "ZIP", destDir)
		createLinuxShortcut(appName, trueFullPath)
	}
}

func printProgressBar(current, total int64) {
	const barWidth = 30
	if total == 0 {
		return
	}

	percentage := float64(current) / float64(total) * 100
	completedWidth := int((float64(current) / float64(total)) * barWidth)

	var bar bytes.Buffer
	bar.WriteString("[")
	for i := 0; i < barWidth; i++ {
		if i < completedWidth {
			bar.WriteString("=")
		} else if i == completedWidth {
			bar.WriteString(">")
		} else {
			bar.WriteString("-")
		}
	}
	bar.WriteString("]")

	fmt.Printf("\rExtracting: %s %.1f%% (%d/%d MB)",
		bar.String(), percentage, current/(1024*1024), total/(1024*1024))
}

// -----------------------------------------------------------------
// UNINSTALL ENGINE
// -----------------------------------------------------------------

func runCleanUninstall(targetDirName string) {
	targetDir := filepath.Clean(targetDirName)

	fmt.Printf("Initializing uninstall sequence for app directory: %s\n", targetDir)

	homeDir, _ := os.UserHomeDir()
	shortcutFile := filepath.Join(homeDir, ".local", "share", "applications", fmt.Sprintf("%s.desktop", targetDir))

	if _, err := os.Stat(shortcutFile); err == nil {
		err := os.Remove(shortcutFile)
		if err != nil {
			fmt.Printf("  Error: Failed to remove desktop shortcut: %v\n", err)
		} else {
			fmt.Println("  Successfully removed system desktop shortcut.")
		}
	} else {
		fmt.Println("  No active desktop shortcuts found.")
	}

	if _, err := os.Stat(targetDir); err == nil {
		err := os.RemoveAll(targetDir)
		if err != nil {
			fmt.Printf("  Error: Failed to remove application directory contents: %v\n", err)
		} else {
			fmt.Printf("  Successfully deleted local binary directory structures for '%s'.\n", targetDir)
		}
	} else {
		fmt.Printf("  Warning: Could not find application directory '%s' in current path.\n", targetDir)
	}

	fmt.Println("Uninstall complete.")
}

// -----------------------------------------------------------------
// SYSTEM SHORTCUT SERVICE
// -----------------------------------------------------------------

func createLinuxShortcut(appName string, dirPath string) {
	absDir, err := filepath.Abs(dirPath)
	if err != nil {
		fmt.Printf("Error getting absolute path: %v\n", err)
		return
	}

	var execPath string
	var iconPath string
	isElectronApp := false

	_ = filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && (info.Mode()&0111 != 0) {
			if execPath == "" || filepath.Base(path) == appName {
				execPath = path
			}
		}
		if filepath.Base(path) == "chrome-sandbox" {
			isElectronApp = true
		}

		ext := filepath.Ext(path)
		if ext == ".png" || ext == ".svg" {
			if iconPath == "" || bytes.Contains([]byte(path), []byte("icon")) || bytes.Contains([]byte(path), []byte("resources")) {
				iconPath = path
			}
		}
		return nil
	})

	if iconPath == "" {
		iconPath = "system-run"
	}
	if execPath == "" {
		fmt.Println("Warning: Could not locate an executable binary file.")
		return
	}

	var execStr string
	if isElectronApp {
		binaryName := filepath.Base(execPath)
		execStr = fmt.Sprintf("bash -c \"pkill -9 -x '%s' 2>/dev/null; '%s' --no-sandbox\"", binaryName, execPath)
	} else {
		execStr = fmt.Sprintf(`"%s"`, execPath)
	}

	desktopContent := fmt.Sprintf(`[Desktop Entry]
Type=Application
Version=1.0
Name=%s
Exec=%s
Path=%s
Icon=%s
Terminal=false
Categories=Utility;Development;
StartupNotify=true
`, appName, execStr, absDir, iconPath)

	homeDir, _ := os.UserHomeDir()
	shortcutDir := filepath.Join(homeDir, ".local", "share", "applications")
	_ = os.MkdirAll(shortcutDir, 0755)

	shortcutFile := filepath.Join(shortcutDir, fmt.Sprintf("%s.desktop", filepath.Base(dirPath)))

	err = os.WriteFile(shortcutFile, []byte(desktopContent), 0755)
	if err != nil {
		fmt.Printf("Error: Failed to create desktop shortcut: %v\n", err)
		return
	}

	_ = os.Chmod(shortcutFile, 0755)
	fmt.Printf("Created Desktop Shortcut: %s\n", shortcutFile)
}
