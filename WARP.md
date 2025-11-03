# WARP.md

This file provides guidance to WARP (warp.dev) when working with code in this repository.

## Project Overview

This is a high-performance market maker bot for the Stellar Decentralized Exchange (SDEX). It connects to Binance for price feeds and places automated bid/ask offers on Stellar DEX based on a sophisticated market making strategy.

**Architecture**: Event-driven system with three main layers (Pricefeed → Strategy → Offer Manager) coordinated through a reactive pipeline.

**Language**: Go 1.24

## Common Commands

### Build and Run
```bash
# Build the binary
go build -o sdex-marketmaker

# Run the bot (requires .env configuration)
./sdex-marketmaker

# Build and run in one step
go build -o sdex-marketmaker && ./sdex-marketmaker
```

### Development
```bash
# Run tests (if they exist)
go test ./...

# Format code
go fmt ./...

# Vet code for issues
go vet ./...

# Tidy dependencies
go mod tidy

# Download dependencies
go mod download
```

### Configuration
- Copy `.env.example` to `.env` and configure:
  - `CONFIG_ID`: MongoDB document ID for bot configuration
  - `MONGO_URI`: MongoDB connection string
  - `MONGO_DATABASE`: MongoDB database name
  - `HORIZON_BASE_URI`: Stellar Horizon API URL (testnet or mainnet)
  - Stellar secret key is stored in MongoDB, not environment

## Architecture Overview

### Core Components

1. **Pricefeed Layer** (supports multiple sources)
   - **Binance Feed** (`pricefeed/binance_feed.go`):
     - Connects to Binance WebSocket for real-time order book data
     - Updates every ~100ms via WebSocket
   - **Horizon Feed** (`pricefeed/horizon_feed.go`):
     - Streams Stellar DEX order book via Horizon SSE
     - Automatically filters out bot's own offers from order book
     - Uses offer monitor to identify and remove own orders
   - Both feeds compute the same market features: external mid price, microprice, rolling volatility, order book imbalance (TOB)
   - User selects feed source at startup ("stellar" or "binance")

2. **Strategy Engine** (`strategy/strategy.go`)
   - Computes bid and ask quotes using sophisticated market making formulas
   - Key inputs:
     - Fair price (blend of external mid price and microprice)
     - Inventory position (from balance monitor)
     - Rolling volatility
     - Order book imbalances
   - Produces asymmetric spreads based on inventory risk
   - See `PRICE.MD` for complete formula documentation

3. **Offer Manager** (`offers/offer_manager.go`)
   - Executes bid/ask offers on Stellar DEX via Horizon API
   - Evaluates existing offers using grace periods (price and quantity tolerances)
   - Only updates offers when outside tolerance bands to minimize fees
   - Handles transaction submission, retries, and error recovery
   - Independent bid/ask evaluation to avoid unnecessary updates

4. **Offer Monitor** (`offers/offer_monitor.go`)
   - Polls Horizon API to track current offers
   - Detects when offers are filled or cancelled
   - Maintains hashmap of active offers for filtering and state tracking

5. **Balance Monitor** (`balance/monitor.go`)
   - Polls Horizon API for account balances (base and counter assets)
   - Triggers strategy recalculation on balance updates
   - Critical for inventory management

6. **Config Store** (`config/`)
   - MongoDB-based configuration system
   - Stores all bot parameters: trading pairs, strategy parameters, risk limits
   - Allows multi-bot deployments with different configurations
   - See `config/model.go` for full schema

### Data Flow

```
Binance WebSocket OR Horizon SSE (order book)
    ↓
Pricefeed Layer (features computation, filters own offers if Horizon)
    ↓
Strategy Engine (quote calculation) ← Balance Monitor (inventory)
    ↓
Offer Manager (trade execution) ← Offer Monitor (current state)
    ↓
Stellar DEX
```

### Key Design Patterns

- **Interface Abstraction**: PriceFeed interface allows multiple feed implementations (Binance, Horizon)
- **Singleton Pattern**: Balance monitor and offer monitor use global singletons
- **Callback Pattern**: Balance monitor invokes strategy via callback when balances update
- **Grace Periods**: Offer manager uses tolerance bands to prevent excessive updates
- **Retry Logic**: Handles `op_offer_not_found` errors with bounded retries (max 3)
- **Warmup Period**: 30-second startup delay before executing offers
- **Watchdog**: Price feed monitor that shuts down bot if feed dies (30s timeout)
- **Self-Offer Filtering**: Horizon feed automatically removes bot's own offers from order book data

## Important Files

- **ARCHITECTURE.md**: High-level system design and component descriptions
- **PRICE.MD**: Complete mathematical documentation of pricing formulas with source locations
- **SAFETY_CHECKLIST.md**: Production deployment checklist and risk mitigation strategies
- **main.go**: Application entry point with service orchestration
- **.env.example**: Configuration template (copy to `.env`)

## Configuration Structure (MongoDB)

Configuration is stored in MongoDB and loaded at startup via `CONFIG_ID` environment variable:

