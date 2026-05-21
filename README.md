mkdir -p ~/bin
go build -ldflags="-s -w" -o ~/bin/qdrant-mcp-server main.go