package main

import (
	"qdrant-mcp-server/server"
)

// Version is the current version of the MCP server, injected during the build.
var Version = "1.0.0"

func main() {
	server.Start(Version)
}
