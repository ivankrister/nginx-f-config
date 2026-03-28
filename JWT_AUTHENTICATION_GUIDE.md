# JWT Authentication for M3U8 Streams - Usage Guide

## Overview

The edge proxy now requires JWT authentication before allowing m3u8 playlist access. The flow is:
1. Client obtains a JWT token from your auth server
2. Client validates the JWT with the edge proxy to get a session ID
3. Client uses the session ID to request m3u8 playlists

## Configuration

### Environment Variables

```bash
# Enable JWT authentication
export JWT_ENABLED=true

# Secret key for validating JWT tokens (must match your auth server's secret)
export JWT_SECRET="your-super-secret-key-here"

# Session TTL (optional, default: 24h)
# Valid units: s (seconds), m (minutes), h (hours)
export SESSION_TTL=24h
```

### Docker Compose Example

```yaml
services:
  edge:
    environment:
      - JWT_ENABLED=true
      - JWT_SECRET=your-super-secret-key-here
      - SESSION_TTL=24h
```

## Usage Flow

### Step 1: Generate JWT Token

First, your authentication server needs to generate a JWT token. The token should use HMAC signing (HS256, HS384, or HS512) and include standard claims.

**Example JWT Payload:**
```json
{
  "sub": "user123",           // User ID (optional)
  "exp": 1711065600,          // Expiration timestamp
  "iat": 1711062000,          // Issued at timestamp
  "user_id": "john.doe"       // Alternative user ID field (optional)
}
```

**Example Token Generation (Node.js):**
```javascript
const jwt = require('jsonwebtoken');

const token = jwt.sign(
  { 
    sub: 'user123',
    user_id: 'john.doe'
  },
  'your-super-secret-key-here',  // Must match JWT_SECRET
  { 
    expiresIn: '24h',
    algorithm: 'HS256'
  }
);

console.log('Token:', token);
```

**Example Token Generation (Python):**
```python
import jwt
import datetime

payload = {
    'sub': 'user123',
    'user_id': 'john.doe',
    'exp': datetime.datetime.utcnow() + datetime.timedelta(hours=24),
    'iat': datetime.datetime.utcnow()
}

token = jwt.encode(
    payload,
    'your-super-secret-key-here',  # Must match JWT_SECRET
    algorithm='HS256'
)

print(f'Token: {token}')
```

### Step 2: Validate JWT and Get Session ID

Before accessing any m3u8 playlist, the client must validate the JWT token with the edge proxy.

**Request:**
```bash
# Using Authorization header (recommended)
curl -X POST http://your-edge-proxy:8080/auth/validate \
  -H "Authorization: Bearer YOUR_JWT_TOKEN_HERE"

# Or using query parameter
curl -X POST "http://your-edge-proxy:8080/auth/validate?token=YOUR_JWT_TOKEN_HERE"
```

**Response (Success):**
```json
{
  "success": true,
  "session_id": "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0u1v2w3x4y5z6",
  "expires_in": 86400
}
```

**Response (Failure):**
```
HTTP/1.1 401 Unauthorized
Invalid token
```

### Step 3: Request M3U8 Playlist with Session ID

Now use the session ID to access m3u8 playlists.

**Option A: Using Cookie (Recommended for browsers)**
```bash
curl -H "Cookie: session_id=YOUR_SESSION_ID" \
  http://your-edge-proxy:8080/path/to/stream.m3u8
```

**Option B: Using Query Parameter (Recommended for players)**
```bash
curl "http://your-edge-proxy:8080/path/to/stream.m3u8?session_id=YOUR_SESSION_ID"
```

**Without Valid Session:**
```
HTTP/1.1 401 Unauthorized
Unauthorized: valid session required
```

## Complete Client Implementation Examples

### JavaScript/Browser Example

```javascript
async function playStream(streamPath) {
  // Step 1: Get JWT from your auth server
  const jwtToken = await getJWTFromAuthServer();
  
  // Step 2: Validate JWT and get session ID
  const response = await fetch('http://your-edge-proxy:8080/auth/validate', {
    method: 'POST',
    headers: {
      'Authorization': `Bearer ${jwtToken}`
    }
  });
  
  if (!response.ok) {
    throw new Error('Authentication failed');
  }
  
  const data = await response.json();
  const sessionId = data.session_id;
  
  // Step 3: Play stream with session ID
  // Option A: Set cookie (for same-origin requests)
  document.cookie = `session_id=${sessionId}; path=/; max-age=${data.expires_in}`;
  const playlistUrl = `http://your-edge-proxy:8080${streamPath}`;
  
  // Option B: Use query parameter (works cross-origin)
  const playlistUrl = `http://your-edge-proxy:8080${streamPath}?session_id=${sessionId}`;
  
  // Initialize HLS.js player
  const video = document.getElementById('video');
  if (Hls.isSupported()) {
    const hls = new Hls();
    hls.loadSource(playlistUrl);
    hls.attachMedia(video);
  }
}
```

### cURL Complete Flow

```bash
#!/bin/bash

