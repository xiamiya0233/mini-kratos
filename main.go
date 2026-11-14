package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Task struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

type Statu struct {
	Status string `json:"status"`
}

type Store struct {
	mu   sync.Mutex
	ID   int64
	Body map[int64]Task
}

type LigteTitle struct {
	Title string
}

type ErrorMessage struct {
	CODE    int64  `json:"code"`
	REASON  string `json:"reason"`
	MESSAGE string `json:"message"`
}

type notelog struct {
	http.ResponseWriter
	status int
}

type AppError struct {
	Reason  string
	Message string
}

type config struct {
	address  string
	duration time.Duration
}

type httpServer struct {
	server *http.Server
}

type App struct {
	server   []Server
	duration time.Duration
}

type Middleware func(http.Handler) http.Handler

type Server interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

var lib = Store{Body: make(map[int64]Task)}

func tasks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var task Task
	var id int64
	var err error

	id, err = strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, &AppError{
			Reason:  "INVALID_ARGUMENT",
			Message: "id必须为正整数",
		})
		return
	}

	task, err = lib.Get(id)
	if err != nil {
		writeError(w, err)
		return
	}

	json.NewEncoder(w).Encode(task)

	fmt.Fprintln(w)
}

func hello(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "这是我参考kratos弄的 mini-kratos")
}

func debugpanic(w http.ResponseWriter, r *http.Request) {
	panic("测试panic")
}

func debugslow(w http.ResponseWriter, r *http.Request) {
	if qurey := r.URL.Query().Get("d"); qurey != "" {
		duration, err := time.ParseDuration(qurey)
		if err != nil || duration <= 0 {
			writeError(w, &AppError{
				"INVALID_ARGUMENT",
				"查询参数解析失败",
			})
			return
		}

		time.Sleep(duration)
	} else {
		time.Sleep(3 * time.Second)
	}

	fmt.Fprintln(w, "slow done")
}

func health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var status Statu
	status.Status = "ok"

	json.NewEncoder(w).Encode(status)
	fmt.Fprintln(w)
}

func task2(w http.ResponseWriter, r *http.Request) {
	var err error

	w.Header().Set("Content-Type", "application/json")
	var title LigteTitle

	err = json.NewDecoder(r.Body).Decode(&title)
	if err != nil {
		writeError(w, &AppError{
			Reason:  "INVALID_ARGUMENT",
			Message: "请求体不是合法的 JSON",
		})
		return
	}

	titles, err := lib.Create(title.Title)
	if err != nil {
		writeError(w, err)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(titles)

	fmt.Fprintln(w)
}

func logged(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &notelog{ResponseWriter: w, status: 200}
			next.ServeHTTP(rec, r)
			slog.Info("消息", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
		})
}

