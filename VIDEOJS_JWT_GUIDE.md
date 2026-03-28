# Video.js Integration with JWT Authentication

## Quick Setup

### 1. Include Video.js
```html
<link href="https://vjs.zencdn.net/8.10.0/video-js.css" rel="stylesheet" />
<script src="https://vjs.zencdn.net/8.10.0/video.min.js"></script>
```

### 2. HTML Video Element
```html
<video id="my-video" class="video-js" controls preload="auto">
  <p class="vjs-no-js">Please enable JavaScript</p>
</video>
```

### 3. Initialize Player with JWT Auth
```javascript
// Initialize Video.js
const player = videojs('my-video', {
  controls: true,
  autoplay: false,
  preload: 'auto'
});

// Your configuration
const EDGE_URL = 'http://localhost:8080';
const JWT_TOKEN = 'your-jwt-token-here';
const STREAM_PATH = '/oryx/stream.m3u8';

// Authenticate and play
async function playStream() {
  try {
    // Step 1: Get session ID
    const response = await fetch(`${EDGE_URL}/auth/validate`, {
      method: 'POST',
      headers: {
        'Authorization': `Bearer ${JWT_TOKEN}`
      }
    });
    
    const data = await response.json();
    const sessionId = data.session_id;
    
    // Step 2: Load stream with session ID
    player.src({
      src: `${EDGE_URL}${STREAM_PATH}?session_id=${sessionId}`,
      type: 'application/x-mpegURL'
    });
    
    // Optional: Auto-play
    player.play();
    
  } catch (error) {
    console.error('Authentication failed:', error);
  }
}

// Call it when ready
playStream();
```

## Complete Working Example

```html
<!DOCTYPE html>
<html>
<head>
  <title>Video.js JWT Auth</title>
  <link href="https://vjs.zencdn.net/8.10.0/video-js.css" rel="stylesheet" />
</head>
<body>
  <video id="my-video" class="video-js" controls width="640" height="360"></video>
  
  <script src="https://vjs.zencdn.net/8.10.0/video.min.js"></script>
  <script>
    const player = videojs('my-video');
    
    const config = {
      edgeUrl: 'http://localhost:8080',
      jwtToken: 'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...', // Your JWT
      streamPath: '/oryx/stream.m3u8'
    };
    
    async function loadStream() {
      try {
        // Authenticate
        const authResp = await fetch(`${config.edgeUrl}/auth/validate`, {
          method: 'POST',
          headers: { 'Authorization': `Bearer ${config.jwtToken}` }
        });
        
        if (!authResp.ok) {
          throw new Error('Authentication failed');
        }
        
        const { session_id } = await authResp.json();
        
        // Load stream
        player.src({
          src: `${config.edgeUrl}${config.streamPath}?session_id=${session_id}`,
          type: 'application/x-mpegURL'
        });
        
        console.log('Stream loaded successfully');
        
      } catch (error) {
        console.error('Error:', error);
        alert('Failed to load stream: ' + error.message);
      }
    }
    
    // Load on page ready
    loadStream();
  </script>
</body>
</html>
```

## Handling One-Time Use Sessions

Since sessions are **one-time use**, you need to handle re-authentication scenarios:

### Scenario 1: Switching Streams

```javascript
let currentJWT = 'your-jwt-token';

async function playStream(streamPath) {
  // Always get fresh session for new stream
  const authResp = await fetch(`${EDGE_URL}/auth/validate`, {
    method: 'POST',
    headers: { 'Authorization': `Bearer ${currentJWT}` }
  });
  
  const { session_id } = await authResp.json();
  
  player.src({
    src: `${EDGE_URL}${streamPath}?session_id=${session_id}`,
    type: 'application/x-mpegURL'
  });
}

// Switch between streams
document.getElementById('stream1-btn').onclick = () => playStream('/stream1.m3u8');
document.getElementById('stream2-btn').onclick = () => playStream('/stream2.m3u8');
```

### Scenario 2: Error Recovery

