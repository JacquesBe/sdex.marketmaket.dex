package main

import (
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jacquesbecker/sdex-marketmaker/balance"
	"github.com/jacquesbecker/sdex-marketmaker/config"
	"github.com/jacquesbecker/sdex-marketmaker/pricefeed"
	"github.com/jacquesbecker/sdex-marketmaker/strategy"
	"github.com/joho/godotenv"
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

	// WaitGroup to track running services
	var wg sync.WaitGroup

	// Initialize Binance price feed
	feed := pricefeed.NewBinanceFeed(botConfig)

	// Run price feed in a goroutine (background service)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := feed.Start(); err != nil {
			log.Printf("Price feed error: %v", err)
		}
	}()

	// Initialize strategy engine
	engine := strategy.NewStrategyEngine(feed, botConfig)
	log.Printf("Strategy engine initialized (volWindow=%dms, blendWeight=%.2f, riskAversion=%.3f)",
		botConfig.StrategyOptions.VolWindowMs,
		botConfig.StrategyOptions.BlendWeight,
		botConfig.StrategyOptions.RiskAversion)

	// Initialize and start the Balance Monitor
	balanceMonitor := balance.NewMonitor(botConfig, horizonBaseURI)
	
	// Set callback to invoke strategy when balances are updated
balanceMonitor.SetBalanceUpdateCallback(func() {
		log.Printf("[STRATEGY] Invoking ComputeQuotes...")
		pBid, pAsk, ok := engine.ComputeQuotes()
		log.Printf("[STRATEGY] ComputeQuotes ok=%v", ok)
		if ok {
			log.Printf("\n💰 [QUOTES] BID: %.6f | ASK: %.6f | Spread: %.6f (%.1fbps)\n",
				pBid, pAsk, pAsk-pBid, ((pAsk-pBid)/((pBid+pAsk)/2))*10000)
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

	// Wait for interrupt signal to gracefully shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("\n[SHUTDOWN] Received shutdown signal, initiating graceful shutdown...")

	// Stop services
	log.Println("[SHUTDOWN] Stopping price feed...")
	feed.Stop()

	log.Println("[SHUTDOWN] Stopping balance monitor...")
	balanceMonitor.Stop()

	// Wait for all goroutines to finish with timeout
	log.Println("[SHUTDOWN] Waiting for services to stop...")
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// Wait for services to stop or timeout after 10 seconds
	select {
	case <-done:
		log.Println("[SHUTDOWN] All services stopped gracefully")
	case <-time.After(10 * time.Second):
		log.Println("[SHUTDOWN] Timeout waiting for services to stop, forcing shutdown")
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
