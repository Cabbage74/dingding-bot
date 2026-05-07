package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/cabbage/dingding-bot/internal/llm"
)

// Tool implementations

func (a *Agent) getTools() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		{
			Name:        "run_shell",
			Description: "Run a shell command on the server and return the output. Use for system queries: date, uptime, free, etc.",
			InputSchema: llm.InputSchema{
				Type: "object",
				Properties: map[string]llm.SchemaProp{
					"command": {Type: "string", Description: "The shell command to execute"},
				},
				Required: []string{"command"},
			},
		},
		{
			Name:        "http_request",
			Description: "Make an HTTP GET request to a URL and return the response body. Use for fetching data from APIs.",
			InputSchema: llm.InputSchema{
				Type: "object",
				Properties: map[string]llm.SchemaProp{
					"url": {Type: "string", Description: "The URL to fetch (HTTPS only)"},
				},
				Required: []string{"url"},
			},
		},
		{
			Name:        "opencli",
			Description: "Run an OpenCLI command to access websites as structured data. Use for searching/browsing content from Bilibili, Zhihu, GitHub, HackerNews, Reddit, Twitter, etc. Example commands: 'hackernews top --limit 5', 'bilibili hot --limit 5', 'github search --query AI --limit 5'. Run 'opencli list' to see all available sites.",
			InputSchema: llm.InputSchema{
				Type: "object",
				Properties: map[string]llm.SchemaProp{
					"command": {Type: "string", Description: "The opencli command to run (e.g. 'bilibili hot --limit 5')"},
				},
				Required: []string{"command"},
			},
		},
	}
}

// executeTools runs tool calls, returns results to LLM, and gets final response.
// Supports multi-round: if LLM requests more tools after first results, loops up to 3 rounds.
func (a *Agent) executeTools(contextMsgs []llm.Message, systemPrompt string, firstResp *llm.ChatResponse) string {
	messages := make([]llm.Message, len(contextMsgs))
	copy(messages, contextMsgs)

	lastText := firstResp.Text
	for round := 0; round < 3; round++ {
		if len(firstResp.ToolUses) == 0 {
			break
		}

		// Add assistant's full response (thinking + text + tool_use)
		messages = append(messages, llm.Message{
			Role:    "assistant",
			Content: firstResp.AllContent,
		})

		// Execute tools
		var toolResults []llm.ToolResultBlock
		for _, tu := range firstResp.ToolUses {
			result := a.executeTool(tu.Name, tu.Input)
			toolResults = append(toolResults, llm.ToolResultBlock{
				Type:      "tool_result",
				ToolUseID: tu.ID,
				Content:   result,
			})
			slog.Info("tool executed", "round", round, "name", tu.Name, "id", tu.ID)
		}

		// Send tool results back
		messages = append(messages, llm.Message{
			Role:    "user",
			Content: toolResultsToContent(toolResults),
		})

		// Get follow-up (might have more tools or final text)
		resp, err := a.llm.ChatCompletion(&llm.ChatRequest{
			Model:       a.llmModel,
			Messages:    messages,
			System:      systemPrompt,
			MaxTokens:   2000,
			Temperature: 0.7,
		})
		if err != nil {
			slog.Error("tool follow-up failed", "round", round, "error", err)
			return lastText
		}

		lastText = resp.Text
		firstResp = resp // continue loop if resp has more tool_use
	}
	return lastText
}

func (a *Agent) executeTool(name string, input json.RawMessage) string {
	var args map[string]interface{}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("error parsing args: %v", err)
	}

	switch name {
	case "run_shell":
		cmd, _ := args["command"].(string)
		if cmd == "" {
			return "error: no command provided"
		}
		// Safety: block dangerous commands
		if strings.Contains(cmd, "rm ") || strings.Contains(cmd, "sudo") || strings.Contains(cmd, "shutdown") {
			return "error: dangerous command blocked"
		}
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		if err != nil {
			return fmt.Sprintf("error: %v\noutput: %s", err, string(out))
		}
		result := strings.TrimSpace(string(out))
		if len(result) > 2000 {
			result = result[:2000] + "..."
		}
		return result

	case "opencli":
		cmd, _ := args["command"].(string)
		if cmd == "" {
			return "error: no command provided"
		}
		out, err := exec.Command("opencli", strings.Fields(cmd)...).CombinedOutput()
		if err != nil {
			return fmt.Sprintf("error: %v\noutput: %s", err, string(out))
		}
		result := strings.TrimSpace(string(out))
		if len(result) > 3000 {
			result = result[:3000] + "..."
		}
		return result

	case "http_request":
		url, _ := args["url"].(string)
		if url == "" {
			return "error: no url provided"
		}
		if !strings.HasPrefix(url, "https://") {
			return "error: only HTTPS URLs are allowed"
		}
		resp, err := a.llm.HTTP.Get(url)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(buf[:n]))

	default:
		return fmt.Sprintf("unknown tool: %s", name)
	}
}

func toolUsesToContent(tools []llm.ToolUseBlock) []interface{} {
	var blocks []interface{}
	for _, t := range tools {
		blocks = append(blocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    t.ID,
			"name":  t.Name,
			"input": t.Input,
		})
	}
	return blocks
}

func toolResultsToContent(results []llm.ToolResultBlock) []interface{} {
	var blocks []interface{}
	for _, r := range results {
		blocks = append(blocks, map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": r.ToolUseID,
			"content":     r.Content,
		})
	}
	return blocks
}