```json
{
  "_id": "...",
  "publicKey": "G...",
  "secretKey": "S...",
  "baseAsset": "XLM",
  "counterAsset": "EUR",
  "baseAssetIssuer": "...",
  "counterAssetIssuer": "...",
  "priceFeedOptions": {
    "baseAsset": "XLM",
    "counterAsset": "EUR",
    "inverse": false,
    "priceFeedMultiple": 1.0,
    "bufferLength": 1000,
    "bookDepth": 5
  },
  "balanceMonitorOptions": {
    "tickRate": 1500
  },
  "strategyOptions": {
    "volWindowMs": 10000,
    "blendWeight": 0.3,
    "riskAversion": 0.001,
    "halfSpreadFloor": 5.0,
    "volatilitySensitivity": 100.0,
    "inventoryBias": 0.05,
    "obImbalanceSensitivity": 0.01
  },
  "offerManagerOptions": {
    "offerMonitorTickRate": 1500,
    "offerQuantity": 2.0,
    "priceGraceBasisPoints": 5.0,
    "offerGraceQuantity": 1.0,
    "feeMaxStroops": 100
  }
}
```

## Safety Considerations

1. **Always test on Stellar testnet first** (use `https://horizon-testnet.stellar.org`)
2. **Start with small `offerQuantity` values** (e.g., 2 XLM)
3. **Monitor logs closely** during initial runs
4. **Price validation**: Code checks `bidPrice > 0`, `askPrice > 0`, `bidPrice < askPrice`
5. **Graceful shutdown**: Ctrl+C cancels all offers before stopping
6. **Watchdog protection**: Bot shuts down if price feed dies (30s timeout)
7. **Never commit changes to production unless explicitly requested**

## Working with This Codebase

### Adding New Features

- **Price feed features**: Modify both `pricefeed/binance_feed.go` and `pricefeed/horizon_feed.go`, update `FeaturesRow` struct
- **New price feed sources**: Implement `pricefeed.PriceFeed` interface, add to main.go prompt
- **Strategy changes**: Edit `strategy/strategy.go` pricing formulas (document in `PRICE.MD`)
- **Offer execution logic**: Modify `offers/offer_manager.go`
- **Configuration parameters**: Update `config/model.go` and MongoDB schema

### Common Issues

- **"No features available yet"**: Price feed still initializing, wait ~10 seconds
- **"Rolling volatility not yet available"**: Need at least 2 data points in window
- **`op_underfunded`**: Insufficient balance for offer quantity
- **`op_offer_not_found`**: Offer filled/cancelled, retry logic handles this
- **Price feed died**: Watchdog will shutdown bot automatically
- **"Empty order book after filtering"** (Horizon): All orders in book are bot's own offers
- **Horizon SSE connection issues**: Check Horizon URL, network connectivity, or use Binance feed instead

### Testing Strategy

1. Test configuration changes on testnet
2. Monitor logs for 24-48 hours before scaling
3. Use small quantities initially (1-5 XLM)
4. Use wider grace periods initially (10-20 bps)
5. Verify bid < ask always holds
6. Check that offers aren't updating every tick (grace periods working)

### Deployment

- Single binary deployment (`sdex-marketmaker`)
- Requires MongoDB connection
- Requires Horizon API access (Stellar network)
- Requires Binance WebSocket access (no API key needed for public endpoints)
- Environment variables via `.env` file
- Network selection via `HORIZON_BASE_URI` (testnet vs mainnet)

## Coding Conventions

- **Logging**: Use `log.Printf` with prefixes like `[STRATEGY]`, `[OFFER MANAGER]`, etc.
- **Error handling**: Return errors up the stack, log at top level
- **Goroutines**: Services run in separate goroutines with WaitGroup coordination
- **Shutdown**: Services implement `Stop()` method and check stop signals
- **Float precision**: Use `%.6f` for prices, `%.7f` for Stellar amounts (7 decimal max)
- **Config access**: All parameters come from `BotConfig` struct loaded from MongoDB

## External Dependencies

- **Binance WebSocket**: Real-time order book data (public, no auth) - optional if using Horizon feed
- **Stellar Horizon API**: Account balances, offer state, transaction submission, order book SSE (for Horizon feed)
- **MongoDB**: Configuration storage (required)
- **Stellar Go SDK**: Transaction building, signing, and submission
- **gorilla/websocket**: WebSocket client library (for Binance feed)

## Monitoring

Key log patterns to watch:
- `✅ offer submitted successfully` - Successful offer placement
- `All offers OK - no update needed` - Grace periods preventing updates (good)
- `BID needs update` / `ASK needs update` - Offer being updated (reason shown)
- `[WATCHDOG] CRITICAL` - Price feed died, bot shutting down
- `[SHUTDOWN]` - Graceful shutdown sequence
- `❌ [STRATEGY ERROR]` - Invalid prices generated (investigate)

## Development Workflow

1. Make configuration changes in MongoDB (for parameter tuning)
2. Make code changes in relevant package
3. Build: `go build -o sdex-marketmaker`
4. Run and select price feed (stellar or binance) when prompted
5. Test on testnet with small quantities
6. Monitor logs for errors/warnings
7. Verify expected behavior (check Stellar Laboratory for offers)
8. Increase scale gradually once stable

### Price Feed Selection

When starting the bot, you'll be prompted to choose:
- **stellar**: Uses Stellar/Horizon order book (native DEX prices, filters own offers)
- **binance**: Uses Binance CEX order book (external reference prices)

For testing or when external prices aren't available, use "stellar" to trade based on the DEX's own order book.