```javascript
player.on('error', async function() {
  const error = player.error();
  
  // Network errors might be auth-related
  if (error.code === 2 || error.code === 4) {
    console.log('Possible auth issue, re-authenticating...');
    
    try {
      // Get new session
      const authResp = await fetch(`${EDGE_URL}/auth/validate`, {
        method: 'POST',
        headers: { 'Authorization': `Bearer ${currentJWT}` }
      });
      
      const { session_id } = await authResp.json();
      
      // Retry with new session
      const currentSrc = player.currentSrc();
      const streamPath = new URL(currentSrc).pathname;
      
      player.src({
        src: `${EDGE_URL}${streamPath}?session_id=${session_id}`,
        type: 'application/x-mpegURL'
      });
      
    } catch (e) {
      console.error('Re-authentication failed:', e);
    }
  }
});
```

### Scenario 3: Page Reload

```javascript
// Save JWT in localStorage (not session ID!)
function saveJWT(token) {
  localStorage.setItem('jwt_token', token);
}

function loadJWT() {
  return localStorage.getItem('jwt_token');
}

// On page load
document.addEventListener('DOMContentLoaded', async function() {
  const player = videojs('my-video');
  
  const jwt = loadJWT();
  if (!jwt) {
    alert('Please login first');
    return;
  }
  
  // Always get fresh session on reload
  const authResp = await fetch(`${EDGE_URL}/auth/validate`, {
    method: 'POST',
    headers: { 'Authorization': `Bearer ${jwt}` }
  });
  
  const { session_id } = await authResp.json();
  
  player.src({
    src: `${EDGE_URL}/stream.m3u8?session_id=${session_id}`,
    type: 'application/x-mpegURL'
  });
});
```

## Advanced: Reusable Authentication Class

```javascript
class StreamAuthenticator {
  constructor(edgeUrl, jwtToken) {
    this.edgeUrl = edgeUrl;
    this.jwtToken = jwtToken;
  }
  
  async authenticate() {
    const response = await fetch(`${this.edgeUrl}/auth/validate`, {
      method: 'POST',
      headers: {
        'Authorization': `Bearer ${this.jwtToken}`,
        'Content-Type': 'application/json'
      }
    });
    
    if (!response.ok) {
      const error = await response.text();
      throw new Error(`Authentication failed: ${error}`);
    }
    
    const data = await response.json();
    return data.session_id;
  }
  
  async getStreamUrl(streamPath) {
    const sessionId = await this.authenticate();
    return `${this.edgeUrl}${streamPath}?session_id=${sessionId}`;
  }
  
  async loadIntoPlayer(player, streamPath) {
    try {
      const streamUrl = await this.getStreamUrl(streamPath);
      
      player.src({
        src: streamUrl,
        type: 'application/x-mpegURL'
      });
      
      return true;
    } catch (error) {
      console.error('Failed to load stream:', error);
      return false;
    }
  }
}

// Usage
const auth = new StreamAuthenticator('http://localhost:8080', 'your-jwt-token');
const player = videojs('my-video');

// Load stream
await auth.loadIntoPlayer(player, '/oryx/stream.m3u8');

// Switch streams
document.getElementById('btn1').onclick = () => 
  auth.loadIntoPlayer(player, '/stream1.m3u8');

document.getElementById('btn2').onclick = () => 
  auth.loadIntoPlayer(player, '/stream2.m3u8');
```

## Common Issues & Solutions

### Issue: "session already used"
**Cause:** Trying to use the same session ID twice  
**Solution:** Call `/auth/validate` again to get a fresh session

```javascript
// ❌ Wrong - reusing session
const session = await authenticate();
player.src({ src: `/stream1.m3u8?session_id=${session}` }); // Works
player.src({ src: `/stream2.m3u8?session_id=${session}` }); // Fails!

// ✅ Correct - fresh session each time
const session1 = await authenticate();
player.src({ src: `/stream1.m3u8?session_id=${session1}` });

const session2 = await authenticate(); // Get new session
player.src({ src: `/stream2.m3u8?session_id=${session2}` });
```

