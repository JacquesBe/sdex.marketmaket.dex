package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jacquesbecker/sdex-marketmaker/balance"
	"github.com/jacquesbecker/sdex-marketmaker/config"
	"github.com/jacquesbecker/sdex-marketmaker/offers"
	"github.com/jacquesbecker/sdex-marketmaker/pricefeed"
	"github.com/jacquesbecker/sdex-marketmaker/strategy"
	"github.com/joho/godotenv"
	"github.com/stellar/go/network"
)

func main() {
	log.Println("Starting SDEX Market Maker Bot...")

	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system environment variables")
	}

	// Load environment variables
	mongoURI := getEnv("MONGO_URI", "mongodb://localhost:27017")
	mongoDatabase := getEnv("MONGO_DATABASE", "sdex_bot")
	configID := getEnv("CONFIG_ID", "")
	horizonBaseURI := getEnv("HORIZON_BASE_URI", "https://horizon.stellar.org")

	if configID == "" {
		log.Fatal("CONFIG_ID environment variable is required")
	}

	// Initialize MongoDB service
	mongoService, err := config.NewMongoService(mongoURI, mongoDatabase)
	if err != nil {
		log.Fatalf("Failed to initialize MongoDB service: %v", err)
	}
	defer mongoService.Close()

	// Load bot configuration from MongoDB
	if err := mongoService.LoadConfig(configID); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	botConfig := mongoService.GetConfig()
	log.Printf("Bot initialized for trading pair: %s/%s (DEX) <- %s/%s (CEX)",
		botConfig.BaseAsset,
		botConfig.CounterAsset,
		botConfig.PriceFeedOptions.BaseAsset,
		botConfig.PriceFeedOptions.CounterAsset,
	)

	// Determine network passphrase from Horizon URL
	var networkPassphrase string
	if horizonBaseURI == "https://horizon-testnet.stellar.org" {
		networkPassphrase = network.TestNetworkPassphrase
		log.Println("Using TESTNET network")
	} else {
		networkPassphrase = network.PublicNetworkPassphrase
		log.Println("Using MAINNET (public) network")
	}

	// WaitGroup to track running services
	var wg sync.WaitGroup

	// Prompt user to select price feed source
	priceFeedSource := promptPriceFeedSource()
	log.Printf("Selected price feed source: %s", priceFeedSource)

	// Initialize offer monitor (needed before Horizon feed)
	offerMonitor := offers.NewMonitor(botConfig, horizonBaseURI)

	// Initialize the selected price feed
	var feed pricefeed.PriceFeed
	if priceFeedSource == "stellar" {
		feed = pricefeed.NewHorizonFeed(botConfig, horizonBaseURI, offerMonitor)
		log.Println("Using Stellar/Horizon price feed")
	} else {
		feed = pricefeed.NewBinanceFeed(botConfig)
		log.Println("Using Binance price feed")
	}

	// Run price feed in a goroutine (background service)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := feed.Start(); err != nil {
			log.Printf("❌ [PRICE FEED] FATAL ERROR: %v", err)
			log.Println("[SHUTDOWN] Price feed encountered fatal error, shutting down bot...")
			// Send shutdown signal
			syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		}
	}()

	// Initialize strategy engine
	engine := strategy.NewStrategyEngine(feed, botConfig)
	log.Printf("Strategy engine initialized (volWindow=%dms, blendWeight=%.2f, riskAversion=%.3f)",
		botConfig.StrategyOptions.VolWindowMs,
		botConfig.StrategyOptions.BlendWeight,
		botConfig.StrategyOptions.RiskAversion)

	// Initialize offer manager (use existing offerMonitor from above)
	offerManager, err := offers.NewManager(botConfig, horizonBaseURI, networkPassphrase, offerMonitor, engine)
	if err != nil {
		log.Fatalf("Failed to initialize offer manager: %v", err)
	}

	// Cancel all existing offers on startup
	log.Println("[STARTUP] Cancelling all existing offers...")
	if err := offerManager.CancelAllOffers(); err != nil {
		log.Printf("[STARTUP] Warning: Failed to cancel offers: %v", err)
	}

	// Initialize and start the Balance Monitor
	balanceMonitor := balance.NewMonitor(botConfig, horizonBaseURI)
	
	// Track startup time for 30-second warmup
	startupTime := time.Now()
	
	// Set callback: Pricefeed invokes Strategy
	feed.SetFeatureUpdateCallback(func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC] Strategy callback panicked: %v", r)
			}
		}()
		pBid, pAsk, ok := engine.ComputeQuotes()
		if ok {
			log.Printf("💰 [QUOTES] BID: %.6f | ASK: %.6f | Spread: %.6f (%.1fbps)",
				pBid, pAsk, pAsk-pBid, ((pAsk-pBid)/((pBid+pAsk)/2))*10000)
		}
	})
	
	// Set disconnect callback: Clear strategy prices when feed dies
	feed.SetDisconnectCallback(func() {
		engine.ClearPrices()
	})
	
	// Set callback: OfferMonitor invokes OfferManager
	offerMonitor.SetOfferUpdateCallback(func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC] OfferManager callback panicked: %v", r)
			}
		}()
		// Check if warmup period has passed (30 seconds)
		elapsed := time.Since(startupTime).Seconds()
		if elapsed < 30 {
			log.Printf("[OFFER MANAGER] Warmup period: %.0fs elapsed, waiting for 30s before executing offers...", elapsed)
			return
		}
		
		// Execute offers (reads prices from strategy internally)
		if err := offerManager.ExecuteOffers(); err != nil {
			log.Printf("[OFFER MANAGER] Error executing offers: %v", err)
		}
	})
	
	// Run balance monitor in a goroutine (background service)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := balanceMonitor.Start(); err != nil {
			log.Printf("Balance monitor error: %v", err)
		}
	}()

	// Run offer monitor in a goroutine (background service)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := offerMonitor.Start(); err != nil {
			log.Printf("Offer monitor error: %v", err)
		}
	}()

	// Price feed watchdog - only for Binance (Horizon can have long quiet periods)
	if priceFeedSource == "binance" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Println("[WATCHDOG] Price feed watchdog started (60s timeout) - Binance only")
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					lastMsg := feed.GetLastMessageAt()
					if lastMsg == 0 {
						// Still initializing
						continue
					}
					elapsed := time.Now().UnixMilli() - lastMsg
					if elapsed > 60000 { // 60 seconds
						log.Printf("\n\u274c [WATCHDOG] CRITICAL: Price feed died! Last message %dms ago. Shutting down...", elapsed)
						// Send shutdown signal
						syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
						return
					}
				}
			}
		}()
	} else {
		log.Println("[WATCHDOG] Watchdog disabled for Horizon feed (quiet markets are normal)")
	}

	// Wait for interrupt signal to gracefully shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("\n[SHUTDOWN] Received shutdown signal, initiating graceful shutdown...")

	// Stop services FIRST (so they don't interfere with offer cancellation)
	log.Println("[SHUTDOWN] Stopping price feed...")
	feed.Stop()

	log.Println("[SHUTDOWN] Stopping balance monitor...")
	balanceMonitor.Stop()

	log.Println("[SHUTDOWN] Stopping offer monitor...")
	offerMonitor.Stop()

	// Wait for all goroutines to finish with timeout
	log.Println("[SHUTDOWN] Waiting for services to stop...")
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// Wait for services to stop or timeout after 5 seconds
	select {
	case <-done:
		log.Println("[SHUTDOWN] All services stopped gracefully")
	case <-time.After(5 * time.Second):
		log.Println("[SHUTDOWN] Timeout waiting for services to stop, continuing...")
	}

	// NOW cancel all offers (after services are stopped)
	log.Println("[SHUTDOWN] Cancelling all offers...")
	if err := offerManager.CancelAllOffers(); err != nil {
		log.Printf("[SHUTDOWN] Warning: Failed to cancel offers: %v", err)
	} else {
		log.Println("[SHUTDOWN] All offers cancelled successfully")
	}

	log.Println("[SHUTDOWN] Closing MongoDB connection...")
	// MongoDB will be closed by defer

	log.Println("[SHUTDOWN] Shutdown complete")
}

// getEnv retrieves an environment variable with a fallback default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// promptPriceFeedSource prompts the user to select a price feed source
func promptPriceFeedSource() string {
	reader := bufio.NewReader(os.Stdin)
	
	for {
		fmt.Println("\n=== Price Feed Selection ===")
		fmt.Println("Which price feed source would you like to use?")
		fmt.Println("  1. stellar  - Use Stellar/Horizon order book (native DEX prices)")
		fmt.Println("  2. binance  - Use Binance CEX order book (external prices)")
		fmt.Print("\nEnter your choice (stellar/binance): ")
		
		input, err := reader.ReadString('\n')
		if err != nil {
			log.Printf("Error reading input: %v", err)
			continue
		}
		
		// Trim whitespace and convert to lowercase
		choice := strings.TrimSpace(strings.ToLower(input))
		
		if choice == "stellar" || choice == "1" {
			return "stellar"
		} else if choice == "binance" || choice == "2" {
			return "binance"
		} else {
			fmt.Printf("Invalid choice '%s'. Please enter 'stellar' or 'binance'.\n", choice)
		}
	}
}
