package download

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ButterFuture/GoQuark/internal/api"
	"github.com/ButterFuture/GoQuark/internal/client"
	"github.com/ButterFuture/GoQuark/internal/config"
)

// newTestManager 创建不读写用户下载历史的测试管理器
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(client.New(&config.Config{}), t.TempDir())
	m.statePath = ""
	m.loaded = true
	return m
}

// waitTask 等待下载协程结束，同时防止调度错误导致测试无限挂起
func waitTask(t *testing.T, task *Task) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		task.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("任务未结束：%s", task.ID)
	}
}

// waitStatus 等待异步暂停收尾完成，不将协程退出误认为状态已经更新
func waitStatus(t *testing.T, m *Manager, task *Task, want Status) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		if snapshot := m.FindByFIDAtDest(task.FID, task.Dest); snapshot != nil && snapshot.Status == want {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("任务未到达状态 %s：%+v", want, m.FindByFIDAtDest(task.FID, task.Dest))
		}
	}
}

// TestManagerLimitsConcurrentFiles 验证最多三个文件运行以及排队任务的暂停、恢复和取消
func TestManagerLimitsConcurrentFiles(t *testing.T) {
	m := newTestManager(t)
	started := make(chan string, 32)
	release := make(chan struct{})
	m.ResolveURL = func(fid string) (string, int64, error) {
		started <- fid
		<-release
		return "", 0, errors.New("测试停止")
	}
	t.Cleanup(func() {
		close(release)
		m.PauseAllWait()
	})
	var tasks []*Task
	for i := 0; i < 8; i++ {
		task, err := m.EnqueueEx(EnqueueOpts{Name: fmt.Sprintf("%d.txt", i), FID: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("未启动三个下载任务")
		}
	}
	var queued []*Task
	running := 0
	for _, snapshot := range m.List() {
		if snapshot.Status == StatusRunning {
			running++
		}
		if snapshot.Status == StatusQueued {
			for _, task := range tasks {
				if task.ID == snapshot.ID {
					queued = append(queued, task)
				}
			}
		}
	}
	if running != 3 || len(queued) != 5 {
		t.Fatalf("运行=%d，排队=%d，期望 3/5", running, len(queued))
	}
	if !m.PauseID(queued[0].ID) {
		t.Fatal("暂停排队任务失败")
	}
	waitTask(t, queued[0])
	waitStatus(t, m, queued[0], StatusPaused)
	if !m.ResumeID(queued[0].ID) {
		t.Fatal("恢复排队任务失败")
	}
	if snapshot := m.FindByFIDAtDest(queued[0].FID, queued[0].Dest); snapshot.Status != StatusQueued {
		t.Fatalf("恢复后应排队，实际=%s", snapshot.Status)
	}
	if !m.CancelID(queued[1].ID) {
		t.Fatal("取消排队任务失败")
	}
	waitTask(t, queued[1])
	if snapshot := m.FindByFIDAtDest(queued[1].FID, queued[1].Dest); snapshot.Status != StatusCancel {
		t.Fatalf("取消状态=%s", snapshot.Status)
	}
	select {
	case fid := <-started:
		t.Fatalf("排队任务不应提前请求下载链接：%s", fid)
	default:
	}
}

// TestPlanDownloadsNestedFiles 验证扫描计划可复用已有下载器并实际写入正确的嵌套路径
func TestPlanDownloadsNestedFiles(t *testing.T) {
	const content = "递归下载内容"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(content))
		}
	}))
	defer server.Close()
	plan, err := BuildPlan(context.Background(), []api.FileEntry{testEntry("root", "资料", true)},
		func(fid string, page, size int) ([]api.FileEntry, error) {
			if fid == "root" {
				return []api.FileEntry{testEntry("first", "说明.txt", false), testEntry("sub", "子目录", true)}, nil
			}
			entries := []api.FileEntry{testEntry("second", "说明.txt", false)}
			for i := 0; i < 6; i++ {
				entries = append(entries, testEntry(fmt.Sprintf("file-%d", i), fmt.Sprintf("文件-%d.txt", i), false))
			}
			return entries, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t)
	if err := plan.CreateDirectories(context.Background(), m.DestDir()); err != nil {
		t.Fatal(err)
	}
	var resolutions atomic.Int32
	m.ResolveURL = func(string) (string, int64, error) {
		resolutions.Add(1)
		return server.URL, int64(len(content)), nil
	}
	var tasks []*Task
	for _, file := range plan.Files {
		task, err := m.EnqueueEx(EnqueueOpts{
			Name: file.RelativePath, FID: file.Entry.FID, Dest: filepath.Join(m.DestDir(), file.RelativePath),
		})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	for _, task := range tasks {
		waitTask(t, task)
		data, err := os.ReadFile(task.Dest)
		if err != nil || string(data) != content {
			t.Fatalf("文件内容不匹配：%s，%q，%v", task.Dest, data, err)
		}
		if snapshot := m.FindByFIDAtDest(task.FID, task.Dest); snapshot.Status != StatusDone {
			t.Fatalf("任务状态=%s，错误=%s", snapshot.Status, snapshot.Error)
		}
	}
	if int(resolutions.Load()) != len(plan.Files) {
		t.Fatalf("解析链接次数=%d", resolutions.Load())
	}
}

// TestManagerBulkResumeLimits 验证手动批量恢复与启动恢复不会绕过三文件并发限制
func TestManagerBulkResumeLimits(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint(all), func(t *testing.T) {
			m := newTestManager(t)
			started := make(chan string, 32)
			release := make(chan struct{})
			m.ResolveURL = func(fid string) (string, int64, error) {
				started <- fid
				<-release
				return "", 0, errors.New("测试停止")
			}
			t.Cleanup(func() {
				close(release)
				m.PauseAllWait()
			})
			for i := 0; i < 7; i++ {
				m.tasks = append(m.tasks, &Task{
					ID: fmt.Sprint(i), FID: fmt.Sprint(i), Dest: filepath.Join(m.DestDir(), fmt.Sprint(i)),
					Status: StatusPaused, AutoPausedByQuit: i < 5,
				})
			}
			want := 5
			n := 0
			if all {
				want = 7
				n = m.ResumePaused()
			} else {
				n = m.ResumeQuitPaused()
			}
			if n != want {
				t.Fatalf("恢复数=%d，期望=%d", n, want)
			}
			for i := 0; i < 3; i++ {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("恢复任务未启动")
				}
			}
			counts := make(map[Status]int)
			for _, snapshot := range m.List() {
				counts[snapshot.Status]++
			}
			if counts[StatusRunning] != 3 || counts[StatusQueued] != want-3 || counts[StatusPaused] != 7-want {
				t.Fatalf("恢复后的状态分布异常：%v", counts)
			}
			select {
			case fid := <-started:
				t.Fatalf("超过并发限制：%s", fid)
			default:
			}
		})
	}
}

