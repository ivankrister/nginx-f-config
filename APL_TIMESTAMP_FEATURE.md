# APL Timestamp Feature

## Overview
This feature adds Unix timestamps to APL segment filenames to prevent cache collisions when the origin stream rolls back.

## Problem
When an APL origin stream rolls back (restarts), it may regenerate segments with the same filename (e.g., `apexgaming0007.ts`) but with different content. This causes issues:
- Clients may receive stale/incorrect segments from cache
- The edge server can't distinguish between old and new versions of the same segment number

## Solution
Add Unix timestamps to segment filenames in the m3u8 playlist responses:
- **Original**: `apexgaming0007.ts`
- **Transformed**: `apexgaming0007-1770464225.ts`

Each unique segment gets a unique cache key based on its timestamp, so rollbacks create new cache entries instead of overwriting old ones.

## Implementation Details

### 1. Playlist Transformation
When serving APL m3u8 playlists, the edge server now transforms segment names by adding timestamps:

```go
// transformAPLPlaylist adds timestamps to segment names in APL m3u8 playlists
func transformAPLPlaylist(body []byte) []byte
```

**Example Input:**
```
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:3.998000,
apexgaming0000.ts
#EXTINF:2.000000,
apexgaming0001.ts
```

**Example Output:**
```
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:3.998000,
apexgaming0000-1770464225.ts
#EXTINF:2.000000,
apexgaming0001-1770464225.ts
```

### 2. Segment Request Handling
When clients request timestamped segments, the edge server:
1. Preserves the timestamp in the cache key
2. Strips the timestamp before requesting from origin
3. Caches the response with the timestamped key

```go
// stripTimestampFromSegment removes the timestamp suffix
// Example: "apexgaming0007-1770464225.ts" -> "apexgaming0007.ts"
func stripTimestampFromSegment(filename string) string
```

### 3. Request Flow

**Client Request:** `/apl/stream/apexgaming0007-1770464225.ts`

1. **selectUpstream()** - Returns path with timestamp preserved: `/stream/apexgaming0007-1770464225.ts`
2. **Cache Lookup** - Uses full timestamped path as cache key
3. **On Cache Miss:**
   - **fetchFromOrigin()** strips timestamp: requests `/stream/apexgaming0007.ts` from origin
   - Response is cached with timestamped key: `apexgaming0007-1770464225.ts`
4. **Response** - Served to client

### 4. Rollback Scenario

**Initial Stream:**
- Playlist contains: `apexgaming0007-1770464225.ts`
- Client requests and caches under key: `apexgaming0007-1770464225.ts`

**After Rollback:**
- Origin regenerates `apexgaming0007.ts` with new content
- New playlist contains: `apexgaming0007-1770464230.ts` (new timestamp)
- Client request for new timestamp = cache MISS
- Edge fetches new content from origin
- New content cached under new key: `apexgaming0007-1770464230.ts`

**Result:** Both versions coexist in cache without conflicts!

## Code Changes

### Modified Functions

1. **selectUpstream()** - Keeps timestamp in path for cache key differentiation
2. **fetchFromOrigin()** - Strips timestamp before requesting from origin
3. **transformAPLPlaylist()** - Adds timestamps to segment names in playlists

### New Helper Functions

1. **stripTimestampFromSegment(filename string) string**
   - Removes timestamp suffix from segment filenames
   - Pattern: `name-timestamp.ext` → `name.ext`

2. **addTimestampToSegment(filename string) string**
   - Adds Unix timestamp to segment filenames
   - Pattern: `name.ext` → `name-{unix_timestamp}.ext`

3. **transformAPLPlaylist(body []byte) []byte**
   - Transforms m3u8 playlist by adding timestamps to all segment references
   - Preserves all #EXT tags and comments

4. **storeCacheEntryWithKey(key, path string, resp, prefetched, ttl, grace)**
   - Allows storing cache entries with custom keys
   - Used internally to support timestamped caching

## Benefits

1. **Rollback Protection** - Old segments remain accessible even after stream rollback
2. **Cache Isolation** - Each timestamp version has its own cache entry
3. **Backward Compatible** - Works with existing origin servers (timestamp stripped for origin requests)
4. **Minimal Overhead** - Timestamp is a simple Unix timestamp (10 digits)
5. **Automatic** - No configuration required, works automatically for APL origin

## Testing

To test this feature:

1. Start the edge server with APL origin configured
2. Request an APL m3u8 playlist: `/apl/stream/playlist.m3u8`
3. Verify segments in response have timestamps: `apexgaming0007-1770464225.ts`
4. Request a segment with timestamp: `/apl/stream/apexgaming0007-1770464225.ts`
5. Verify it's cached correctly
6. Simulate rollback by restarting origin or waiting for new segments
7. Verify new timestamps create new cache entries

## Notes

- Timestamps are generated when the playlist is served, not when segments are fetched
- All segments in a single playlist response get the same timestamp
- The feature is automatic and only applies to APL origin requests
- Other origins (PERYA, SV, etc.) are not affected
