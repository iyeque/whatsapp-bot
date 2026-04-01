package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"whatsapp-gpt-bot/dashboard"
	"whatsapp-gpt-bot/whatsapp"

	"github.com/joho/godotenv"

	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

const (
	LOG_FILE = "whatsapp-bot.log"
	DB_PATH  = "file:whatsapp.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&cache=shared&mode=rwc"
)

func main() {
	// Load .env file
	err := godotenv.Load()
	if err != nil {
		fmt.Println("Error loading .env file, proceeding with environment variables")
	}

	fmt.Println("Starting WhatsApp bot manager...")

	// Initialize and start the metrics dashboard
	if err := dashboard.Start(); err != nil {
		fmt.Printf("Failed to start metrics dashboard: %v\n", err)
		return
	}
	fmt.Println("Performance dashboard initialized...")

	logFile, err := setupLogging()
	if err != nil {
		fmt.Printf("Failed to set up logging: %v\n", err)
		return
	}
	defer logFile.Close()

	logger := waLog.Stdout("Bot", "INFO", false)
	fmt.Println("Logger initialized...")

	// Start external services (Whisper, TTS) if configured
	startExternalServices(logger)

	accountManager, err := whatsapp.NewAccountManager(DB_PATH, logger)
	if err != nil {
		logger.Errorf("Failed to create account manager: %v", err)
		return
	}
	defer accountManager.Close()

	if err := accountManager.LoadBots(); err != nil {
		logger.Errorf("Failed to load existing bots: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go handleCommands(accountManager, logger)

	select {
	case <-c:
		logger.Infof("Interrupt received, shutting down...")
		accountManager.DisconnectAll()
	case <-ctx.Done():
		logger.Errorf("Global timeout reached")
		accountManager.DisconnectAll()
	}
}

func handleCommands(am *whatsapp.AccountManager, logger waLog.Logger) {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Println("\nAvailable commands:")
		fmt.Println("1. new - Create new bot instance")
		fmt.Println("2. list - List all active bots")
		fmt.Println("3. remove <bot_id> - Remove a bot instance")
		fmt.Println("4. quit - Exit the application")
		fmt.Print("\nEnter command: ")

		command, _ := reader.ReadString('\n')
		command = strings.TrimSpace(command)
		args := strings.Fields(command)

		if len(args) == 0 {
			continue
		}

		switch args[0] {
		case "new":
			logger.Infof("Creating new bot instance...")
			bot, err := am.CreateNewBot()
			if err != nil {
				logger.Errorf("Error creating new bot: %v", err)
				continue
			}

			logger.Infof("Connecting bot %s...", bot.GetID())
			go func() {
				maxRetries := 5
				for i := 0; i < maxRetries; i++ {
					if err := bot.Connect(); err != nil {
						logger.Errorf("Error connecting bot (attempt %d/%d): %v", i+1, maxRetries, err)
						if i < maxRetries-1 {
							time.Sleep(10 * time.Second)
							continue
						}
					} else {
						return // Connected successfully
					}
				}
			}()

			logger.Infof("New bot instance created. Wait for QR code to appear...")

		case "list":
			bots := am.ListBots()
			if len(bots) == 0 {
				logger.Infof("No active bots")
				continue
			}

			logger.Infof("Active bots:")
			for id, bot := range bots {
				connected := bot.IsConnected()
				status := "disconnected"
				if connected {
					status = "connected"
				}
				logger.Infof("- %s: %s", id, status)
			}

		case "remove":
			if len(args) < 2 {
				logger.Warnf("Please specify bot ID")
				continue
			}

			if err := am.RemoveBot(args[1]); err != nil {
				logger.Errorf("Error removing bot: %v", err)
			} else {
				logger.Infof("Bot %s removed successfully", args[1])
			}

		case "quit":
			logger.Infof("Shutting down...")
			am.DisconnectAll()
			os.Exit(0)

		default:
			logger.Warnf("Unknown command: %s", args[0])
		}
	}
}

func setupLogging() (*os.File, error) {
	logPath := filepath.Join("logs", LOG_FILE)
	if err := os.MkdirAll("logs", 0755); err != nil {
		return nil, err
	}
	return os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}

func startExternalServices(logger waLog.Logger) {
	// Start Whisper STT server
	whisperCmd := cleanEnvValue(os.Getenv("WHISPER_CMD"))
	whisperDir := cleanEnvValue(os.Getenv("WHISPER_DIR"))
	if whisperCmd != "" {
		logger.Infof("Starting Whisper server: %s in %s", whisperCmd, whisperDir)
		go runExternalCommand(whisperCmd, whisperDir, "whisper", logger)
	}

	// Start TTS server
	ttsCmd := cleanEnvValue(os.Getenv("TTS_CMD"))
	ttsDir := cleanEnvValue(os.Getenv("TTS_DIR"))
	if ttsCmd != "" {
		logger.Infof("Starting TTS server: %s in %s", ttsCmd, ttsDir)
		go runExternalCommand(ttsCmd, ttsDir, "tts", logger)
	}
}

// cleanEnvValue fixes common Windows path mangling caused by .env double-quote escaping.
func cleanEnvValue(v string) string {
	if v == "" {
		return ""
	}
	// If the parser turned \n or \t into literal control characters, 
	// we turn them back into / to ensure the path remains valid for Go's exec.
	v = strings.ReplaceAll(v, "\n", "/")
	v = strings.ReplaceAll(v, "\r", "/")
	v = strings.ReplaceAll(v, "\t", "/")
	return v
}

func runExternalCommand(cmdStr string, dir string, name string, logger waLog.Logger) {
	for {
		args := strings.Fields(cmdStr)
		if len(args) == 0 {
			return
		}

		cmd := exec.Command(args[0], args[1:]...)
		if dir != "" {
			cmd.Dir = dir
		}

		// Redirect output to a log file
		logPath := filepath.Join("logs", name+".log")
		logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			logger.Errorf("Failed to create log file for %s: %v", name, err)
		} else {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
			// Note: We don't defer Close here because it's in a loop
		}

		if err := cmd.Start(); err != nil {
			logger.Errorf("%s server failed to start: %v", name, err)
			logFile.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		logger.Infof("%s server started with PID %d", name, cmd.Process.Pid)

		if err := cmd.Wait(); err != nil {
			logger.Errorf("%s server exited with error: %v. Restarting in 5s...", name, err)
		} else {
			logger.Infof("%s server exited normally. Restarting in 5s...", name)
		}

		logFile.Close()
		time.Sleep(5 * time.Second)
	}
}

