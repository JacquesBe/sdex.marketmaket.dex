package balance

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jacquesbecker/sdex-marketmaker/config"
)

// HorizonBalance represents a balance entry from Horizon API
type HorizonBalance struct {
	Balance             string `json:"balance"`
	LiquidityPoolID     string `json:"liquidity_pool_id,omitempty"`
	Limit               string `json:"limit,omitempty"`
	BuyingLiabilities   string `json:"buying_liabilities"`
	SellingLiabilities  string `json:"selling_liabilities"`
	AssetType           string `json:"asset_type"`
	AssetCode           string `json:"asset_code,omitempty"`
	AssetIssuer         string `json:"asset_issuer,omitempty"`
}

// HorizonAccountResponse represents the account data from Horizon API
type HorizonAccountResponse struct {
	ID       string           `json:"id"`
	Sequence string           `json:"sequence"`
	Balances []HorizonBalance `json:"balances"`
}

// AssetBalance represents the balance for a specific asset
type AssetBalance struct {
	Balance            string
	BuyingLiabilities  string
	SellingLiabilities string
	Limit              string
}

// Monitor manages the balance monitoring service
type Monitor struct {
	config         *config.BotConfig
	horizonBaseURI string
	httpClient     *http.Client
	stopChan       chan bool
	mu             sync.RWMutex
	baseBalance    *AssetBalance
	counterBalance *AssetBalance
	lastUpdateAt   int64 // unix ms of last successful balance fetch
	logCounter     int
}

var (
	instance *Monitor
	once     sync.Once
)

// NewMonitor creates a new balance monitor (singleton)
func NewMonitor(botConfig *config.BotConfig, horizonBaseURI string) *Monitor {
	once.Do(func() {
		instance = &Monitor{
			config:         botConfig,
			horizonBaseURI: horizonBaseURI,
			httpClient: &http.Client{
				Timeout: 10 * time.Second,
			},
			stopChan: make(chan bool),
		}
	})
	return instance
}

// GetInstance returns the singleton instance of the balance monitor
func GetInstance() *Monitor {
	return instance
}

// Start begins the balance monitoring service
func (m *Monitor) Start() error {
	tickRate := time.Duration(m.config.BalanceMonitorOptions.TickRate) * time.Millisecond
	
	log.Printf("Starting Balance Monitor for account: %s", m.config.PublicKey)
	log.Printf("Tick Rate: %dms | Horizon: %s", m.config.BalanceMonitorOptions.TickRate, m.horizonBaseURI)

	ticker := time.NewTicker(tickRate)
	defer ticker.Stop()

	// Fetch balances immediately on start (do not exit on error)
	if err := m.fetchBalances(); err != nil {
		log.Printf("Initial balance fetch error: %v", err)
	}

	for {
		select {
		case t := <-ticker.C:
			log.Printf("[BALANCE TICK] %s", t.Format(time.RFC3339))
			if err := m.fetchBalances(); err != nil {
				log.Printf("Balance fetch error: %v", err)
			}
		case <-m.stopChan:
			log.Println("Balance monitor stopped")
			return nil
		}
	}
}

// fetchBalances retrieves the account balances from Horizon
func (m *Monitor) fetchBalances() error {
	url := fmt.Sprintf("%s/accounts/%s", m.horizonBaseURI, m.config.PublicKey)
	
	resp, err := m.httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("failed to fetch account data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("horizon API returned status %d: %s", resp.StatusCode, string(body))
	}

	var accountData HorizonAccountResponse
	if err := json.NewDecoder(resp.Body).Decode(&accountData); err != nil {
		return fmt.Errorf("failed to decode account data: %w", err)
	}

	// Update cached balances
	m.updateBalances(&accountData)

	// Update last successful time
	m.lastUpdateAt = time.Now().UnixMilli()

	// Log the balances
	m.logBalances(&accountData)
	
	return nil
}

// updateBalances updates the cached balances for base and counter assets
func (m *Monitor) updateBalances(account *HorizonAccountResponse) {
	m.mu.Lock()
	for _, balance := range account.Balances {
		// Check if this is the base asset
		if m.isBaseAsset(balance) {
			m.baseBalance = &AssetBalance{
				Balance:            balance.Balance,
				BuyingLiabilities:  balance.BuyingLiabilities,
				SellingLiabilities: balance.SellingLiabilities,
				Limit:              balance.Limit,
			}
		}

		// Check if this is the counter asset
		if m.isCounterAsset(balance) {
			m.counterBalance = &AssetBalance{
				Balance:            balance.Balance,
				BuyingLiabilities:  balance.BuyingLiabilities,
				SellingLiabilities: balance.SellingLiabilities,
				Limit:              balance.Limit,
			}
		}
	}
	m.mu.Unlock()
}

// isBaseAsset checks if a balance matches the configured base asset
func (m *Monitor) isBaseAsset(balance HorizonBalance) bool {
	if m.config.BaseAssetIssuer == "native" {
		return balance.AssetType == "native"
	}
	return balance.AssetCode == m.config.BaseAsset && balance.AssetIssuer == m.config.BaseAssetIssuer
}

// isCounterAsset checks if a balance matches the configured counter asset
func (m *Monitor) isCounterAsset(balance HorizonBalance) bool {
	if m.config.CounterAssetIssuer == "native" {
		return balance.AssetType == "native"
	}
	return balance.AssetCode == m.config.CounterAsset && balance.AssetIssuer == m.config.CounterAssetIssuer
}

// GetLatestBalances returns the latest balances for base and counter assets
// Returns (baseBalance, counterBalance, error)
func (m *Monitor) GetLatestBalances() (*AssetBalance, *AssetBalance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.baseBalance == nil || m.counterBalance == nil {
		return nil, nil, fmt.Errorf("balances not yet fetched")
	}

	return m.baseBalance, m.counterBalance, nil
}

// logBalances logs the current account balances
func (m *Monitor) logBalances(account *HorizonAccountResponse) {
	// Log every fetch
	log.Printf("\n💳 [BALANCES]")
	for _, balance := range account.Balances {
		if balance.AssetType == "native" {
			log.Printf("   XLM: %s", balance.Balance)
		} else {
			log.Printf("   %s: %s", balance.AssetCode, balance.Balance)
		}
	}
}

// Stop gracefully stops the balance monitor
func (m *Monitor) Stop() {
	log.Println("Stopping Balance Monitor...")
	close(m.stopChan)
}

// GetLastUpdateAt returns the last successful balance fetch time (unix ms)
func (m *Monitor) GetLastUpdateAt() int64 {
	return m.lastUpdateAt
}
