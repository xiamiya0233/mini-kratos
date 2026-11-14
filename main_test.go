package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	health(w, r)
	if w.Code != 200 {
		t.Errorf("状态吗：%d，期望：%d", w.Code, 200)
	}
}

func TestChainOrder(t *testing.T) {
	var got []string

	a := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = append(got, "a前")
			next.ServeHTTP(w, r)
			got = append(got, "a后")
		})
	}

	b := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = append(got, "b前")
			next.ServeHTTP(w, r)
			got = append(got, "b后")
		})
	}

	biz := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "业务")
	})

	h := Chain(a, b)(biz)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	want := []string{"a前", "b前", "业务", "b后", "a后"}
	if !slices.Equal(got, want) {
		t.Errorf("顺序：%v，期望：%v", got, want)
	}
}

func TestRecover(t *testing.T) {
	bom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("测试panic")
	})
	w := httptest.NewRecorder()
	recovered(bom).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if w.Code != 500 {
		t.Errorf("实际CODE：%d，预期CODE：%d", w.Code, 500)
	}
	e := ErrorMessage{}
	err := json.NewDecoder(w.Body).Decode(&e)
	if err != nil {
		fmt.Println("解析错误：", err)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("实际Content-Type：%v，预期Content-Type：%v", w.Header().Get("Content-Type"), "application/json")
	}
	if e.REASON != "INTERNAL" {
		t.Errorf("实际REASON：%v，预期REASON：%v", e.REASON, "INTERNAL")
	}

	w2 := httptest.NewRecorder()
	recovered(http.HandlerFunc(health)).ServeHTTP(w2, httptest.NewRequest("GET", "/healthz", nil))
	if w2.Code != 200 {
		t.Errorf("实际CODE：%d，预期CODE：%d", w2.Code, 200)
	}
}

func TestReconfig(t *testing.T) {
	tests := []struct {
		name     string
		address  string // 环境变量 HTTP_ADDR 的值，空表示不设置
		duration string // 环境变量 SHUTDOWN_TIMEOUT 的值，空表示不设置
		wantErr  bool
		wantAddr string
		wantDur  time.Duration
	}{
		{"默认值", "", "", false, ":8080", 5 * time.Second},
		{"自定义", ":9999", "3s", false, ":9999", 3 * time.Second},
		{"不是时长", ":9090", "a", true, "", 0},
		{"缺单位", ":9091", "5", true, "", 0},
		{"零", ":9092", "0s", true, "", 0},
		{"负数", ":9093", "-1s", true, "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HTTP_ADDR", tt.address)
			t.Setenv("SHUTDOWN_TIMEOUT", tt.duration)

			cfg, err := Reconfig()

			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望出错，实际没有错误，配置为 %+v", cfg)
				}
				// 错误信息要带变量名和原值，方便看日志的人知道改哪里
				if !strings.Contains(err.Error(), "SHUTDOWN_TIMEOUT") || !strings.Contains(err.Error(), tt.duration) {
					t.Errorf("错误信息应包含变量名和原值 %q，实际：%v", tt.duration, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不期望出错，实际：%v", err)
			}
			if cfg.address != tt.wantAddr || cfg.duration != tt.wantDur {
				t.Errorf("实际 %+v，预期地址 %q、时长 %v", cfg, tt.wantAddr, tt.wantDur)
			}
		})
	}
}

type fakeServer struct {
	startErr error
	stopped  bool
}

func (s *fakeServer) Start(ctx context.Context) error {
	return s.startErr
}

func (s *fakeServer) Stop(ctx context.Context) error {
	s.stopped = true
	return nil
}

func TestAppRunStartError(t *testing.T) {
	want := errors.New("启动失败")
	f := &fakeServer{startErr: want}
	app := AppServer(time.Second, f)

	got := app.Run()

	if !errors.Is(got, want) {
		t.Errorf("Run 应返回启动错误 %v，实际：%v", want, got)
	}
	if !f.stopped {
		t.Errorf("启动失败后 Run 应调用服务的 Stop，实际没有调用")
	}
}
