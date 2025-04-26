package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"whatsapp-gpt-bot/utils"
	"whatsapp-gpt-bot/whatsapp"

	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

const (
	LOG_FILE = "whatsapp-bot.log"
	DB_PATH  = "file:whatsapp.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&cache=shared&mode=rwc"
)

func main() {
	fmt.Println("Starting WhatsApp bot manager...")

	utils.InitDashboard()
	fmt.Println("Performance dashboard initialized...")

	logFile, err := setupLogging()
	if err != nil {
		fmt.Printf("Failed to set up logging: %v\n", err)
		return
	}
	defer logFile.Close()

	logger := waLog.Stdout("Bot", "DEBUG", true)
	fmt.Println("Logger initialized...")

	accountManager, err := whatsapp.NewAccountManager(DB_PATH, logger)
	if err != nil {
		logger.Errorf("Failed to create account manager: %v", err)
		return
	}
	defer accountManager.Close()

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
			bot, err := am.CreateNewBot()
			if err != nil {
				fmt.Printf("Error creating new bot: %v\n", err)
				continue
			}

			if err := bot.Connect(); err != nil {
				fmt.Printf("Error connecting bot: %v\n", err)
				continue
			}

			fmt.Println("New bot instance created. Scan the QR code to connect...")

		case "list":
			bots := am.ListBots()
			if len(bots) == 0 {
				fmt.Println("No active bots")
				continue
			}

			fmt.Println("\nActive bots:")
			for id, bot := range bots {
				connected := bot.IsConnected()
				status := "disconnected"
				if connected {
					status = "connected"
				}
				fmt.Printf("- %s: %s\n", id, status)
			}

		case "remove":
			if len(args) < 2 {
				fmt.Println("Please specify bot ID")
				continue
			}

			if err := am.RemoveBot(args[1]); err != nil {
				fmt.Printf("Error removing bot: %v\n", err)
			} else {
				fmt.Printf("Bot %s removed successfully\n", args[1])
			}

		case "quit":
			fmt.Println("Shutting down...")
			am.DisconnectAll()
			os.Exit(0)

		default:
			fmt.Println("Unknown command")
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
