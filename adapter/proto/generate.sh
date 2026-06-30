#!/bin/bash
# Proto 代码生成脚本
set -e
cd "$(dirname "$0")"

echo "🔧 Generating protobuf code..."

if ! command -v protoc &> /dev/null; then
    echo "❌ protoc not found. Please install:"
    echo "   brew install protobuf (macOS)"
    echo "   apt-get install protobuf-compiler (Linux)"
    exit 1
fi

echo "📦 Installing Go protobuf plugins..."
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

echo "🔨 Generating Go code from adapter.proto..."
protoc --go_out=. --go_opt=paths=source_relative \
    --go-grpc_out=. --go-grpc_opt=paths=source_relative \
    adapter.proto

echo ""
echo "✅ Code generation complete!"
echo "📄 Generated files:"
echo "   - proto/adapter.pb.go"
echo "   - proto/adapter_grpc.pb.go"
