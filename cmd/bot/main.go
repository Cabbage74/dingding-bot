package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"gopkg.in/yaml.v3"

	"github.com/cabbage/dingding-bot/internal/agent"
	"github.com/cabbage/dingding-bot/internal/dingtalk"
	"github.com/cabbage/dingding-bot/internal/llm"
	"github.com/cabbage/dingding-bot/internal/memory"
	"github.com/cabbage/dingding-bot/internal/skill"
	"github.com/cabbage/dingding-bot/internal/store"
)

type Config struct {
	Server struct {
		Port         int           `yaml:"port"`
		ReadTimeout  time.Duration `yaml:"read_timeout"`
		WriteTimeout time.Duration `yaml:"write_timeout"`
	} `yaml:"server"`
	DingTalk struct {
		AppKey    string `yaml:"app_key"`
		AppSecret string `yaml:"app_secret"`
		RobotCode string `yaml:"robot_code"`
	} `yaml:"dingtalk"`
	DeepSeek struct {
		APIKey         string `yaml:"api_key"`
		ChatURL        string `yaml:"chat_url"`
		EmbedURL       string `yaml:"embed_url"`
		EmbedAPIKey    string `yaml:"embed_api_key"`
		Model          string `yaml:"model"`
		EmbeddingModel string `yaml:"embedding_model"`
	} `yaml:"deepseek"`
	Chroma struct {
		URL        string `yaml:"url"`
		Collection string `yaml:"collection"`
	} `yaml:"chroma"`
	SQLite struct {
		Path string `yaml:"path"`
	} `yaml:"sqlite"`
	Context struct {
		MaxMessages int           `yaml:"max_messages"`
		TTL         time.Duration `yaml:"ttl"`
	} `yaml:"context"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

func main() {
	cfgPath := "config.yaml"
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		cfgPath = v
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	var msgCount atomic.Int64
	startTime := time.Now()

	// Initialize SQLite
	db, err := store.Open(cfg.SQLite.Path)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	slog.Info("sqlite initialized", "path", cfg.SQLite.Path)

	// Initialize ChromaDB
	chromaClient := memory.NewChromaClient(cfg.Chroma.URL, cfg.Chroma.Collection)
	if err := chromaClient.EnsureCollection(); err != nil {
		slog.Warn("chromadb setup failed (will retry on use)", "error", err)
	} else {
		slog.Info("chromadb collection ready", "name", cfg.Chroma.Collection)
	}

	// Initialize DeepSeek LLM client
	llmClient := llm.NewClient(cfg.DeepSeek.APIKey, cfg.DeepSeek.ChatURL, cfg.DeepSeek.EmbedURL, cfg.DeepSeek.EmbedAPIKey)

	// Initialize context manager
	contextMgr := memory.NewContextManager(cfg.Context.MaxMessages, cfg.Context.TTL)
	contextMgr.SetLLMClient(llmClient, cfg.DeepSeek.Model)

	// Initialize memory extractor
	extractor := memory.NewExtractor(llmClient, cfg.DeepSeek.Model)

	// Initialize DingTalk client
	dtClient := dingtalk.NewClient(cfg.DingTalk.AppKey, cfg.DingTalk.AppSecret, cfg.DingTalk.RobotCode)

	// Initialize skill registry
	skillReg := skill.NewRegistry(db.DB)

	// Initialize cron scheduler
	cronHandler := func(s store.Skill) error {
		slog.Info("cron skill triggered", "name", s.Name, "chat_id", s.ChatID)
		if s.ChatID == "" {
			return fmt.Errorf("skill %q has no chat_id", s.Name)
		}
		resp, err := llmClient.ChatCompletion(&llm.ChatRequest{
			Model:    cfg.DeepSeek.Model,
			Messages: []llm.Message{{Role: "user", Content: s.ActionPrompt}},
			System:   "你是一个钉钉群聊助手。请根据用户的要求完成任务，回复要简洁、直接。",
			MaxTokens: 2000,
		})
		if err != nil {
			return fmt.Errorf("cron skill %q llm call: %w", s.Name, err)
		}
		if err := dtClient.ReplyText(s.ChatID, resp.Text); err != nil {
			return fmt.Errorf("cron skill %q send reply: %w", s.Name, err)
		}
		slog.Info("cron skill done", "name", s.Name, "response", resp.Text)
		return nil
	}
	skillExecutor := skill.NewExecutor(skillReg, cronHandler)

	// Create agent
	bot := agent.New(
		llmClient,
		cfg.DeepSeek.Model,
		cfg.DeepSeek.EmbeddingModel,
		chromaClient,
		contextMgr,
		extractor,
		skillReg,
		dtClient,
	)
	bot.SetOnSkillCreated(skillExecutor.Reload)
	bot.SetOnMessage(func() { msgCount.Add(1) })

	if err := skillExecutor.Start(); err != nil {
		slog.Error("start skill scheduler", "error", err)
	}

	// Start context cleanup ticker
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			contextMgr.Cleanup()
		}
	}()

	// Set up HTTP router
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Observability endpoints
	r.Get("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		skills, _ := skillReg.List()
		cronCount := 0
		for _, s := range skills {
			if s.TriggerType == "cron" {
				cronCount++
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uptime_seconds": time.Since(startTime).Seconds(),
			"messages_total": msgCount.Load(),
			"skills_total":   len(skills),
			"cron_skills":    cronCount,
		})
	})

	r.Get("/api/skills", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		skills, err := skillReg.List()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		json.NewEncoder(w).Encode(skills)
	})

	r.Get("/api/memories", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		memories := chromaClient.ListMemories(20)
		if memories == nil {
			memories = []memory.SearchResult{}
		}
		json.NewEncoder(w).Encode(memories)
	})

	// CRUD endpoints
	r.Delete("/api/skills", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", 400)
			return
		}
		var skillID int64
		if _, err := fmt.Sscanf(id, "%d", &skillID); err != nil {
			http.Error(w, "invalid id", 400)
			return
		}
		if err := skillReg.Delete(skillID); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		skillExecutor.Reload()
		w.Write([]byte(`{"ok":true}`))
	})

	r.Delete("/api/memories", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", 400)
			return
		}
		if err := chromaClient.DeleteMemory(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})

	r.Post("/api/skills", func(w http.ResponseWriter, r *http.Request) {
		var def skill.SkillDef
		if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
			http.Error(w, "invalid json", 400)
			return
		}
		chatID := r.URL.Query().Get("chat_id")
		if chatID == "" {
			chatID = "web"
		}
		if _, err := skillReg.Create(&def, "web", chatID); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		skillExecutor.Reload()
		w.Write([]byte(`{"ok":true}`))
	})

	r.Get("/api/tools", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tools := []map[string]interface{}{
			{"name": "run_shell", "description": "在服务器上执行Shell命令 (date, free等)", "status": "active"},
			{"name": "http_request", "description": "HTTPS GET请求，获取外部API数据", "status": "active"},
			{"name": "web_search", "description": "DuckDuckGo搜索，无需浏览器", "status": "active"},
			{"name": "web_fetch", "description": "Headless Chrome浏览网页提取文字", "status": "active"},
		}
		json.NewEncoder(w).Encode(tools)
	})

	r.Get("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	})

	// Start DingTalk Stream connection
	streamClient := dingtalk.NewStreamClient(
		cfg.DingTalk.AppKey,
		cfg.DingTalk.AppSecret,
		bot.ProcessMessage,
	)
	go func() {
		if err := streamClient.Connect(); err != nil {
			slog.Error("stream connect error", "error", err)
		}
	}()

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	slog.Info("starting server", "addr", addr)

	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down...")
	streamClient.Stop()
	skillExecutor.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	slog.Info("server stopped")
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>尼格 Bot Dashboard</title>
<style>
* { margin:0; padding:0; box-sizing:border-box; }
body { font-family: -apple-system, sans-serif; background: #f5f5f5; color: #333; padding: 20px; }
h1 { font-size: 24px; margin-bottom: 20px; }
.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 16px; margin-bottom: 24px; }
.card { background: white; border-radius: 8px; padding: 16px; box-shadow: 0 1px 3px rgba(0,0,0,.1); }
.card .label { font-size: 13px; color: #888; }
.card .value { font-size: 28px; font-weight: 700; margin-top: 4px; }
.card .value.green { color: #22c55e; }
.card .value.blue { color: #3b82f6; }
.section { background: white; border-radius: 8px; padding: 16px; margin-bottom: 16px; box-shadow: 0 1px 3px rgba(0,0,0,.1); }
.section h2 { font-size: 16px; margin-bottom: 12px; color: #555; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 8px 12px; border-bottom: 1px solid #eee; font-size: 14px; }
th { color: #888; font-weight: 500; }
.tag { display: inline-block; padding: 2px 8px; border-radius: 4px; font-size: 12px; }
.tag.cron { background: #fef3c7; color: #92400e; }
.tag.keyword { background: #dbeafe; color: #1e40af; }
.tag.mention { background: #d1fae5; color: #065f46; }
.empty { color: #aaa; font-size: 14px; padding: 12px 0; }
.refresh { font-size: 12px; color: #aaa; float: right; }
.btn { padding:4px 10px; border:none; border-radius:4px; cursor:pointer; font-size:12px; }
.btn.del { background:#fee2e2; color:#dc2626; }
.btn.del:hover { background:#fecaca; }
.btn.add { background:#dbeafe; color:#2563eb; margin-bottom:8px; }
.form-row { display:flex; gap:8px; margin-bottom:8px; flex-wrap:wrap; }
.form-row input, .form-row select { padding:6px 10px; border:1px solid #ddd; border-radius:4px; font-size:13px; }
</style>
</head>
<body>
<h1>🤖 尼格 Bot Dashboard</h1>
<div class="grid">
  <div class="card"><div class="label">运行时间</div><div class="value blue" id="uptime">--</div></div>
  <div class="card"><div class="label">处理消息</div><div class="value" id="msgs">0</div></div>
  <div class="card"><div class="label">技能总数</div><div class="value" id="skills_count">0</div></div>
  <div class="card"><div class="label">Cron 技能</div><div class="value green" id="cron">0</div></div>
</div>
<div class="section">
  <h2>可用 Tools</h2>
  <div id="tools_list"></div>
</div>
<div class="section">
  <h2>技能列表</h2>
  <div id="skill_form" style="display:none">
    <div class="form-row">
      <input id="s_name" placeholder="名称">
      <input id="s_desc" placeholder="描述/指令">
      <select id="s_type"><option value="cron">cron</option><option value="keyword">keyword</option><option value="mention">mention</option></select>
      <input id="s_config" placeholder="cron或关键词">
      <input id="s_chat" placeholder="chat_id(可选)">
    </div>
    <button class="btn add" onclick="createSkill()">确认创建</button>
  </div>
  <button class="btn add" onclick="document.getElementById('skill_form').style.display='block'">+ 新建技能</button>
  <div id="skills_list"></div>
</div>
<div class="section">
  <h2>长期记忆</h2><div id="memories_list"></div>
</div>
<div class="refresh" id="updated">加载中...</div>
<script>
async function load() {
  const [stats, tools, skills, memories] = await Promise.all([
    fetch('/api/stats').then(r=>r.json()),
    fetch('/api/tools').then(r=>r.json()),
    fetch('/api/skills').then(r=>r.json()),
    fetch('/api/memories').then(r=>r.json())
  ]);
  document.getElementById('uptime').textContent = fmtDuration(stats.uptime_seconds);
  document.getElementById('msgs').textContent = stats.messages_total;
  document.getElementById('skills_count').textContent = stats.skills_total;
  document.getElementById('cron').textContent = stats.cron_skills;
  if (tools && tools.length > 0) {
    document.getElementById('tools_list').innerHTML = '<table><tr><th>Tool</th><th>描述</th><th>状态</th></tr>'+tools.map(t=>'<tr><td><code>'+t.name+'</code></td><td>'+t.description+'</td><td><span class="tag '+(t.status==='active'?'mention':'cron')+'">'+(t.status==='active'?'✅ 可用':'❌ '+t.status)+'</span></td></tr>').join('')+'</table>';
  }
  if (skills && skills.length > 0) {
    document.getElementById('skills_list').innerHTML = '<table><tr><th>名称</th><th>类型</th><th>配置</th><th>操作</th></tr>'+skills.map(s=>'<tr><td>'+s.name+'</td><td><span class="tag '+s.trigger_type+'">'+s.trigger_type+'</span></td><td>'+s.trigger_config+'</td><td><button class="btn del" onclick="delSkill('+s.id+')">删除</button></td></tr>').join('')+'</table>';
  } else {
    document.getElementById('skills_list').innerHTML = '<div class="empty">暂无技能</div>';
  }
  if (memories && memories.length > 0) {
    document.getElementById('memories_list').innerHTML = '<table><tr><th>ID</th><th>内容</th><th>操作</th></tr>'+memories.map(m=>'<tr><td><code>'+m.ID+'</code></td><td>'+m.Document+'</td><td><button class="btn del" onclick="delMem(\''+m.ID+'\')">删除</button></td></tr>').join('')+'</table>';
  } else {
    document.getElementById('memories_list').innerHTML = '<div class="empty">暂无记忆</div>';
  }
  document.getElementById('updated').textContent = '更新于 '+new Date().toLocaleTimeString();
}
async function delSkill(id) { if(confirm('确认删除这个技能?')) { await fetch('/api/skills?id='+id,{method:'DELETE'}); load(); } }
async function delMem(id) { if(confirm('确认删除这条记忆?')) { await fetch('/api/memories?id='+id,{method:'DELETE'}); load(); } }
async function createSkill() {
  var body = {
    name: document.getElementById('s_name').value,
    description: document.getElementById('s_desc').value,
    trigger_type: document.getElementById('s_type').value,
    trigger_config: document.getElementById('s_config').value,
    action_prompt: document.getElementById('s_desc').value
  };
  var chat = document.getElementById('s_chat').value || 'web';
  var resp = await fetch('/api/skills?chat_id='+chat,{method:'POST',body:JSON.stringify(body)});
  if (resp.ok) { document.getElementById('skill_form').style.display='none'; load(); }
}
function fmtDuration(s) {
  const d=Math.floor(s/86400),h=Math.floor(s/3600)%24,m=Math.floor(s/60)%60;
  return (d>0?d+'d ':'')+(h>0?h+'h ':'')+m+'m';
}
load();
setInterval(load, 10000);
</script>
</body>
</html>`
