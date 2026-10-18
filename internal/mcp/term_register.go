package mcp

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// registerTermTools declares the single terminal entry, which is the whole
// default surface. Keep it that way: the tool listing is context the caller pays
// for on every turn, and the capability count must not move it (issue #117 33b).
func registerTermTools(mcpServer *mcpserver.MCPServer, s *Server) {
	mcpServer.AddTool(s.newTool(termToolName,
		mcpgo.WithDescription(termDescription),
		mcpgo.WithString("cmd", mcpgo.Description("One terminal line, e.g. `run a1f3:1 ls -la`. Empty = guide: usage summary plus what is available to act on now.")),
		mcpgo.WithNumber("timeout", mcpgo.Description("Time axis: max seconds to wait. A command that ends early returns early; 0 = do not wait."), mcpgo.DefaultNumber(defaultTimeoutSeconds)),
		mcpgo.WithNumber("bytes_limit", mcpgo.Description("Space axis: max raw bytes returned. 0 = handle only, consume nothing."), mcpgo.DefaultNumber(defaultBytesLimit)),
		mcpgo.WithNumber("lines_limit", mcpgo.Description("Space axis: max complete lines returned. 0 = no line limit."), mcpgo.DefaultNumber(defaultLinesLimit)),
		mcpgo.WithNumber("total_bytes_limit", mcpgo.Description("Whole reply: absolute cap across all slots."), mcpgo.DefaultNumber(defaultTotalBytesLimit)),
		mcpgo.WithNumber("quiet_ms", mcpgo.Description("pty completion: output silence in milliseconds that ends a command. pipe ignores it (process exit ends it)."), mcpgo.DefaultNumber(defaultQuietMillis)),
		mcpgo.WithString("on_time_overflow", mcpgo.Description("Time window overflow: truncate = return what arrived, fold = return no bytes."), mcpgo.DefaultString("truncate"), mcpgo.Enum("truncate", "fold")),
		mcpgo.WithString("on_bytes_overflow", mcpgo.Description("Byte window overflow: truncate = return the head, fold = return no bytes."), mcpgo.DefaultString("truncate"), mcpgo.Enum("truncate", "fold")),
		mcpgo.WithString("on_lines_overflow", mcpgo.Description("Line window overflow: truncate = return the first N lines, fold = return no bytes."), mcpgo.DefaultString("truncate"), mcpgo.Enum("truncate", "fold")),
		mcpgo.WithString("pipe_to", mcpgo.Description("Send this command's stdout into another handle's stdin; those bytes stay out of the reply body.")),
		mcpgo.WithBoolean("learn_mode", mcpgo.Description("Attach the matched directive's one-line usage to its slot (once per directive)."), mcpgo.DefaultBool(false)),
	), withLogging(termToolName, s.handleTerm, s))
}
