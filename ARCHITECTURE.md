# ARCHITECTURE.md

## Overview
This is a high-performance market maker bot for the Stellar Decentralized Exchange (SDEX). The system is designed with efficiency and configurability as primary concerns, enabling multiple bot instances to run with different trading pair configurations.

## Core Design Principles

1. **Efficiency First**: Minimized latency, optimized data flow, and asynchronous operations throughout
2. **Event-Driven Architecture**: Components communicate through a reactive pipeline (Pricefeed → Strategy → Offer Manager)
3. **Configuration-Driven**: All trading pair specifics stored in MongoDB, allowing multi-bot deployments
4. **Separation of Concerns**: Clear boundaries between data ingestion, decision-making, and execution

## System Components

### 1. Pricefeed Layer (Data Ingestion)
**Purpose**: Real-time market data acquisition from centralized exchanges

- **Technology**: WebSocket connections to Binance API
- **Operation Mode**: Background service running continuously
- **Responsibilities**:
  - Establish and maintain WebSocket connections
  - Stream real-time price data (orderbook, trades, ticker)
  - Normalize data into internal format
  - Invoke Strategy layer with updated market data
- **Key Considerations**:
  - Automatic reconnection handling
  - Rate limiting awareness
  - Minimal processing overhead
  - Support for multiple trading pairs simultaneously

### 2. Strategy Layer (Decision Engine)
**Purpose**: Analyze market data and determine trading actions

- **Triggered By**: Pricefeed layer with new market data
- **Responsibilities**:
  - Implement market making algorithms (spread, inventory management, etc.)
  - Calculate optimal bid/ask prices and sizes
  - Determine when to create, update, or cancel offers
  - Risk management and position limits
  - Invoke Offer Manager with trading decisions
- **Key Considerations**:
  - Stateful (tracks current position, open offers)
  - Configurable parameters per bot instance
  - Fast execution to minimize latency between price updates and order actions

### 3. Offer Manager Layer (Execution)
**Purpose**: Interface with Stellar DEX for order management

- **Triggered By**: Strategy layer with trading decisions
- **Responsibilities**:
  - Create new offers on SDEX
  - Update existing offers (price/volume changes)
  - Delete stale or unwanted offers
  - Track offer lifecycle and status
  - Handle Stellar transaction submission and monitoring
- **Key Considerations**:
  - Transaction building and signing
  - Sequence number management
  - Fee optimization
  - Error handling and retries
  - Offer state synchronization with SDEX

### 4. Configuration Store (MongoDB)
**Purpose**: Centralized configuration management

- **Structure**: Collection of bot configuration documents
- **Per-Bot Document Contains**:
  - Trading pair information (base/quote assets on both CEX and DEX)
  - Asset codes and issuers for Stellar
  - Strategy parameters (spread, order sizes, refresh intervals)
  - Risk limits (max position, max order size)
  - Bot-specific settings
- **Key Considerations**:
  - Hot-reload capability for configuration changes
  - Version tracking for configuration updates
  - Multiple bot instances reading different documents

### 5. Environment Configuration
**Purpose**: Secure credential and environment-specific settings

- **Stored As**: Environment variables
- **Contains**:
  - Binance API keys and secrets
  - Stellar account secrets (keypairs)
  - MongoDB connection strings
  - Stellar Horizon server URLs
  - Log levels and monitoring endpoints

## Data Flow

```
[Binance WebSocket] 
    ↓ (real-time price data)
[Pricefeed Layer]
    ↓ (normalized market data)
[Strategy Layer] ← [MongoDB Config]
    ↓ (trading decisions)
[Offer Manager Layer]
    ↓ (transactions)
[Stellar DEX]
```

## Technology Stack (Recommended)

- **Language**: Node.js/TypeScript (async I/O optimized) or Python (asyncio)
- **WebSocket Client**: Native WebSocket libraries with reconnection logic
- **Stellar SDK**: stellar-sdk (JS) or stellar-sdk (Python)
- **Database**: MongoDB with native driver
- **Process Management**: PM2 or systemd for production deployment

## Deployment Model

### Single Bot Instance
- One process running all three layers
- Connected to one MongoDB configuration document
- Trades one pair (or multiple pairs if configured)

### Multi-Bot Instance
- Multiple processes, each with unique configuration document
- Shared MongoDB instance
- Isolated Stellar accounts (different keypairs)
- Independent CEX data streams (can be optimized later)

## Scalability Considerations

1. **Phase 1 (Current)**: Monolithic process with three layers
2. **Phase 2 (Future)**: Microservices - separate Pricefeed service feeding multiple Strategy/OfferManager instances
3. **Phase 3 (Future)**: Distributed system with message queue (Redis/RabbitMQ) between layers

## Configuration Schema (MongoDB Document Example)

```json
{
  "bot_id": "bot_001",
  "enabled": true,
  "cex_config": {
    "exchange": "binance",
    "symbol": "BTCUSDT"
  },
  "dex_config": {
    "base_asset": {
      "code": "BTC",
      "issuer": "GXXXXXX..."
    },
    "quote_asset": {
      "code": "USDC",
      "issuer": "GXXXXXX..."
    }
  },
  "strategy_config": {
    "type": "simple_spread",
    "spread_bps": 20,
    "order_size": 0.01,
    "max_position": 0.5,
    "refresh_interval_ms": 1000
  }
}
```

## Error Handling Strategy

- **Pricefeed**: Auto-reconnect on disconnect, exponential backoff
- **Strategy**: Log errors, continue on next price update
- **Offer Manager**: Transaction retry logic, sequence number recovery
- **Global**: Graceful shutdown handling, state persistence

## Monitoring & Observability

- Structured logging (JSON format)
- Metrics collection (latency, order fill rates, PnL)
- Health check endpoints
- Alert on critical failures (connection loss, transaction failures)

## Next Steps

1. Implement Pricefeed layer with Binance WebSocket integration
2. Define internal data structures for market data
3. Build Strategy layer framework with pluggable strategy modules
4. Implement Offer Manager with Stellar SDK integration
5. Create MongoDB schema and connection handling
6. Develop configuration loader and hot-reload mechanism
