# Horizon Feed Implementation Summary

## What Was Implemented

### 1. PriceFeed Interface (`pricefeed/interface.go`)
A common interface that both Binance and Horizon feeds implement:
- `Start()` - Begin streaming price data
- `Stop()` - Gracefully stop the feed
- `GetLatestFeatures()` - Get most recent feature row
- `GetLastMessageAt()` - Get timestamp of last message (for watchdog)

### 2. Horizon Feed (`pricefeed/horizon_feed.go`)
A new price feed implementation that:
- **Streams order book from Stellar/Horizon via SSE** (Server-Sent Events)
- **Filters out bot's own offers** from the order book before computing features
  - Uses the offer monitor's hashmap to identify own offers
  - Compares price and quantity with small tolerance
  - If offer not in hashmap, includes it in the order book
- **Computes the same features** as Binance feed:
  - External mid price
  - Microprice
  - Rolling volatility
  - Order book imbalances (TOB)
- **Automatically reconnects** on connection failures (5s delay)

### 3. Updated Components

#### Strategy Engine (`strategy/strategy.go`)
- Now accepts `pricefeed.PriceFeed` interface instead of concrete `*pricefeed.BinanceFeed`
- Works with any feed implementation (Binance or Horizon)

#### Main Application (`main.go`)
- **Interactive prompt** at startup to select price feed:
  - Type `stellar` or `1` for Horizon/Stellar DEX feed
  - Type `binance` or `2` for Binance CEX feed
- Initializes offer monitor before price feed (needed for Horizon filtering)
- Creates appropriate feed based on user selection

#### WARP.md
- Updated documentation to reflect dual feed support
- Added Horizon-specific troubleshooting
- Updated architecture diagrams and data flow

## How Own Offer Filtering Works

1. **Offer Monitor** continuously polls Horizon API for bot's active offers
2. **Hashmap Storage**: Offers stored in map with offer ID as key, contains:
   - Price (normalized)
   - Quantity (normalized)
   - Type (bid/ask)
3. **Horizon Feed Filtering** (`filterAndConvertLevels`):
   - For each order book entry from Horizon
   - Check if price/quantity match any offer in hashmap (with tolerance)
   - Tolerance: price < 0.000001, quantity < 0.0001
   - If match found, skip this entry (it's our offer) AND mark offer as matched
   - **Each offer can only be matched once** (prevents over-filtering)
   - If no match, include in order book
4. **Graceful handling**:
   - If hashmap is empty, no filtering occurs
   - If offer in hashmap but not in order book, ignore (already filled/cancelled)
   - **Prevents false positives**: Even if multiple OB entries match the same offer, only one is filtered

## Data Flow Comparison

### Binance Feed Flow
```
Binance WebSocket
    ↓ (order book updates ~100ms)
BinanceFeed.processOrderBook()
    ↓ (compute features)
FeaturesBuffer.Append()
    ↓
Strategy reads via GetLatestFeatures()
```

### Horizon Feed Flow
```
Horizon SSE Stream
    ↓ (order book updates)
HorizonFeed.processOrderBook()
    ↓ (filter own offers using OfferMonitor hashmap)
Filtered Order Book
    ↓ (compute features)
FeaturesBuffer.Append()
    ↓
Strategy reads via GetLatestFeatures()
```

## Testing Checklist

### Pre-Testing
- [ ] Ensure MongoDB is running and config is loaded
- [ ] Ensure Horizon URL is correct in `.env` (testnet or mainnet)
- [ ] Build: `go build -o sdex-marketmaker`

### Test Binance Feed (Existing Functionality)
1. [ ] Run bot: `./sdex-marketmaker`
2. [ ] Select `binance` when prompted
3. [ ] Verify WebSocket connection succeeds
4. [ ] Verify order book updates appear in logs
5. [ ] Verify features are computed (check rolling volatility)
6. [ ] Verify strategy quotes are generated
7. [ ] Verify offers are placed/updated

### Test Horizon Feed (New Functionality)
1. [ ] Run bot: `./sdex-marketmaker`
2. [ ] Select `stellar` when prompted
3. [ ] Verify SSE connection succeeds
4. [ ] Verify order book updates appear in logs
5. [ ] **Check "Filtered out X own offers" in logs**
6. [ ] Verify features are computed
7. [ ] Verify strategy quotes are generated
8. [ ] Verify offers are placed/updated
9. [ ] **Watch for "Empty order book after filtering" warnings**
   - This means all orders are yours (thin market)
   - Strategy won't execute in this case

### Test Own Offer Filtering
1. [ ] Start bot with Horizon feed
2. [ ] Let it place initial offers
3. [ ] Watch offer monitor detect these offers
4. [ ] Observe Horizon feed log "Filtered out X own offers"
5. [ ] Verify order book doesn't include bot's own prices
6. [ ] Manually check Stellar Laboratory to confirm offers exist
7. [ ] Verify strategy still computes quotes based on other market orders

### Edge Cases to Test
- [ ] **Empty market**: What happens when only bot's offers exist?
  - Expected: "Empty order book after filtering" → no strategy execution
- [ ] **Offer partially filled**: Does filtering still work?
  - Expected: Quantity changes, filter might not match, order appears in book
- [ ] **Network interruption**: Does Horizon feed reconnect?
  - Expected: Error logged, reconnection attempt after 5s
- [ ] **Watchdog**: Does it detect dead Horizon feed?
  - Expected: After 30s of no messages, bot shuts down

## Known Limitations

1. **Price/Quantity Matching Tolerance**: 
   - Uses fixed tolerances (price: 0.000001, qty: 0.0001)
   - May need adjustment for different asset scales

2. **Horizon SSE Stream**:
   - No native heartbeat mechanism
   - Relies on order book updates to stay alive
   - Very quiet markets may trigger watchdog

3. **Performance**:
   - Horizon feed filters on every update
   - O(n*m) complexity where n=order book size, m=own offers
   - Should be fine for typical market maker (1-2 offers)

## Troubleshooting

### "Empty order book after filtering"
**Cause**: All orders in the book belong to the bot  
**Solution**: Wait for other market participants, or use Binance feed

### "Connection error" with Horizon SSE
**Cause**: Network issues, wrong Horizon URL, or API downtime  
**Solution**: Check `HORIZON_BASE_URI` in `.env`, verify network, or use Binance feed

### Features not computing
**Cause**: No valid order book data after filtering  
**Solution**: Ensure there are other market participants' orders

### Offers not being filtered
**Cause**: Offer monitor hasn't detected offers yet, or price/quantity mismatch  
**Solution**: Check offer monitor logs, adjust tolerance if needed

## Next Steps / Future Enhancements

1. **Dynamic Tolerance**: Make filter tolerance configurable
2. **Hybrid Mode**: Use Binance for fair price, Horizon for order book imbalance
3. **Performance Optimization**: Cache offer lookups, use sets instead of iteration
4. **Offer ID Matching**: If Horizon order book includes offer IDs in future, match directly
5. **Metrics**: Track filter effectiveness (% of orders filtered)

## Files Changed

- **New Files**:
  - `pricefeed/interface.go`
  - `pricefeed/horizon_feed.go`
  - `HORIZON_FEED_IMPLEMENTATION.md` (this file)

- **Modified Files**:
  - `main.go` - Added prompt, feed selection logic
  - `strategy/strategy.go` - Changed to use PriceFeed interface
  - `WARP.md` - Updated documentation

- **Unchanged** (works with both feeds):
  - `offers/offer_manager.go`
  - `offers/offer_monitor.go`
  - `balance/monitor.go`
  - `config/model.go`
  - All strategy formulas and calculations
