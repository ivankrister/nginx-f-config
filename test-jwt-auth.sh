#!/bin/bash

# Quick Test Script for JWT Authentication
# Usage: ./test-jwt-auth.sh

set -e

# Configuration
EDGE_URL="${EDGE_URL:-http://localhost:8080}"
JWT_SECRET="${JWT_SECRET:-your-super-secret-key-here}"
STREAM_PATH="${STREAM_PATH:-/oryx/test.m3u8}"

echo "=== JWT Authentication Test ==="
echo "Edge URL: $EDGE_URL"
echo ""

# Check if Python is available for JWT generation
if ! command -v python3 &> /dev/null; then
    echo "Error: python3 is required to generate JWT tokens"
    echo "Install with: brew install python3 (macOS) or apt-get install python3 (Linux)"
    exit 1
fi

# Install pyjwt if needed
python3 -c "import jwt" 2>/dev/null || {
    echo "Installing PyJWT..."
    pip3 install pyjwt
}

# Generate JWT Token
echo "Step 1: Generating JWT token..."
JWT_TOKEN=$(python3 -c "
import jwt
import datetime
token = jwt.encode(
    {
        'sub': 'test-user',
        'user_id': 'test@example.com',
        'exp': datetime.datetime.utcnow() + datetime.timedelta(hours=24)
    },
    '$JWT_SECRET',
    algorithm='HS256'
)
print(token)
")

echo "Generated JWT: ${JWT_TOKEN:0:50}..."
echo ""

# Validate JWT and get session
echo "Step 2: Validating JWT and getting session ID..."
AUTH_RESPONSE=$(curl -s -w "\nHTTP_STATUS:%{http_code}" -X POST "${EDGE_URL}/auth/validate" \
  -H "Authorization: Bearer ${JWT_TOKEN}")

HTTP_STATUS=$(echo "$AUTH_RESPONSE" | grep HTTP_STATUS | cut -d: -f2)
RESPONSE_BODY=$(echo "$AUTH_RESPONSE" | sed '/HTTP_STATUS/d')

if [ "$HTTP_STATUS" != "200" ]; then
    echo "❌ Authentication failed!"
    echo "HTTP Status: $HTTP_STATUS"
    echo "Response: $RESPONSE_BODY"
    exit 1
fi

SESSION_ID=$(echo "$RESPONSE_BODY" | python3 -c "import sys, json; print(json.load(sys.stdin)['session_id'])")

if [ -z "$SESSION_ID" ]; then
    echo "❌ Failed to extract session ID"
    echo "Response: $RESPONSE_BODY"
    exit 1
fi

echo "✅ Session created: ${SESSION_ID:0:20}..."
echo ""

# Test M3U8 access with session
echo "Step 3: Testing M3U8 access with session..."

# Test with cookie
echo "Testing with cookie header..."
M3U8_RESPONSE=$(curl -s -w "\nHTTP_STATUS:%{http_code}" \
  -H "Cookie: session_id=${SESSION_ID}" \
  "${EDGE_URL}${STREAM_PATH}")

HTTP_STATUS=$(echo "$M3U8_RESPONSE" | grep HTTP_STATUS | cut -d: -f2)

if [ "$HTTP_STATUS" = "200" ]; then
    echo "✅ M3U8 access successful (cookie)"
elif [ "$HTTP_STATUS" = "401" ]; then
    echo "❌ M3U8 access denied (401 Unauthorized)"
else
    echo "⚠️  Unexpected status: $HTTP_STATUS"
fi
echo ""

# Test with query parameter
echo "Testing with query parameter..."
M3U8_RESPONSE=$(curl -s -w "\nHTTP_STATUS:%{http_code}" \
  "${EDGE_URL}${STREAM_PATH}?session_id=${SESSION_ID}")

HTTP_STATUS=$(echo "$M3U8_RESPONSE" | grep HTTP_STATUS | cut -d: -f2)

if [ "$HTTP_STATUS" = "200" ]; then
    echo "✅ M3U8 access successful (query param)"
elif [ "$HTTP_STATUS" = "401" ]; then
    echo "❌ M3U8 access denied (401 Unauthorized)"
else
    echo "⚠️  Unexpected status: $HTTP_STATUS"
fi
echo ""

# Test without session (should fail)
echo "Step 4: Testing M3U8 access without session (should fail)..."
M3U8_RESPONSE=$(curl -s -w "\nHTTP_STATUS:%{http_code}" \
  "${EDGE_URL}${STREAM_PATH}")

HTTP_STATUS=$(echo "$M3U8_RESPONSE" | grep HTTP_STATUS | cut -d: -f2)

if [ "$HTTP_STATUS" = "401" ]; then
    echo "✅ Correctly blocked access without session"
elif [ "$HTTP_STATUS" = "200" ]; then
    echo "⚠️  WARNING: M3U8 accessible without session (JWT may be disabled)"
else
    echo "⚠️  Unexpected status: $HTTP_STATUS"
fi
echo ""

echo "=== Test Complete ==="
echo ""
echo "To use this session in your player:"
echo "  Cookie: session_id=${SESSION_ID}"
echo "  Or URL: ${EDGE_URL}${STREAM_PATH}?session_id=${SESSION_ID}"
