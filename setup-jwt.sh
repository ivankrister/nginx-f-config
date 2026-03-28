#!/bin/bash

# Setup script for JWT Authentication feature
echo "🔧 Setting up JWT Authentication..."
echo ""

# Step 1: Download dependencies
echo "Step 1: Downloading Go dependencies..."
go mod tidy

if [ $? -ne 0 ]; then
    echo "❌ Failed to download dependencies"
    exit 1
fi

echo "✅ Dependencies downloaded"
echo ""

# Step 2: Build the application
echo "Step 2: Building edge proxy..."
go build -o edge ./cmd/edge/main.go

if [ $? -ne 0 ]; then
    echo "❌ Build failed"
    exit 1
fi

echo "✅ Build successful"
echo ""

# Step 3: Show configuration instructions
echo "Step 3: Configuration"
echo "===================="
echo ""
echo "Set these environment variables:"
echo ""
echo "  export JWT_ENABLED=true"
echo "  export JWT_SECRET=\"your-super-secret-key-minimum-32-characters\""
echo "  export SESSION_TTL=24h  # Optional, default: 24h"
echo ""
echo "Example:"
echo "  export JWT_ENABLED=true"
echo "  export JWT_SECRET=\"$(openssl rand -base64 32)\""
echo ""

# Step 4: Running instructions
echo "Step 4: Run the edge proxy"
echo "=========================="
echo ""
echo "  ./edge"
echo ""
echo "Or with Docker:"
echo "  docker-compose up --build"
echo ""

echo "✅ Setup complete!"
echo ""
echo "📚 Documentation:"
echo "  - QUICK_START_JWT.md - Quick reference"
echo "  - VIDEOJS_JWT_GUIDE.md - Video.js integration"
echo "  - ONE_TIME_USE_SESSIONS.md - Security details"
echo "  - videojs-jwt-example.html - Interactive demo"
echo ""
echo "🧪 Test the implementation:"
echo "  ./test-jwt-auth.sh"
