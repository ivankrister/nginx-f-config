# Edge Proxy Monitoring

The edge proxy now includes comprehensive monitoring for cache performance, prefetch operations, and origin request failures with both JSON API and visual dashboard interfaces.

## Features Added

### 1. Cache Monitoring
- **Cache Hit/Miss Ratio**: Tracks cache effectiveness
- **Cache Size**: Current number of cached items
- **Cache Evictions**: Number of items evicted from cache

### 2. Prefetch Monitoring
- **Prefetch Queue Size**: Number of active prefetch operations
- **Success/Failure Rates**: Prefetch operation success rates
- **Scheduled vs Completed**: Track prefetch scheduling and completion

### 3. Origin Request Monitoring
- **Request Counts**: Total requests per origin (Oryx, Perya, SV, SU, UK)
- **Failure Rates**: Percentage of failed requests per origin
- **Error Categorization**: Timeouts, DNS errors, connection errors
- **Performance Metrics**: Average response times

## Accessing Metrics

### 🎯 Visual Dashboard (Recommended)
```bash
# Through nginx proxy
http://localhost:8080/dashboard

# Direct to Go service (if port 9000 is exposed)
http://localhost:9000/dashboard
```

**Features:**
- 📊 **Real-time charts** showing cache, prefetch, and origin metrics
- 📈 **Interactive visualizations** with Chart.js
- 🎨 **Beautiful responsive design** that works on desktop and mobile
- ⚡ **Auto-refresh** every 10 seconds
- 📋 **Detailed origin performance table** with failure rate indicators

### 🔗 JSON API Endpoint
```bash
# Through nginx proxy (recommended)
curl http://localhost:8080/metrics

# Direct to Go service (if port 9000 is exposed)
curl http://localhost:9000/metrics
```

This returns a JSON response with comprehensive metrics:

```json
{
  "timestamp": "2025-11-13T10:30:00Z",
  "uptime": "2h15m30s",
  "cache_hits": 1250,
  "cache_misses": 180,
  "cache_hit_ratio": 87.4,
  "cache_size": 450,
  "cache_evicted": 15,
  "prefetch_scheduled": 890,
  "prefetch_success": 845,
  "prefetch_failures": 45,
  "prefetch_success_rate": 94.9,
  "prefetch_active": 3,
  "origin_requests": 1430,
  "origin_failures": 25,
  "origin_failure_rate": 1.7,
  "origin_timeouts": 10,
  "origin_dns_errors": 2,
  "origin_conn_errors": 13,
  "origin_stats": {
    "oryx": {
      "requests": 800,
      "failures": 15,
      "failure_rate": 1.9
    },
    "perya": {
      "requests": 300,
      "failures": 5,
      "failure_rate": 1.7
    },
    "sv": {
      "requests": 200,
      "failures": 3,
      "failure_rate": 1.5
    },
    "su": {
      "requests": 100,
      "failures": 2,
      "failure_rate": 2.0
    },
    "uk": {
      "requests": 30,
      "failures": 0,
      "failure_rate": 0.0
    }
  },
  "avg_response_time_ms": 45,
  "request_count": 1430
}
```

### Log Output
The proxy automatically logs key metrics every minute:

```
2025/11/13 10:30:00 METRICS: requests=1430 cache_hit_ratio=87.4% prefetch_success=94.9% origin_failures=1.7% active_prefetch=3 avg_response_ms=45
```

## Key Metrics to Monitor

### Performance Indicators
- **Cache Hit Ratio**: Should be > 80% for optimal performance
- **Average Response Time**: Monitor for latency increases
- **Origin Failure Rate**: Should be < 5% under normal conditions

### Operational Health
- **Prefetch Success Rate**: Should be > 90%
- **Active Prefetch Count**: Monitor queue depth
- **Error Types**: Watch for patterns in timeout/DNS/connection errors

### Capacity Planning
- **Request Count**: Track overall load
- **Cache Size**: Monitor memory usage
- **Cache Evictions**: Indicates if cache size needs adjustment

## Alerting Recommendations

1. **Cache Hit Ratio < 70%**: Investigate cache configuration
2. **Origin Failure Rate > 10%**: Check upstream health
3. **Prefetch Failure Rate > 20%**: Review prefetch configuration
4. **Average Response Time > 1000ms**: Performance degradation
5. **Active Prefetch > 50**: Potential queue backup

## Integration with Monitoring Systems

The `/metrics` endpoint can be scraped by:
- Prometheus
- Grafana
- DataDog
- Custom monitoring scripts

Example Prometheus configuration:
```yaml
- job_name: 'edge-proxy'
  static_configs:
    - targets: ['localhost:9000']
  metrics_path: '/metrics'
  scrape_interval: 30s
```

## Quick Start

1. **Rebuild and start the containers:**
   ```bash
   docker-compose down
   docker-compose build edge-go
   docker-compose up -d
   ```

2. **Access the dashboard:**
   - Open your browser and go to: `http://localhost:8080/dashboard`
   - Or directly: `http://localhost:9000/dashboard` (if port 9000 is exposed)

3. **View raw metrics:**
   - JSON API: `http://localhost:8080/metrics`
   - Or: `curl http://localhost:8080/metrics | jq`

The dashboard will automatically refresh every 10 seconds and provides:
- Real-time performance metrics
- Interactive charts for cache, prefetch, and origin data
- Color-coded failure rate indicators
- Mobile-responsive design
- **🔄 Manual Reset Button** with confirmation dialog

## 🔄 **Metrics Reset Features**

### **Automatic Daily Reset**
By default, metrics reset daily at midnight (00:00). This helps maintain relevant daily statistics.

**Configuration:**
```bash
# Enable/disable daily reset (default: true)
METRICS_RESET_DAILY=true

# Set reset time in HH:MM format (default: 00:00 for midnight)
METRICS_RESET_TIME=00:00
```

**Examples:**
- Reset at 6 AM: `METRICS_RESET_TIME=06:00`
- Reset at midnight UTC: `METRICS_RESET_TIME=00:00`
- Disable daily reset: `METRICS_RESET_DAILY=false`

### **Manual Reset**
Reset metrics immediately using either method:

**1. Dashboard Button:**
- Click the **🔄 Reset Metrics** button in the dashboard header
- Confirm the action in the dialog box
- All metrics will be cleared and start fresh

**2. HTTP API:**
```bash
curl -X POST http://localhost:8080/reset-metrics
# or direct: curl -X POST http://localhost:9000/reset-metrics
```

**What Gets Reset:**
- ✅ All request counters (total, per-origin)
- ✅ Cache statistics (hits, misses, evictions)
- ✅ Prefetch statistics (scheduled, success, failures)
- ✅ Error counters (timeouts, DNS, connection errors)
- ✅ Performance metrics (response times)
- ✅ Uptime counter (resets to current time)
- ❌ Active prefetch count (represents current state)

## Dashboard Screenshots

The dashboard includes:
- **Status Cards**: Key metrics at a glance (uptime, requests, cache hit ratio, response time, active prefetch)
- **Cache Performance**: Doughnut chart showing hits vs misses
- **Prefetch Operations**: Bar chart showing scheduled, successful, and failed prefetch operations
- **Origin Distribution**: Pie chart showing request distribution across origins
- **Error Analysis**: Bar chart breaking down error types (timeouts, DNS, connection)
- **Detailed Table**: Per-origin statistics with color-coded failure rates