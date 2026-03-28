# One-Time Use Sessions - Security Feature

## Overview

Sessions created from JWT validation are **ONE-TIME USE ONLY**. This prevents token/session sharing between users.

## Security Features

### 1. One-Time Use 🔒
- Each session can only be used **once** to access an m3u8 playlist
- After first use, the session is marked as "used" and becomes invalid
- Prevents sharing session IDs between multiple users

### 2. IP Address Binding 🌐
- Session is bound to the client's IP address during creation
- If session is used from a different IP, access is **denied**
- Prevents session hijacking or sharing across networks

### 3. User Agent Binding 🖥️
- Session is bound to the client's User-Agent header
- If session is used with a different user agent, access is **denied**
- Additional layer of security against session theft

## How It Works

### Normal Flow ✅
```
1. User authenticates → Gets session_id (bound to IP: 192.168.1.100)
2. User requests m3u8 → Session validated & marked as used
3. HLS player requests segments → No auth needed (standard HLS behavior)
4. User finishes watching → Session remains used but expired
```

### Prevented Scenarios ❌

#### Scenario 1: Sharing Session ID
```
User A: Authenticates → session_id: abc123
User A: Shares session_id with User B
User B: Tries to use session_id: abc123
Result: ❌ "session already used" OR "IP address mismatch"
```

#### Scenario 2: Session Theft
```
Attacker: Steals session_id from network
Attacker: Tries to use from different IP/browser
Result: ❌ "IP address mismatch" OR "user agent mismatch"
```

#### Scenario 3: Using Same Session Twice
```
User: Authenticates → Gets session
User: Accesses stream.m3u8 → ✅ Works (first time)
User: Tries to access stream.m3u8 again → ❌ "session already used"
```

## Important: Re-Authentication Required

Since sessions are one-time use, users need to **re-authenticate** for each new stream or reload.

### Video.js Example with Re-Auth
```javascript
async function playStream(streamPath) {
  // Always get a fresh session before loading a new stream
  const { session_id } = await authenticateJWT(jwtToken);
  
  // Load stream with fresh session
  player.src({
    src: `${edgeUrl}${streamPath}?session_id=${session_id}`,
    type: 'application/x-mpegURL'
  });
}

// When switching streams or reloading
async function changeStream(newStreamPath) {
  // Must re-authenticate to get new session
  await playStream(newStreamPath);
}
```

### Multi-Stream Support
If you need to support multiple concurrent streams, users must authenticate separately for each:

```javascript
// Stream 1
const session1 = await authenticateJWT(jwtToken);
player1.src({ src: `/stream1.m3u8?session_id=${session1.session_id}` });

// Stream 2 - needs separate session!
const session2 = await authenticateJWT(jwtToken);
player2.src({ src: `/stream2.m3u8?session_id=${session2.session_id}` });
```

## Error Messages

| Error | Meaning | Solution |
|-------|---------|----------|
| `session already used` | Session was used before | Re-authenticate to get new session |
| `IP address mismatch` | Accessing from different IP than created | Use same network or re-authenticate |
| `user agent mismatch` | Different browser/player | Use same browser or re-authenticate |
| `session expired` | Session TTL expired | Re-authenticate |
| `session not found` | Invalid/deleted session | Authenticate with valid JWT |

## Best Practices

### 1. Handle Re-Authentication Gracefully
```javascript
player.on('error', async function(e) {
  const error = player.error();
  
  if (error.code === 2) { // Network error, possibly auth issue
    console.log('Re-authenticating...');
    const newSession = await authenticateJWT(jwtToken);
    player.src({
      src: `${streamUrl}?session_id=${newSession.session_id}`,
      type: 'application/x-mpegURL'
    });
  }
});
```

### 2. Keep JWT Token Fresh
```javascript
// Store JWT token, not session ID
let jwtToken = getJWTFromAuthServer();

// For each stream access, create new session
async function loadStream(path) {
  const { session_id } = await fetch('/auth/validate', {
    headers: { 'Authorization': `Bearer ${jwtToken}` }
  }).then(r => r.json());
  
  return session_id;
}
```

### 3. User Experience Considerations
- **Pre-authenticate**: Get session before user clicks play
- **Loading states**: Show "Authenticating..." during session creation
- **Error handling**: Display friendly messages on auth failures
- **Refresh JWT**: Renew JWT tokens before they expire

## Logging & Monitoring

Edge proxy logs all authentication events:

```log
✅ Success:
JWT validated successfully, session created: abc123... (user: john@example.com, ip: 192.168.1.100)
Authorized m3u8 access: /stream.m3u8 (session: abc123..., ip: 192.168.1.100)

❌ Failures:
Unauthorized m3u8 access attempt: /stream.m3u8 (session: abc123, reason: session already used, ip: 192.168.1.100)
Unauthorized m3u8 access attempt: /stream.m3u8 (session: abc123, reason: IP address mismatch, ip: 192.168.1.200)
Unauthorized m3u8 access attempt: /stream.m3u8 (session: abc123, reason: user agent mismatch, ip: 192.168.1.100)
```

Monitor these logs to detect:
- Sharing attempts (same session from multiple IPs)
- Replay attacks (session already used)
- Suspicious activity patterns

## FAQ

### Q: Can I use the same session for all segments?
**A:** Yes! Session validation only applies to `.m3u8` playlist files. Segment files (`.ts`, `.m4s`) don't require authentication, which is standard for HLS streaming.

### Q: What if user refreshes the page?
**A:** They must re-authenticate. Implement auto-authentication on page load.

### Q: Can I disable one-time use?
**A:** Not currently. This is a core security feature. If you need multi-use sessions, you'd need to modify the code (not recommended for security).

### Q: What about DVR/seeking?
**A:** Works fine! The player requests different byte ranges of cached segments, but doesn't need to re-fetch the m3u8 after initial load.

### Q: Impact on CDN/caching?
**A:** Session IDs in query parameters don't affect segment caching. The edge proxy handles this correctly.

## Security Recommendations

1. ✅ **Always use HTTPS** in production
2. ✅ **Use strong JWT secrets** (32+ characters, random)
3. ✅ **Set reasonable JWT expiration** (e.g., 1 hour)
4. ✅ **Monitor authentication logs** for suspicious patterns
5. ✅ **Implement rate limiting** on `/auth/validate` endpoint
6. ✅ **Use short session TTL** (but long enough for playback)
7. ✅ **Validate JWT claims** (exp, iat, aud, iss)

## Advanced: Custom Security Policies

If you need different security models:

### Option 1: Multi-Use Sessions (Less Secure)
Comment out the "used" check in the code (not recommended)

### Option 2: Time-Based One-Time Use
Allow reuse within a short window (e.g., 5 seconds) for retries

### Option 3: Device Binding
Add device fingerprinting for mobile apps

These require code modifications to `cmd/edge/main.go`.

---

**Remember**: One-time use sessions provide strong security against unauthorized sharing while maintaining seamless HLS playback for legitimate users.
