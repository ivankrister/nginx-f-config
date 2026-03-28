# Quick Start: JWT Authentication

## TL;DR
Two-step process to access m3u8 streams:

### 1. Get Session ID
```bash
curl -X POST http://your-edge:8080/auth/validate \
  -H "Authorization: Bearer YOUR_JWT_TOKEN"
```
Response:
```json
{"success": true, "session_id": "abc123...", "expires_in": 86400}
```

### 2. Access M3U8 with Session
```bash
# Option A: Cookie
curl -H "Cookie: session_id=abc123..." \
  http://your-edge:8080/path/stream.m3u8

# Option B: Query parameter (best for players)
curl http://your-edge:8080/path/stream.m3u8?session_id=abc123...
```

## Configuration 
```bash
export JWT_ENABLED=true
export JWT_SECRET="your-super-secret-key"
export SESSION_TTL=24h  # optional
```

## Player Integration

### HLS.js
```javascript
// 1. Validate JWT
const response = await fetch('http://edge:8080/auth/validate', {
  method: 'POST',
  headers: { 'Authorization': `Bearer ${jwtToken}` }
});
const { session_id } = await response.json();

// 2. Load stream with session
const url = `http://edge:8080/stream.m3u8?session_id=${session_id}`;
hls.loadSource(url);
```

### Video.js with HLS
```javascript
// 1. Get session ID (same as above)
const { session_id } = await getSession(jwtToken);

// 2. Initialize player
const player = videojs('video');
player.src({
  src: `http://edge:8080/stream.m3u8?session_id=${session_id}`,
  type: 'application/x-mpegURL'
});
```

### FFmpeg/VLC
```bash
# Get session ID first
SESSION_ID=$(curl -s -X POST http://edge:8080/auth/validate \
  -H "Authorization: Bearer $JWT" | jq -r '.session_id')

# Then play
ffplay "http://edge:8080/stream.m3u8?session_id=$SESSION_ID"
vlc "http://edge:8080/stream.m3u8?session_id=$SESSION_ID"
```

## Testing
```bash
./test-jwt-auth.sh
```

## How It Works
1. **Initial Request** → POST `/auth/validate` with JWT → Returns session_id
2. **Stream Access** → GET `/path/file.m3u8?session_id=...` → Returns playlist
3. **Segments** → Automatically accessible (no auth needed for .ts/.m4s files)

## Security Notes
- ✅ Sessions expire after SESSION_TTL (default 24h)
- ✅ Only .m3u8 files require session validation
- ✅ Segment files (.ts, .m4s) work without session
- ⚠️ Use HTTPS in production
- ⚠️ JWT_SECRET must be strong (32+ chars)

## Troubleshooting

**"Missing JWT token"** → Include `Authorization: Bearer TOKEN` header

**"Invalid token"** → Check JWT_SECRET matches between auth server and edge

**"Unauthorized: valid session required"** → Validate JWT first to get session_id

**No authentication enforced** → Set `JWT_ENABLED=true`

See [JWT_AUTHENTICATION_GUIDE.md](JWT_AUTHENTICATION_GUIDE.md) for full documentation.
