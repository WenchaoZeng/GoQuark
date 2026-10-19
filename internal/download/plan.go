package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ButterFuture/GoQuark/internal/api"
)

const directoryPageSize = 200

// PlannedFile 保存云盘文件及其相对于下载根目录的安全路径
type PlannedFile struct {
	Entry        api.FileEntry
	RelativePath string
}

// Plan 包含文件下载项和需要保留的目录（包括空目录）
type Plan struct {
	Files       []PlannedFile
	Directories []string
}

type planBuilder struct {
	ctx     context.Context
	list    func(string, int, int) ([]api.FileEntry, error)
	plan    Plan
	paths   map[string]api.FileEntry
	parents map[string]bool
}

// BuildPlan 分页遍历所选文件和文件夹并保留所选文件夹自身的名称
// list 由调用方提供，便于不同入口复用以及离线测试
func BuildPlan(ctx context.Context, entries []api.FileEntry, list func(string, int, int) ([]api.FileEntry, error)) (Plan, error) {
	b := planBuilder{ctx: ctx, list: list, paths: make(map[string]api.FileEntry), parents: make(map[string]bool)}
	for _, entry := range entries {
		if err := b.add(entry, ""); err != nil {
			return Plan{}, err
		}
	}
	return b.plan, nil
}

// add 清理单个路径分量，拒绝循环目录和清理后重名的不同云盘节点
func (b *planBuilder) add(entry api.FileEntry, parent string) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if entry.FID == "" {
		return fmt.Errorf("文件缺少 FID：%s", entry.Name)
	}
	if entry.Dir && b.parents[entry.FID] {
		return fmt.Errorf("检测到循环目录：%s", entry.Name)
	}
	rel := filepath.Join(parent, SanitizeFilename(entry.Name))
	// 按不区分大小写检查，避免 macOS/Windows 上的路径碰撞
	key := strings.ToLower(rel)
	if previous, ok := b.paths[key]; ok {
		if previous.FID == entry.FID && previous.Dir == entry.Dir {
			return nil
		}
		return fmt.Errorf("下载路径重名：%s（%s / %s）", rel, previous.Name, entry.Name)
	}
	b.paths[key] = entry
	if !entry.Dir {
		b.plan.Files = append(b.plan.Files, PlannedFile{Entry: entry, RelativePath: rel})
		return nil
	}
	b.plan.Directories = append(b.plan.Directories, rel)
	b.parents[entry.FID] = true
	defer delete(b.parents, entry.FID)
	return b.walk(entry.FID, rel)
}

// walk 顺序获取目录的所有分页，检测重复分页以防接口异常导致无限扫描
func (b *planBuilder) walk(fid, parent string) error {
	seen := make(map[string]bool)
	for page := 1; ; page++ {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		entries, err := b.list(fid, page, directoryPageSize)
		if err != nil {
			return fmt.Errorf("读取目录 %s 第 %d 页：%w", parent, page, err)
		}
		for _, entry := range entries {
			if seen[entry.FID] {
				return fmt.Errorf("目录分页包含重复节点：%s / %s", parent, entry.Name)
			}
			seen[entry.FID] = true
			if err := b.add(entry, parent); err != nil {
				return err
			}
		}
		if len(entries) < directoryPageSize {
			return nil
		}
	}
}

// CreateDirectories 按父子顺序创建目录，拒绝嵌套符号链接以防写出下载根目录
func (p Plan) CreateDirectories(ctx context.Context, root string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("创建下载根目录：%w", err)
	}
	for _, rel := range p.Directories {
		if err := ctx.Err(); err != nil {
			return err
		}
		dest := filepath.Join(root, rel)
		info, err := os.Lstat(dest)
		if os.IsNotExist(err) {
			if err := os.Mkdir(dest, 0o755); err != nil {
				return fmt.Errorf("创建目录 %s：%w", dest, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("检查目录 %s：%w", dest, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("目标目录被文件或符号链接占用：%s", dest)
		}
	}
	return nil
}