// TestManagerRejectsActiveDestination 验证不同云盘文件不能同时写入相同目标
func TestManagerRejectsActiveDestination(t *testing.T) {
	m := newTestManager(t)
	dest := filepath.Join(m.DestDir(), "资料", "说明.txt")
	m.tasks = []*Task{{FID: "other", Dest: dest, Status: StatusQueued}}
	if active := m.FindActiveAtDest(dest); active == nil || active.FID != "other" {
		t.Fatalf("未识别占用路径的任务：%v", active)
	}
	_, err := m.EnqueueEx(EnqueueOpts{FID: "new", Dest: dest})
	if err == nil || !strings.Contains(err.Error(), "已有下载任务") {
		t.Fatalf("应拒绝重复写入：%v", err)
	}
}

// TestFindByFIDAtDest 验证去重使用完整路径，并优先匹配活动任务
func TestFindByFIDAtDest(t *testing.T) {
	m := newTestManager(t)
	a := filepath.Join(m.DestDir(), "资料", "a.txt")
	b := filepath.Join(m.DestDir(), "其他", "a.txt")
	m.tasks = []*Task{
		{ID: "a-done", FID: "fid", Dest: a, Status: StatusDone},
		{ID: "a-active", FID: "fid", Dest: a, Status: StatusQueued},
		{ID: "b-done", FID: "fid", Dest: b, Status: StatusDone},
	}
	if snapshot := m.FindByFIDAtDest("fid", a); snapshot == nil || snapshot.ID != "a-active" {
		t.Fatalf("未匹配活动任务：%v", snapshot)
	}
	if snapshot := m.FindByFIDAtDest("fid", b); snapshot == nil || snapshot.ID != "b-done" {
		t.Fatalf("不同路径误去重：%v", snapshot)
	}
	if snapshot := m.FindByFIDAtDest("fid", filepath.Join(m.DestDir(), "a.txt")); snapshot != nil {
		t.Fatalf("未下载路径不应匹配：%v", snapshot)
	}
}

// TestManagerReportsResolutionError 验证延迟解析下载链接失败时保留真实原因
func TestManagerReportsResolutionError(t *testing.T) {
	m := newTestManager(t)
	m.ResolveURL = func(string) (string, int64, error) { return "", 0, errors.New("登录已过期") }
	task, err := m.EnqueueEx(EnqueueOpts{Name: "文件", FID: "fid"})
	if err != nil {
		t.Fatal(err)
	}
	waitTask(t, task)
	snapshot := m.FindByFIDAtDest("fid", task.Dest)
	if snapshot.Status != StatusError || !strings.Contains(snapshot.Error, "登录已过期") {
		t.Fatalf("错误未保留：%+v", snapshot)
	}
}
