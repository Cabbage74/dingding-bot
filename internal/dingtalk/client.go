package dingtalk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Client wraps the DingTalk Open API.
type Client struct {
	AppKey    string
	AppSecret string
	RobotCode string
	BaseURL   string
	HTTP      *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// NewClient creates a new DingTalk API client.
func NewClient(appKey, appSecret, robotCode string) *Client {
	return &Client{
		AppKey:    appKey,
		AppSecret: appSecret,
		RobotCode: robotCode,
		BaseURL:   "https://api.dingtalk.com",
		HTTP:      &http.Client{Timeout: 10 * time.Second},
	}
}

type tokenResp struct {
	AccessToken string `json:"accessToken"`
	ExpireIn    int    `json:"expireIn"`
	ErrCode     string `json:"code,omitempty"`
	ErrMsg      string `json:"message,omitempty"`
}

// AccessToken returns a cached or fresh access token.
func (c *Client) AccessToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	url := fmt.Sprintf("%s/v1.0/oauth2/accessToken", c.BaseURL)
	body := map[string]string{
		"appKey":    c.AppKey,
		"appSecret": c.AppSecret,
	}
	data, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("get token: %w", err)
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
	var tr tokenResp
	if err := json.Unmarshal(respData, &tr); err != nil {
		return "", fmt.Errorf("parse token resp: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("empty token, code=%s msg=%s", tr.ErrCode, tr.ErrMsg)
	}

	c.token = tr.AccessToken
	if tr.ExpireIn > 0 {
		c.tokenExpiry = time.Now().Add(time.Duration(tr.ExpireIn) * time.Second).Add(-60 * time.Second)
	}

	slog.Info("got dingtalk access token", "expires_in", tr.ExpireIn)
	return c.token, nil
}

// ReplyViaWebhook sends a text reply via the session webhook URL.
func (c *Client) ReplyViaWebhook(webhookURL, text string) error {
	body := map[string]interface{}{
		"msgtype": "text",
		"text": map[string]string{
			"content": text,
		},
	}
	data, _ := json.Marshal(body)
	slog.Info("webhook sending", "url", webhookURL, "body", string(data))

	req, _ := http.NewRequest("POST", webhookURL, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("webhook reply: %w", err)
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
	slog.Info("webhook reply", "status", resp.StatusCode, "body", string(respData))
	return nil
}

// ReplyText sends a text message to a group chat via the API.
func (c *Client) ReplyText(chatID, text string) error {
	msgParam, _ := json.Marshal(map[string]string{"content": text})
	return c.SendGroupMessage(chatID, "sampleText", string(msgParam))
}

// ReplyMarkdown sends a markdown message to a group chat.
func (c *Client) ReplyMarkdown(chatID, title, text string) error {
	msgParam, _ := json.Marshal(map[string]string{"title": title, "text": text})
	return c.SendGroupMessage(chatID, "sampleMarkdown", string(msgParam))
}

// SendGroupMessage sends a message to a group chat via the robot API.
func (c *Client) SendGroupMessage(chatID, msgKey, msgParam string) error {
	token, err := c.AccessToken()
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/v1.0/robot/groupMessages/send", c.BaseURL)
	body := map[string]interface{}{
		"openConversationId": chatID,
		"msgKey":             msgKey,
		"msgParam":           msgParam,
		"robotCode":          c.RobotCode,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
	slog.Info("send group message response", "status", resp.StatusCode, "body", string(respData))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("send message HTTP %d: %s", resp.StatusCode, string(respData))
	}

	return nil
}
