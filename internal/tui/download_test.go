package tui

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ButterFuture/GoQuark/internal/api"
	"github.com/ButterFuture/GoQuark/internal/client"
	"github.com/ButterFuture/GoQuark/internal/config"
	"github.com/ButterFuture/GoQuark/internal/download"
)

// TestSelectedDownloadEntries 验证文件夹、混选、光标回退及上级目录排除规则
func TestSelectedDownloadEntries(t *testing.T) {
	rows := []row{
		{entry: api.FileEntry{FID: "parent", Name: "..", Dir: true}},
		{entry: api.FileEntry{FID: "dir", Name: "资料", Dir: true}},
		{entry: api.FileEntry{FID: "file", Name: "说明.txt"}},
	}
	cases := []struct {
		name     string
		cursor   int
		selected map[int]bool
		want     []string
	}{
		{name: "光标文件夹", cursor: 1, want: []string{"dir"}},
		{name: "勾选文件夹优先于光标文件", cursor: 2, selected: map[int]bool{1: true}, want: []string{"dir"}},
		{name: "混选并排除上级目录", cursor: 0, selected: map[int]bool{0: true, 1: true, 2: true}, want: []string{"dir", "file"}},
		{name: "上级目录不下载", cursor: 0},
		{name: "无效勾选回退光标", cursor: 2, selected: map[int]bool{-1: true, 99: true}, want: []string{"file"}},
		{name: "越界光标", cursor: 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := model{rows: rows, cursor: tc.cursor, selected: tc.selected}
			var got []string
			for _, entry := range m.selectedDownloadEntries() {
				got = append(got, entry.FID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("选中项=%v，期望=%v", got, tc.want)
			}
		})
	}
}

// TestDoDownloadPreparesLazily 验证普通文件仍保存到根目录，且准备阶段不会请求链接
func TestDoDownloadPreparesLazily(t *testing.T) {
	cfg := &config.Config{DownloadDir: t.TempDir()}
	cl := client.New(cfg)
	mgr := download.NewManager(cl, cfg.DownloadDir)
	mgr.SetStatePath(filepath.Join(t.TempDir(), "history.json"))
	m := model{cfg: cfg, client: cl, dlMgr: mgr, rows: []row{{entry: api.FileEntry{FID: "file", Name: "说明.txt"}}}}
	next, cmd := m.doDownload()
	preparing := next.(model)
	if !preparing.dlPreparing || cmd == nil {
		t.Fatal("应异步准备下载")
	}
	defer preparing.cancelDownloadPreparation()
	if _, duplicate := preparing.doDownload(); duplicate != nil {
		t.Fatal("扫描期间不能重复提交")
	}
	msg := cmd().(dlEnqueueMsg)
	if msg.err != nil || len(msg.jobs) != 1 || msg.prepareID != preparing.dlPrepareID {
		t.Fatalf("准备结果异常：%+v", msg)
	}
	job := msg.jobs[0]
	if job.dest != filepath.Join(cfg.DownloadDir, "说明.txt") || job.fid != "file" || job.url != "" {
		t.Fatalf("下载目标或延迟解析异常：%+v", job)
	}
	preparing.cancelDownloadPreparation()
	next, _ = preparing.Update(msg)
	if len(mgr.List()) != 0 || next.(model).mode != modeBrowse {
		t.Fatal("已取消的准备结果不应入队")
	}
}

// TestDownloadPreparationFeedback 验证扫描反馈可见以及完成后解除扫描状态
func TestDownloadPreparationFeedback(t *testing.T) {
	m := model{dlPreparing: true, dlPrepareID: 1, width: 120, listH: 2}
	if !strings.Contains(m.viewBrowse(), "正在扫描文件夹") {
		t.Fatal("应显示扫描提示")
	}
	next, _ := m.Update(dlEnqueueMsg{prepareID: 1, status: "已创建 1 个空文件夹"})
	updated := next.(model)
	if updated.dlPreparing || updated.status != "已创建 1 个空文件夹" {
		t.Fatalf("扫描状态未清除：%+v", updated)
	}
}
