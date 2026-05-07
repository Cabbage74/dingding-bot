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
			Name:        "web_search",
			Description: "Search the web using DuckDuckGo (HTML version, no JavaScript required). Returns search result titles and snippets. Use for finding current information, news, or answers to questions. Works on the server without a browser.",
			InputSchema: llm.InputSchema{
				Type: "object",
				Properties: map[string]llm.SchemaProp{
					"query": {Type: "string", Description: "The search query"},
				},
				Required: []string{"query"},
			},
		},
		{
			Name:        "web_fetch",
			Description: "Fetch and extract text content from a web page using headless Chrome. Use for reading articles, checking web pages, or getting info from websites. Returns the visible text on the page (up to 5000 chars). Note: some sites (Zhihu, Bilibili) require login and will show login page instead.",
			InputSchema: llm.InputSchema{
				Type: "object",
				Properties: map[string]llm.SchemaProp{
					"url": {Type: "string", Description: "The URL of the web page to fetch (HTTPS only)"},
				},
				Required: []string{"url"},
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

	case "web_search":
		query, _ := args["query"].(string)
		if query == "" {
			return "error: no query provided"
		}
		url := "https://html.duckduckgo.com/html/?q=" + strings.ReplaceAll(query, " ", "+")
		resp, err := a.llm.HTTP.Get(url)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 16384)
		n, _ := resp.Body.Read(buf)
		html := string(buf[:n])
		// Simple extraction: get text between result snippets
		var results []string
		for _, line := range strings.Split(html, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "result__snippet") || strings.Contains(line, "result__title") {
				// Strip HTML tags
				text := stripTags(line)
				if text != "" {
					results = append(results, text)
				}
			}
		}
		if len(results) == 0 {
			return "no results found for: " + query
		}
		out := strings.Join(results, "\n")
		if len(out) > 3000 {
			out = out[:3000]
		}
		return out

	case "web_fetch":
		url, _ := args["url"].(string)
		if url == "" {
			return "error: no url provided"
		}
		if !strings.HasPrefix(url, "https://") {
			return "error: only HTTPS URLs are allowed"
		}
		out, err := exec.Command("/opt/dingding-bot/web_fetch.sh", url, "12").CombinedOutput()
		if err != nil {
			return fmt.Sprintf("error: %v\noutput: %s", err, string(out))
		}
		result := strings.TrimSpace(string(out))
		if len(result) > 5000 {
			result = result[:5000] + "..."
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

func stripTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, c := range s {
		if c == '<' {
			inTag = true
		} else if c == '>' {
			inTag = false
		} else if !inTag {
			b.WriteRune(c)
		}
	}
	return strings.TrimSpace(b.String())
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