# Your JWT token
JWT_TOKEN="eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
EDGE_URL="http://localhost:8080"
STREAM_PATH="/oryx/stream.m3u8"

# Step 1: Validate JWT and get session ID
echo "Validating JWT..."
AUTH_RESPONSE=$(curl -s -X POST "${EDGE_URL}/auth/validate" \
  -H "Authorization: Bearer ${JWT_TOKEN}")

SESSION_ID=$(echo $AUTH_RESPONSE | jq -r '.session_id')

if [ "$SESSION_ID" = "null" ] || [ -z "$SESSION_ID" ]; then
  echo "Authentication failed!"
  echo $AUTH_RESPONSE
  exit 1
fi

echo "Session ID: $SESSION_ID"

# Step 2: Request m3u8 playlist
echo "Requesting playlist..."
curl -H "Cookie: session_id=${SESSION_ID}" \
  "${EDGE_URL}${STREAM_PATH}"

# Or with query parameter:
# curl "${EDGE_URL}${STREAM_PATH}?session_id=${SESSION_ID}"
```

### Python Example

```python
import requests
import jwt as pyjwt

# Configuration
EDGE_URL = "http://localhost:8080"
JWT_SECRET = "your-super-secret-key-here"
STREAM_PATH = "/oryx/stream.m3u8"

# Step 1: Generate JWT (normally done by your auth server)
token = pyjwt.encode(
    {'sub': 'user123', 'user_id': 'john.doe'},
    JWT_SECRET,
    algorithm='HS256'
)

# Step 2: Validate JWT and get session ID
auth_response = requests.post(
    f"{EDGE_URL}/auth/validate",
    headers={"Authorization": f"Bearer {token}"}
)

if auth_response.status_code != 200:
    print(f"Authentication failed: {auth_response.text}")
    exit(1)

auth_data = auth_response.json()
session_id = auth_data['session_id']
print(f"Session ID: {session_id}")

# Step 3: Request m3u8 playlist
# Option A: Using cookie
playlist_response = requests.get(
    f"{EDGE_URL}{STREAM_PATH}",
    cookies={"session_id": session_id}
)

# Option B: Using query parameter
playlist_response = requests.get(
    f"{EDGE_URL}{STREAM_PATH}?session_id={session_id}"
)

if playlist_response.status_code == 200:
    print("Playlist content:")
    print(playlist_response.text)
else:
    print(f"Failed to get playlist: {playlist_response.status_code}")
```

## Important Notes

### Session Expiration
- Sessions expire after `SESSION_TTL` (default: 24 hours)
- Clients should handle 401 responses and re-authenticate when sessions expire
- Expired sessions are automatically cleaned up by the edge proxy

### Security Best Practices
1. **HTTPS**: Always use HTTPS in production to protect JWT tokens
2. **JWT Secret**: Use a strong, random secret key (minimum 32 characters)
3. **Token Expiration**: Set reasonable JWT expiration times
4. **Session TTL**: Match session TTL to your use case (shorter is more secure)

### Non-M3U8 Content
- Authentication is **only** enforced for `.m3u8` playlist files
- Segment files (.ts, .m4s) do not require session validation
- This is standard for HLS streaming to reduce overhead

### Debugging
Check edge proxy logs for authentication events:
```bash
docker logs -f <container-name>
```

Look for messages like:
```
JWT validated successfully, session created: a1b2c3... (user: john.doe)
Unauthorized m3u8 access attempt: /path/file.m3u8 (session: invalid-id)
```

## Testing the Implementation

### 1. Test Without Authentication
```bash
curl http://localhost:8080/test/stream.m3u8
# Expected: 401 Unauthorized: valid session required
```

### 2. Test With Invalid Token
```bash
curl -X POST http://localhost:8080/auth/validate \
  -H "Authorization: Bearer invalid-token-here"
# Expected: 401 Unauthorized: Invalid token
```

### 3. Test Complete Flow
```bash
# Generate valid JWT, validate, get session, request m3u8
# (See complete cURL example above)
```

## Disabling JWT Authentication

To disable JWT authentication:
```bash
export JWT_ENABLED=false
# or remove the environment variable entirely
```

When disabled, m3u8 files are accessible without authentication (legacy behavior).
