# Kraken Price Feed Implementation

## Overview

This document describes the Kraken price feed implementation, which supports **synthetic pair pricing** (e.g., XLM/SHX) by streaming two separate USD pairs from Kraken and combining their prices. The system maintains a **separate Stellar orderbook monitor** to compute orderbook imbalance metrics from the Stellar DEX.

## Architecture

### Component Diagram

```
┌──────────────────┐     ┌──────────────────┐
│  Kraken WS       │     │  Kraken WS       │
│  XLM/USD         │     │  SHX/USD         │
└────────┬─────────┘     └────────┬─────────┘
         │                        │
         └────────┬───────────────┘
                  │
                  ▼
        ┌─────────────────────┐
        │  Kraken Feed        │
        │  Synthetic Pair     │
        │  XLM/SHX = XLM/USD  │
        │           ─────────  │
        │           SHX/USD    │
        └─────────┬───────────┘
                  │
                  │ Price + Volatility
                  │
                  ▼
        ┌─────────────────────┐
        │  Strategy Engine    │◄─── OB Imbalance (BidPenalty/AskPenalty)
        │                     │
        └─────────────────────┘
                  ▲
                  │
        ┌─────────────────────┐
        │ Stellar OB Monitor  │
        │ (Horizon Polling)   │
        │ Computes Imbalance  │
        └─────────────────────┘
```

### Key Components

1. **Kraken Feed (`pricefeed/kraken_feed.go`)**
   - Dual WebSocket connections to Kraken
   - Streams `BaseAsset/USD` (e.g., XLM/USD)
   - Streams `CounterAsset/USD` (e.g., SHX/USD)
   - Computes synthetic pair price via cross-rate formula
   - Maintains volatility computation via `FeatureBuilder`
   - Accepts orderbook imbalance injection from Stellar monitor

2. **Stellar Orderbook Monitor (`pricefeed/stellar_orderbook_monitor.go`)**
   - Independent HTTP polling of Stellar DEX orderbook
   - Filters out bot's own offers (via `offers.Monitor`)
   - Computes depth-based orderbook imbalance
   - Calculates `BidPenalty` and `AskPenalty` metrics
   - Pushes metrics to Kraken feed via callback

3. **Main Orchestration (`main.go`)**
   - New "kraken" option in price feed selection
   - Creates both Kraken feed and Stellar OB monitor when "kraken" selected
   - Wires callback from Stellar monitor → Kraken feed
   - Starts both services in separate goroutines
   - Includes Kraken in watchdog monitoring

## Mathematical Formulas

### Synthetic Pair Pricing

Given two USD pairs streaming from Kraken:
- `XLM/USD`: bid₁, ask₁
- `SHX/USD`: bid₂, ask₂

**Mid prices:**
```
midXLM = (bid₁ + ask₁) / 2
midSHX = (bid₂ + ask₂) / 2
```

**Synthetic mid price (XLM/SHX):**
```
syntheticMid = midXLM / midSHX
```

**Synthetic bid/ask (cross-rate formulas):**
```
syntheticBid = bid₁ / ask₂   (sell XLM for USD, buy SHX with USD)
syntheticAsk = ask₁ / bid₂   (buy XLM with USD, sell SHX for USD)
```

This ensures proper bid/ask ordering and accounts for crossing two markets.

### Orderbook Imbalance (from Stellar DEX)

**Depth calculation** (same as Binance/Horizon feeds):
```
DepthBidSum = Σ(qty_i / price_i) for bid levels 1..N  (converted to base asset)
DepthAskSum = Σ(qty_i) for ask levels 1..N           (already in base asset)
```

**Imbalance:**
```
depthImbalance = (DepthBidSum - DepthAskSum) / (DepthBidSum + DepthAskSum)
```
- Range: [-1, +1]
- Negative = ask side heavier (more sellers)
- Positive = bid side heavier (more buyers)

**Penalties:**
```
bidPenalty = max(0, -depthImbalance)   → positive when ask side heavier
askPenalty = max(0, depthImbalance)    → positive when bid side heavier
```