### Issue: "IP address mismatch"
**Cause:** User's IP changed (VPN, mobile network switch)  
**Solution:** Re-authenticate from new IP

```javascript
// Detect network changes and re-authenticate
window.addEventListener('online', async function() {
  console.log('Network connection restored, re-authenticating...');
  await loadStream();
});
```

### Issue: Stream doesn't play
**Cause:** Various - check browser console  
**Solution:** Debug systematically

```javascript
player.on('loadstart', () => console.log('Loading...'));
player.on('error', (e) => console.error('Error:', player.error()));
player.on('loadedmetadata', () => console.log('Metadata loaded'));
player.on('canplay', () => console.log('Can play!'));

// Check if stream URL is correct
console.log('Stream URL:', player.currentSrc());

// Verify session in network tab (DevTools)
```

## Testing Locally

1. **Start your edge proxy**
   ```bash
   export JWT_ENABLED=true
   export JWT_SECRET="your-secret"
   ./edge
   ```

2. **Create test HTML** (use example above)

3. **Generate test JWT**
   ```python
   import jwt
   token = jwt.encode({'sub': 'test-user'}, 'your-secret', algorithm='HS256')
   print(token)
   ```

4. **Open in browser** and check console for errors

## Production Checklist

- [ ] Use HTTPS for edge proxy
- [ ] Don't hardcode JWT tokens in frontend
- [ ] Fetch JWT from your backend auth API
- [ ] Handle token expiration gracefully
- [ ] Implement error boundaries/fallbacks
- [ ] Add loading states during authentication
- [ ] Monitor authentication failures
- [ ] Set appropriate CORS headers
- [ ] Use strong JWT secrets
- [ ] Implement rate limiting

## Full Production Example

```javascript
class SecureStreamPlayer {
  constructor(videoElementId, authServerUrl, edgeProxyUrl) {
    this.player = videojs(videoElementId);
    this.authServerUrl = authServerUrl;
    this.edgeProxyUrl = edgeProxyUrl;
    this.jwtToken = null;
    
    this.setupErrorHandling();
  }
  
  setupErrorHandling() {
    this.player.on('error', async () => {
      const error = this.player.error();
      if (error.code === 2 || error.code === 4) {
        // Possible auth issue - try to recover
        await this.retryWithFreshAuth();
      }
    });
  }
  
  async login(username, password) {
    // Get JWT from your auth server
    const response = await fetch(`${this.authServerUrl}/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password })
    });
    
    const data = await response.json();
    this.jwtToken = data.token;
    return true;
  }
  
  async playStream(streamPath) {
    if (!this.jwtToken) {
      throw new Error('Not authenticated. Call login() first.');
    }
    
    // Get session from edge proxy
    const authResp = await fetch(`${this.edgeProxyUrl}/auth/validate`, {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${this.jwtToken}` }
    });
    
    if (!authResp.ok) {
      throw new Error('Session creation failed');
    }
    
    const { session_id } = await authResp.json();
    
    // Load stream
    this.player.src({
      src: `${this.edgeProxyUrl}${streamPath}?session_id=${session_id}`,
      type: 'application/x-mpegURL'
    });
    
    this.player.play();
  }
  
  async retryWithFreshAuth() {
    console.log('Retrying with fresh session...');
    const currentPath = new URL(this.player.currentSrc()).pathname;
    await this.playStream(currentPath);
  }
}

// Usage
const streamPlayer = new SecureStreamPlayer(
  'my-video',
  'https://auth.example.com',
  'https://edge.example.com'
);

// Login and play
await streamPlayer.login('user@example.com', 'password');
await streamPlayer.playStream('/live/channel1.m3u8');
```

---

**See Also:**
- [videojs-jwt-example.html](videojs-jwt-example.html) - Interactive demo
- [ONE_TIME_USE_SESSIONS.md](ONE_TIME_USE_SESSIONS.md) - Security details
- [QUICK_START_JWT.md](QUICK_START_JWT.md) - Quick reference
