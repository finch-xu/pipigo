package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Task 定义了定时任务的结构
type Task struct {
	ID       int    `json:"id" gorm:"primaryKey"`
	Name     string `json:"name"`
	CronExpr string `json:"cron"`
	URL      string `json:"url"`
	Method   string `json:"method"`
	// 新增字段
	Headers string `json:"headers" gorm:"type:text"` // 请求头 (JSON string)
	Body    string `json:"body" gorm:"type:text"`    // 请求体 (JSON string)
	Timeout int    `json:"timeout"`                  // 超时时间 (秒)

	Logs    []Log     `json:"logs" gorm:"foreignKey:TaskID;constraint:OnDelete:CASCADE"`
	NextRun time.Time `json:"next_run"`
}

// Log 定义了任务执行日志的结构
type Log struct {
	ID           int       `json:"id" gorm:"primaryKey"`
	TaskID       int       `json:"task_id"`
	Time         time.Time `json:"time"`
	StatusText   string    `json:"status_text"`                    // 简短的状态文本，例如 "状态: 200"
	ResponseBody string    `json:"response_body" gorm:"type:text"` // 完整的响应体
}

var (
	db        *gorm.DB
	tasks     = make(map[int]*Task)
	cronIDs   = make(map[int]cron.EntryID)
	taskMutex sync.Mutex
	c         = cron.New(cron.WithSeconds())
)

func main() {
	var err error
	db, err = gorm.Open(sqlite.Open("db/tasks.db"), &gorm.Config{})
	if err != nil {
		panic("连接数据库失败: " + err.Error())
	}

	// 自动迁移数据库结构
	db.AutoMigrate(&Task{}, &Log{})

	// 启动时从数据库加载任务
	loadTasksFromDB()

	r := gin.Default()

	// 首页
	r.GET("/", func(ctx *gin.Context) {
		ctx.Data(http.StatusOK, "text/html; charset=utf-8", []byte(htmlPage))
	})

	// 获取所有任务
	r.GET("/api/tasks", func(ctx *gin.Context) {
		var list []Task
		// 预加载日志并按时间倒序排序
		db.Preload("Logs", func(db *gorm.DB) *gorm.DB {
			return db.Order("logs.time DESC")
		}).Order("id DESC").Find(&list)

		// 更新每个任务的下一次执行时间
		taskMutex.Lock()
		for i := range list {
			if entryID, ok := cronIDs[list[i].ID]; ok {
				list[i].NextRun = c.Entry(entryID).Next
			}
		}
		taskMutex.Unlock()

		ctx.JSON(http.StatusOK, list)
	})

	// 添加新任务
	r.POST("/api/tasks", func(ctx *gin.Context) {
		var req Task
		if err := ctx.ShouldBindJSON(&req); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if req.Name == "" || req.CronExpr == "" || req.URL == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "任务名称、Cron表达式和URL是必填项"})
			return
		}

		if req.Timeout <= 0 {
			req.Timeout = 10 // 默认超时时间10秒
		}

		if err := db.Create(&req).Error; err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		registerTask(&req)
		ctx.JSON(http.StatusOK, req)
	})

	// 删除任务
	r.DELETE("/api/tasks/:id", func(ctx *gin.Context) {
		var task Task
		if err := db.First(&task, ctx.Param("id")).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
			return
		}

		// 从 cron 调度中移除
		taskMutex.Lock()
		if entryID, ok := cronIDs[task.ID]; ok {
			c.Remove(entryID)
			delete(cronIDs, task.ID)
		}
		delete(tasks, task.ID)
		taskMutex.Unlock()

		// 从数据库删除
		db.Delete(&task)
		ctx.JSON(http.StatusOK, gin.H{"message": "任务已删除"})
	})

	// 立即执行任务
	r.POST("/api/tasks/:id/run", func(ctx *gin.Context) {
		var task Task
		if err := db.First(&task, ctx.Param("id")).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
			return
		}
		go runTask(task.ID)
		ctx.JSON(http.StatusOK, gin.H{"message": "任务已在后台立即执行"})
	})

	c.Start()
	fmt.Println("服务已启动，请访问 http://localhost:8080")
	r.Run("0.0.0.0:8899")
}

// registerTask 将任务注册到 cron 调度器
func registerTask(t *Task) {
	taskMutex.Lock()
	tasks[t.ID] = t
	taskMutex.Unlock()

	entryID, err := c.AddFunc(t.CronExpr, func() {
		runTask(t.ID)
	})
	if err != nil {
		fmt.Printf("任务 #%d (%s) 注册失败: %v\n", t.ID, t.Name, err)
		return
	}

	taskMutex.Lock()
	cronIDs[t.ID] = entryID
	taskMutex.Unlock()
	fmt.Printf("任务 #%d (%s) 已成功注册, Cron: '%s'\n", t.ID, t.Name, t.CronExpr)
}