These penalties widen the spread asymmetrically based on Stellar market depth.

## Data Flow

### Price Flow (Kraken → Strategy)
1. Kraken WS receives XLM/USD spread update → stores `baseBid`, `baseAsk`
2. Kraken WS receives SHX/USD spread update → stores `counterBid`, `counterAsk`
3. `computeSyntheticPair()` triggered on each update
4. Synthetic bid/ask/mid computed via cross-rate formulas
5. `OrderbookState` built with synthetic prices
6. `FeatureBuilder.Build()` computes:
   - External mid price (synthetic mid)
   - Microprice (weighted by L1 quantities)
   - Rolling volatility (std dev of mid prices over window)
7. If Stellar OB data available, override `BidPenalty`/`AskPenalty`
8. `FeaturesRow` appended to buffer
9. `FeatureUpdateCallback` invoked → Strategy computes quotes

### Orderbook Imbalance Flow (Stellar → Kraken → Strategy)
1. Stellar OB Monitor polls Horizon API every 3s (configurable)
2. Fetches order book with `limit=BookDepth`
3. Filters out own offers (via offer monitor)
4. Computes `DepthBidSum` and `DepthAskSum`
5. Calculates `bidPenalty` and `askPenalty`
6. Invokes `SetUpdateCallback()` → pushes to Kraken feed
7. Kraken feed stores penalties in `stellarBidPenalty`/`stellarAskPenalty`
8. Next `computeSyntheticPair()` call overrides feature row penalties
9. Strategy uses Stellar OB imbalance for spread adjustments

## Configuration

**No config changes required!** The Kraken feed reuses existing `PriceFeedOptions`:

```json
{
  "priceFeedOptions": {
    "baseAsset": "XLM",
    "counterAsset": "SHX",
    "bufferLength": 1000,
    "bookDepth": 5,
    "orderBookTickRate": 3000  // Stellar OB polling rate (ms)
  }
}
```

- `baseAsset`/`counterAsset`: Used to construct Kraken pair symbols (e.g., "XLMUSD", "SHXUSD")
- `bufferLength`: Size of features buffer for volatility calculation
- `bookDepth`: Depth levels for Stellar OB imbalance calculation
- `orderBookTickRate`: Polling interval for Stellar orderbook monitor

## Usage

### Selecting Kraken Feed

When starting the bot:
```
=== Price Feed Selection ===
Which price feed source would you like to use?
  1. stellar  - Use Stellar/Horizon order book (native DEX prices)
  2. binance  - Use Binance CEX order book (external prices)
  3. kraken   - Use Kraken synthetic pairs (BaseAsset/USD ÷ CounterAsset/USD + Stellar OB)

Enter your choice (stellar/binance/kraken): kraken
```

### Build and Run

```bash
# Build
go build -o sdex-marketmaker

# Run
./sdex-marketmaker
```

### Log Output

**Kraken Feed Logs:**
```
[KRAKEN] Starting dual price feed: XLM/USD and SHX/USD
[KRAKEN BASE] Connecting to XLMUSD...
[KRAKEN COUNTER] Connecting to SHXUSD...
[KRAKEN BASE] ✅ Successfully subscribed
[KRAKEN COUNTER] ✅ Successfully subscribed
[KRAKEN SYNTHETIC] XLM/SHX = 0.123456 | Bid: 0.123400 | Ask: 0.123500 | Spread: 81.0 bps | Vol: 0.000123 | Stellar OB: true
```

**Stellar OB Monitor Logs:**
```
[STELLAR OB] Starting Stellar orderbook monitor for XLM/SHX
[STELLAR OB] Tick Rate: 3000ms | Horizon: https://horizon.stellar.org
[STELLAR OB] Initial order book fetched successfully
[STELLAR OB] Imbalance: -0.0234 | BidPenalty: 0.0234 | AskPenalty: 0.0000 | Depth (bid/ask): 123.45/156.78
```

## How It Works: Example

### Scenario: Trading XLM/SHX on Stellar

**Given Kraken prices:**
- XLM/USD: bid=0.10, ask=0.11
- SHX/USD: bid=0.80, ask=0.85

