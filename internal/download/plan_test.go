package download

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ButterFuture/GoQuark/internal/api"
)

// testEntry 构造用于递归扫描测试的云盘节点
func testEntry(fid, name string, dir bool) api.FileEntry {
	return api.FileEntry{FID: fid, Name: name, Dir: dir}
}

// TestBuildPlanPreservesTree 验证混选、嵌套路径、同名文件和空文件夹
func TestBuildPlanPreservesTree(t *testing.T) {
	root := testEntry("docs", "资料", true)
	loose := testEntry("loose", "单独.txt", false)
	tree := map[string][]api.FileEntry{
		"docs":   {testEntry("readme", "说明.txt", false), testEntry("images", "图片", true), testEntry("empty", "空目录", true)},
		"images": {testEntry("image-readme", "说明.txt", false)},
	}
	plan, err := BuildPlan(context.Background(), []api.FileEntry{root, loose, root}, func(fid string, page, size int) ([]api.FileEntry, error) {
		return tree[fid], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{filepath.Join("资料", "说明.txt"), filepath.Join("资料", "图片", "说明.txt"), "单独.txt"}
	var paths []string
	for _, file := range plan.Files {
		paths = append(paths, file.RelativePath)
	}
	if !reflect.DeepEqual(paths, wantFiles) {
		t.Fatalf("文件路径 = %v，期望 %v", paths, wantFiles)
	}
	wantDirs := []string{"资料", filepath.Join("资料", "图片"), filepath.Join("资料", "空目录")}
	if !reflect.DeepEqual(plan.Directories, wantDirs) {
		t.Fatalf("目录路径 = %v，期望 %v", plan.Directories, wantDirs)
	}
	dest := t.TempDir()
	if err := plan.CreateDirectories(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	for _, dir := range wantDirs {
		if info, err := os.Stat(filepath.Join(dest, dir)); err != nil || !info.IsDir() {
			t.Fatalf("目录未创建：%s，%v", dir, err)
		}
	}
}

// TestBuildPlanPagination 验证每个目录均读取所有分页，包括恰好满页的情况
func TestBuildPlanPagination(t *testing.T) {
	for _, count := range []int{directoryPageSize, directoryPageSize + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			calls := 0
			plan, err := BuildPlan(context.Background(), []api.FileEntry{testEntry("root", "目录", true)},
				func(fid string, page, size int) ([]api.FileEntry, error) {
					calls++
					var entries []api.FileEntry
					for i := (page - 1) * size; i < min(page*size, count); i++ {
						entries = append(entries, testEntry(fmt.Sprint(i), fmt.Sprintf("%d.txt", i), false))
					}
					return entries, nil
				})
			if err != nil || len(plan.Files) != count || calls != 2 {
				t.Fatalf("文件数=%d，分页请求数=%d，错误=%v", len(plan.Files), calls, err)
			}
		})
	}
}

// TestBuildPlanRejectsInvalidTrees 验证路径碰撞、循环、重复分页和错误传播
func TestBuildPlanRejectsInvalidTrees(t *testing.T) {
	cases := []struct {
		name    string
		entries []api.FileEntry
		list    func(string, int, int) ([]api.FileEntry, error)
		want    string
	}{
		{name: "清理后重名", entries: []api.FileEntry{testEntry("a", "a/b", false), testEntry("b", "a\\b", false)}, want: "重名"},
		{name: "大小写重名", entries: []api.FileEntry{testEntry("a", "A", true), testEntry("b", "a", false)},
			list: func(string, int, int) ([]api.FileEntry, error) { return nil, nil }, want: "重名"},
		{name: "缺少FID", entries: []api.FileEntry{testEntry("", "文件", false)}, want: "FID"},
		{name: "循环目录", entries: []api.FileEntry{testEntry("root", "目录", true)},
			list: func(string, int, int) ([]api.FileEntry, error) {
				return []api.FileEntry{testEntry("root", "循环", true)}, nil
			}, want: "循环"},
		{name: "扫描失败", entries: []api.FileEntry{testEntry("root", "目录", true)},
			list: func(string, int, int) ([]api.FileEntry, error) { return nil, errors.New("读取失败") }, want: "读取目录 目录 第 1 页"},
		{name: "重复分页", entries: []api.FileEntry{testEntry("root", "目录", true)},
			list: func(string, int, int) ([]api.FileEntry, error) {
				var entries []api.FileEntry
				for i := 0; i < directoryPageSize; i++ {
					entries = append(entries, testEntry(fmt.Sprint(i), fmt.Sprint(i), false))
				}
				return entries, nil
			}, want: "重复节点"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := BuildPlan(context.Background(), tc.entries, tc.list)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误=%v，期望包含 %q", err, tc.want)
			}
			if len(plan.Files) != 0 || len(plan.Directories) != 0 {
				t.Fatal("扫描失败不应返回部分下载计划")
			}
		})
	}
}

// TestBuildPlanSanitizesComponents 验证每层独立清理路径而不是清理完整递归路径
func TestBuildPlanSanitizesComponents(t *testing.T) {
	plan, err := BuildPlan(context.Background(), []api.FileEntry{testEntry("dir", "..", true)},
		func(string, int, int) ([]api.FileEntry, error) {
			return []api.FileEntry{testEntry("file", "../../文件.txt", false)}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("download.bin", ".._.._文件.txt")
	if len(plan.Files) != 1 || plan.Files[0].RelativePath != want {
		t.Fatalf("路径未安全清理：%v", plan.Files)
	}
}

// TestBuildPlanCancellation 验证已取消的扫描不会继续请求目录
func TestBuildPlanCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := BuildPlan(ctx, []api.FileEntry{testEntry("dir", "目录", true)}, func(string, int, int) ([]api.FileEntry, error) {
		t.Fatal("取消后不应请求目录")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误=%v", err)
	}
}

// TestCreateDirectoriesRejectsOccupiedPaths 验证目标目录被文件或符号链接占用时不会覆盖或越界
func TestCreateDirectoriesRejectsOccupiedPaths(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint(symlink), func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "资料")
			var err error
			if symlink {
				err = os.Symlink(t.TempDir(), dest)
			} else {
				err = os.WriteFile(dest, []byte("保留"), 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			plan := Plan{Directories: []string{"资料", filepath.Join("资料", "子目录")}}
			if err := plan.CreateDirectories(context.Background(), root); err == nil {
				t.Fatal("应拒绝被占用的路径")
			}
		})
	}
}