// runTask 执行指定的任务
func runTask(id int) {
	taskMutex.Lock()
	t, ok := tasks[id]
	taskMutex.Unlock()
	if !ok {
		fmt.Printf("执行任务失败：找不到任务 #%d\n", id)
		return
	}

	fmt.Printf("开始执行任务 #%d: %s\n", t.ID, t.Name)

	client := &http.Client{Timeout: time.Duration(t.Timeout) * time.Second}
	var req *http.Request
	var err error

	// 创建请求
	if t.Method == "POST" {
		req, err = http.NewRequest("POST", t.URL, bytes.NewBufferString(t.Body))
		if err == nil {
			// 默认设置为JSON格式，如果Headers中指定了，则会被覆盖
			req.Header.Set("Content-Type", "application/json")
		}
	} else { // 默认为GET
		req, err = http.NewRequest("GET", t.URL, nil)
	}

	if err != nil {
		appendLog(t.ID, "创建请求失败: "+err.Error(), "")
		return
	}

	// 设置请求头
	if t.Headers != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(t.Headers), &headers); err == nil {
			for key, value := range headers {
				req.Header.Set(key, value)
			}
		} else {
			// 如果JSON解析失败，记录一个警告，但继续执行
			fmt.Printf("任务 #%d 的请求头JSON格式错误: %v\n", t.ID, err)
		}
	}

	// 执行请求
	resp, err := client.Do(req)
	if err != nil {
		appendLog(t.ID, "请求失败: "+err.Error(), "")
		return
	}
	defer resp.Body.Close()

	// 读取响应体
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		appendLog(t.ID, fmt.Sprintf("状态: %d, 读取响应体失败: %s", resp.StatusCode, err.Error()), "")
		return
	}

	// 记录日志
	statusText := fmt.Sprintf("状态: %d", resp.StatusCode)
	appendLog(t.ID, statusText, string(bodyBytes))
}

// appendLog 向数据库添加一条日志
func appendLog(taskID int, statusText, responseBody string) {
	log := Log{
		TaskID:       taskID,
		Time:         time.Now(),
		StatusText:   statusText,
		ResponseBody: responseBody,
	}
	if err := db.Create(&log).Error; err != nil {
		fmt.Printf("任务 #%d 写日志失败: %v\n", taskID, err)
	}
}

// loadTasksFromDB 从数据库加载所有任务并注册它们
func loadTasksFromDB() {
	var list []Task
	db.Find(&list)
	fmt.Printf("从数据库加载了 %d 个任务...\n", len(list))
	for i := range list {
		// 使用拷贝，避免闭包问题
		taskCopy := list[i]
		registerTask(&taskCopy)
	}
}