**Synthetic XLM/SHX calculation:**
```
midXLM = (0.10 + 0.11) / 2 = 0.105
midSHX = (0.80 + 0.85) / 2 = 0.825
syntheticMid = 0.105 / 0.825 = 0.12727

syntheticBid = 0.10 / 0.85 = 0.11765  (buy XLM with USD, sell SHX for USD)
syntheticAsk = 0.11 / 0.80 = 0.13750  (sell XLM for USD, buy SHX with USD)
```

**Given Stellar orderbook:**
- Bid depth: 100 XLM
- Ask depth: 150 XLM

**Orderbook imbalance:**
```
depthImbalance = (100 - 150) / (100 + 150) = -50 / 250 = -0.2
bidPenalty = max(0, -(-0.2)) = 0.2  (ask side heavier → widen our bid)
askPenalty = max(0, -0.2) = 0      (bid side lighter → no penalty)
```

**Strategy uses:**
- Fair price from Kraken: 0.12727
- Volatility from Kraken: (rolling std dev of mid prices)
- OB imbalance from Stellar: bidPenalty=0.2, askPenalty=0

This combination gives you:
- **External price discovery** (Kraken's deep liquidity)
- **Synthetic pair support** (cross-rate construction)
- **Local market microstructure** (Stellar orderbook imbalance)

## Advantages

### Why Dual Streams?

1. **Illiquid pairs**: XLM/SHX might not trade directly on Kraken, but XLM/USD and SHX/USD do
2. **Better liquidity**: USD pairs typically have tighter spreads and more depth
3. **Flexibility**: Can trade any BaseAsset/CounterAsset pair as long as both have USD pairs

### Why Separate Stellar OB Monitor?

1. **Decoupling**: Price discovery (Kraken) separate from local market depth (Stellar)
2. **Hybrid strategy**: External fair price + local imbalance penalties
3. **Reusability**: Same Stellar monitor could serve multiple feeds
4. **Clarity**: Clean separation of concerns vs. mixed responsibilities

## Limitations

1. **Staleness check**: If either Kraken stream goes stale (>10s), synthetic pair calculation skips
2. **L1 quantities**: Kraken spread channel doesn't provide depth → uses placeholder quantities (100) for L1
3. **No depth from Kraken**: Only Stellar provides depth-based imbalance (Kraken just provides spread)
4. **Two network connections**: Requires both Kraken WS and Horizon HTTP to be operational

## Watchdog Protection

The Kraken feed is included in the price feed watchdog:
- Monitors `LastMessageAt` timestamp
- If no messages for 60 seconds → shuts down bot
- Prevents trading on stale prices

## Shutdown Behavior

On graceful shutdown (Ctrl+C):
1. Stops Kraken feed (closes both WS connections)
2. Stops Stellar OB monitor
3. Waits for goroutines to finish (5s timeout)
4. Cancels all offers on Stellar
5. Closes MongoDB connection

## Testing Checklist

- [ ] Verify both Kraken WS connections establish (BASE and COUNTER)
- [ ] Confirm subscription confirmations in logs
- [ ] Check synthetic mid/bid/ask calculations are correct
- [ ] Verify Stellar OB monitor starts and polls successfully
- [ ] Ensure `BidPenalty`/`AskPenalty` flow from Stellar → Kraken → Strategy
- [ ] Test with pairs that exist on Kraken (e.g., XLM/USD + USDC/USD)
- [ ] Monitor for reconnection behavior if WS drops
- [ ] Test graceful shutdown (all services stop cleanly)

## Future Enhancements

1. **Orderbook streaming**: Use Kraken's orderbook channel instead of spread for better depth
2. **Volume-weighted mid**: Use bid/ask volumes from Kraken for microprice calculation
3. **Multi-exchange aggregation**: Combine Kraken + Binance for best price discovery
4. **Synthetic pair from other bases**: Support non-USD pairs (e.g., BTC, EUR)
5. **Offer filtering from Stellar OB**: Currently notes we can't filter by offer ID from aggregated book