func recovered(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("消息", "panic", err, "path", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				writeError(w, &AppError{
					Reason:  "INTERNAL",
					Message: "服务器内部错误",
				})
				return
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func Chain(ms ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		h := next
		for i := len(ms) - 1; i >= 0; i-- {
			h = ms[i](h)
		}
		return h
	}
}

func writeError(w http.ResponseWriter, err error) {
	var status int
	w.Header().Set("Content-Type", "application/json")
	ae, ok := err.(*AppError)
	if !ok {
		status = 500
		ae = &AppError{Reason: "INTERNAL", Message: "服务器内部错误"}
	}

	switch ae.Reason {
	case "NOT_FOUND":
		status = http.StatusNotFound
	case "INVALID_ARGUMENT":
		status = http.StatusBadRequest
	case "INTERNAL":
		status = http.StatusInternalServerError
	default:
		status = http.StatusInternalServerError
	}
	w.WriteHeader(status)

	s := ErrorMessage{CODE: int64(status), REASON: ae.Reason, MESSAGE: ae.Message}
	json.NewEncoder(w).Encode(s)
	fmt.Fprintln(w)
}

// Run 启动全部服务，然后一直等：要么收到退出信号，要么某个服务的 Start 返回了。
// 不管是哪一种，最后都用同一个时间预算把所有服务关掉。
func (a *App) Run() error {
	ch := make(chan os.Signal, 1)
	// 每个服务的 Start 结果都会送进来（nil 也送），容量等于服务个数，
	// 这样即使 Run 已经不再接收，goroutine 也不会卡在发送上。
	errch := make(chan error, len(a.server))

	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(ch)

	for _, s := range a.server {
		go func() {
			errch <- s.Start(context.Background())
		}()
	}

	var startErr error
	select {
	case sig := <-ch:
		slog.Info("收到退出的信号，开始关闭", "signal", sig)
	case startErr = <-errch:
	}

	ctx, cancel := context.WithTimeout(context.Background(), a.duration)
	defer cancel()
	stopErr := a.Stop(ctx)

	// 启动错误优先：它才是程序退出的原因
	if startErr != nil {
		return startErr
	}
	return stopErr
}

// Stop 把每个服务都关一遍，返回遇到的第一个错误；中途出错也继续关后面的。
func (a *App) Stop(ctx context.Context) error {
	var first error
	for _, s := range a.server {
		if err := s.Stop(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func Reconfig() (config, error) {
	HTTP_ADDR := os.Getenv("HTTP_ADDR")
	if HTTP_ADDR == "" {
		HTTP_ADDR = ":8080"
	}

	// 只有"没设置"才用默认值；写了 0s 属于写错，要报错而不是悄悄改成 5s
	SHUTDOWN_TIMEOUT := os.Getenv("SHUTDOWN_TIMEOUT")
	if SHUTDOWN_TIMEOUT == "" {
		SHUTDOWN_TIMEOUT = "5s"
	}
	val, err := time.ParseDuration(SHUTDOWN_TIMEOUT)
	if err != nil {
		return config{}, fmt.Errorf("SHUTDOWN_TIMEOUT=%q 不是合法时长: %w", SHUTDOWN_TIMEOUT, err)
	}
	if val <= 0 {
		return config{}, fmt.Errorf("SHUTDOWN_TIMEOUT=%q 必须大于 0", SHUTDOWN_TIMEOUT)
	}

	return config{
		address:  HTTP_ADDR,
		duration: val,
	}, nil
}

func AppServer(duration time.Duration, sr ...Server) *App {
	return &App{
		duration: duration,
		server:   sr,
	}
}

func (s *Store) Create(title string) (Task, error) {
	var task Task
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(title) == "" {
		return Task{}, &AppError{
			Reason:  "INVALID_ARGUMENT",
			Message: "标题不能为空",
		}
	}

	s.ID += 1
	task.ID = s.ID
	task.Title = title
	s.Body[task.ID] = task

	return s.Body[task.ID], nil
}

func (s *Store) Get(id int64) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.Body[id]
	if !ok {
		return Task{}, &AppError{
			Reason:  "NOT_FOUND",
			Message: "不存在数据",
		}
	}

	return task, nil
}

func (rec *notelog) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

func (e *AppError) Error() string { return e.Message }

func (e *httpServer) Start(ctx context.Context) error {
	err := e.server.ListenAndServe()
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (e *httpServer) Stop(ctx context.Context) error {
	err := e.server.Shutdown(ctx)
	if err != nil {
		e.server.Close()
		return err
	}
	return err
}

func main() {
	cfg, errcfg := Reconfig()
	if errcfg != nil {
		slog.Error("错误信息：", "err", errcfg)
		os.Exit(1)
	}

	http.HandleFunc("/hello", hello)
	http.HandleFunc("/healthz", health)
	http.HandleFunc("GET /tasks/{id}", tasks)
	http.HandleFunc("GET /debug/panic", debugpanic)
	http.HandleFunc("POST /tasks", task2)
	http.HandleFunc("GET /debug/slow", debugslow)

	slog.Info("服务启动", "监听地址", cfg.address, "关闭预算", cfg.duration)

	srv := &http.Server{
		Addr:    cfg.address,
		Handler: Chain(logged, recovered)(http.DefaultServeMux),
	}

	app := AppServer(cfg.duration, &httpServer{server: srv})
	if err := app.Run(); err != nil {
		slog.Error("服务退出", "err", err)
		os.Exit(1)
	}
}