// htmlPage 定义了前端页面的内容
const htmlPage = `
<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>定时任务管理器</title>
<script src="https://unpkg.com/vue@3/dist/vue.global.prod.js"></script>
<script src="https://unpkg.com/axios/dist/axios.min.js"></script>
<style>
	body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif; padding: 20px; background-color: #f4f7f9; color: #333; }
	#app { max-width: 900px; margin: 0 auto; }
	h1, h2 { color: #2c3e50; }
	.form-container { background: #fff; padding: 20px; border-radius: 8px; box-shadow: 0 2px 10px rgba(0,0,0,0.05); margin-bottom: 20px; }
	.form-grid { display: grid; grid-template-columns: repeat(2, 1fr); gap: 15px; }
	.form-group { display: flex; flex-direction: column; }
    .full-width { grid-column: 1 / -1; }
	input, select, textarea { padding: 10px; border: 1px solid #ccc; border-radius: 4px; font-size: 14px; margin-top: 5px; }
	textarea { resize: vertical; min-height: 80px; font-family: monospace; }
	button { padding: 10px 15px; border: none; border-radius: 4px; color: #fff; cursor: pointer; font-size: 14px; transition: background-color 0.2s; }
	.btn-add { background-color: #28a745; margin-top: 10px; }
	.btn-add:hover { background-color: #218838; }
	.btn-action { background-color: #007bff; }
	.btn-action:hover { background-color: #0069d9; }
	.btn-delete { background-color: #dc3545; }
	.btn-delete:hover { background-color: #c82333; }
	.task-list { margin-top: 20px; }
	.task { background: #fff; border: 1px solid #e1e4e8; padding: 15px; margin-bottom: 15px; border-radius: 8px; box-shadow: 0 1px 5px rgba(0,0,0,0.03); }
	.task-header { display: flex; justify-content: space-between; align-items: center; }
	.task-details { font-size: 14px; color: #555; margin: 10px 0; word-break: break-all; }
	.task-actions button { margin-left: 5px; }
	.logs-container { margin-top: 10px; }
	.log-entry { font-size: 13px; color: #555; border-top: 1px dashed #eee; padding-top: 10px; margin-top: 10px; }
	.log-entry:first-child { border-top: none; padding-top: 0; margin-top: 0; }
	.response-body { background-color: #f6f8fa; padding: 10px; border-radius: 4px; margin-top: 5px; white-space: pre-wrap; word-break: break-all; max-height: 200px; overflow-y: auto; font-family: monospace; }
	.tag { background-color: #eef; color: #0366d6; padding: 2px 6px; border-radius: 4px; font-size: 12px; font-weight: bold; }
</style>
</head>
<body>
<div id="app">
	<h1>定时任务管理器</h1>
	<div class="form-container">
		<h2>添加新任务</h2>
		<div class="form-grid">
			<div class="form-group">
				<label>任务名称*</label>
				<input v-model.trim="newTask.name" placeholder="例如：每日数据同步">
			</div>
			<div class="form-group">
				<label>Cron 表达式*</label>
				<input v-model.trim="newTask.cron" placeholder="例如: 0 30 1 * * * (每天1:30执行)">
			</div>
			<div class="form-group full-width">
				<label>请求地址 (URL)*</label>
				<input v-model.trim="newTask.url" placeholder="https://api.example.com/data">
			</div>
			<div class="form-group">
				<label>请求方法</label>
				<select v-model="newTask.method">
					<option>POST</option>
					<option>GET</option>
				</select>
			</div>
            <div class="form-group">
				<label>超时时间 (秒)</label>
				<input type="number" v-model.number="newTask.timeout" placeholder="默认10秒">
			</div>
			<div class="form-group full-width">
				<label>请求头 (Headers) - JSON格式</label>
				<textarea v-model="newTask.headers" placeholder='{ "Authorization": "Bearer YOUR_TOKEN" }'></textarea>
			</div>
			<div class="form-group full-width">
				<label>请求体 (Body) - 仅POST</label>
				<textarea v-model="newTask.body" placeholder='{ "key": "value", "id": 123 }'></textarea>
			</div>
		</div>
		<button @click="addTask" class="btn-add">添加任务</button>
	</div>

	<div class="task-list">
		<h2>任务列表</h2>
		<div v-for="task in tasks" :key="task.id" class="task">
			<div class="task-header">
				<h3>{{ task.name }}</h3>
				<div class="task-actions">
					<button @click="runTask(task.id)" class="btn-action">立即执行</button>
					<button @click="deleteTask(task.id)" class="btn-delete">删除</button>
				</div>
			</div>
			<div class="task-details">
				<div><span class="tag">{{ task.method }}</span> {{ task.url }}</div>
				<div><strong>Cron:</strong> {{ task.cron }}</div>
				<div><strong>下次执行时间:</strong> {{ formatTime(task.next_run) }}</div>
			</div>
			<div class="logs-container">
				<h4>最新执行结果:</h4>
				<div v-if="task.logs && task.logs.length > 0" class="log-entry">
					<div><strong>执行时间:</strong> {{ formatTime(task.logs[0].time) }}</div>
					<div><strong>执行状态:</strong> {{ task.logs[0].status_text }}</div>
					<div><strong>响应体 (Response Body):</strong></div>
					<div class="response-body">{{ task.logs[0].response_body || '(空)' }}</div>
				</div>
				<div v-else>暂无执行记录</div>
			</div>
		</div>
	</div>
</div>

<script>
const { createApp } = Vue

createApp({
	data() {
		return {
			tasks: [],
			newTask: this.getInitialNewTask(),
			intervalId: null
		}
	},
	mounted() {
		this.loadTasks()
		// 每10秒自动刷新一次列表
		this.intervalId = setInterval(this.loadTasks, 10000)
	},
	beforeUnmount() {
		clearInterval(this.intervalId)
	},
	methods: {
		getInitialNewTask() {
			return {
				name: '',
				cron: '',
				url: '',
				method: 'POST',
				headers: '{}',
				body: '{}',
				timeout: 10
			}
		},
		loadTasks() {
			axios.get('/api/tasks')
				.then(res => { this.tasks = res.data || []; })
				.catch(err => console.error("加载任务失败:", err))
		},
		addTask() {
			if (!this.newTask.name || !this.newTask.cron || !this.newTask.url) {
				return alert("请填写所有必填项 (*)")
			}
			// 校验 Headers 和 Body 是否为合法JSON
			try {
				JSON.parse(this.newTask.headers)
			} catch (e) {
				return alert("请求头 (Headers) 不是有效的JSON格式！")
			}
			if (this.newTask.method === 'POST') {
				try {
					JSON.parse(this.newTask.body)
				} catch (e) {
					return alert("请求体 (Body) 不是有效的JSON格式！")
				}
			}

			axios.post('/api/tasks', this.newTask)
				.then(() => {
					this.newTask = this.getInitialNewTask()
					this.loadTasks()
				})
				.catch(err => {
					alert("添加任务失败: " + (err.response?.data?.error || err.message))
				})
		},
		deleteTask(id) {
			if (confirm("确定要删除这个任务吗？")) {
				axios.delete('/api/tasks/' + id)
					.then(() => { this.loadTasks() })
					.catch(err => alert("删除失败: " + err.message))
			}
		},
		runTask(id) {
			axios.post('/api/tasks/' + id + '/run')
				.then(() => {
					alert("任务已提交执行，请稍后查看最新结果。")
					// 延迟一点时间再刷新，等待后台执行完成
					setTimeout(() => this.loadTasks(), 1000)
				})
				.catch(err => alert("执行失败: " + err.message))
		},
		formatTime(timeStr) {
			if (!timeStr || timeStr.startsWith("0001-01-01")) return "N/A"
			return new Date(timeStr).toLocaleString()
		}
	}
}).mount('#app')
</script>
</body>
</html>
`
