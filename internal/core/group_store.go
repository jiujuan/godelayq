package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultGroupsPath 是分组文件的约定位置。
const DefaultGroupsPath = "./data/groups.json"

// 分组名会直接出现在 URL 段与查询参数里，因此只允许无歧义的字符。
// 允许空格或中文会让 "?group=运维 组" 这类值在编码/解码环节出岔子，
// 也让"两个只差大小写的组"难以辨认。
var groupNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrGroupNameInvalid 表示分组名不合规则。
var ErrGroupNameInvalid = errors.New("invalid group name")

// ErrGroupColorInvalid 表示颜色不是 #rgb/#rrggbb。
var ErrGroupColorInvalid = errors.New("invalid group color")

// ErrGroupNotFound 表示指定分组不存在。
var ErrGroupNotFound = errors.New("group not found")

// Group 是一个任务分组的元数据。
//
// Name 既是主键也是 Job.Group 的取值，所以改名要同步改写任务（由调用方决定策略）。
type Group struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Color       string    `json:"color,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// GroupStore 保存分组元数据。
//
// 刻意不含 Flush/Close：分组是低频实体，每次改动同步落盘即可，
// 没有后台合并协程需要收尾。jobs 那套合并写盘是为高频状态迁移准备的，
// 照搬过来只会多出一个"崩溃丢最后一次改动"的窗口。
type GroupStore interface {
	// List 按名称字典序返回全部分组，顺序稳定以便直接铺到 UI 上
	List() ([]Group, error)
	// Get 忽略大小写查找一个分组
	Get(name string) (Group, bool, error)
	// Save 新建或整体覆盖一个分组；已存在时保留其 CreatedAt
	Save(group Group) error
	// Delete 忽略大小写删除；不存在返回 ErrGroupNotFound
	Delete(name string) error
}

// ValidateGroupName 检查分组名是否合规，错误信息里带上可接受的形状。
func ValidateGroupName(name string) error {
	if !groupNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q, expected 1-64 characters of [A-Za-z0-9_-]", ErrGroupNameInvalid, name)
	}
	return nil
}

// ValidateGroupColor 检查颜色取值；空串表示"没配色"，同样合法。
// 单独导出是给 HTTP 层在写盘之前先拒掉非法输入用（存进去的坏颜色会渲染成不可见样式）。
func ValidateGroupColor(value string) error {
	if value == "" || isHexColor(value) {
		return nil
	}
	return fmt.Errorf("%w: %q, expected #rgb or #rrggbb", ErrGroupColorInvalid, value)
}

// 编译期钉住接口实现：GroupStore 的方法集一旦被改动，这里立刻报错，
// 而不是等到 HTTP 层接线时才发现漏了方法。
var _ GroupStore = (*JSONFileGroupStore)(nil)

// JSONFileGroupStore 是把分组存成单个 JSON 文件的 GroupStore。
type JSONFileGroupStore struct {
	path string

	mu     sync.Mutex
	groups map[string]Group // key 为小写名称，值保留用户书写的大小写
}

// NewJSONFileGroupStore 打开（必要时创建）分组文件。文件不存在视为空分组集合，
// 而不是错误——首次启动不该因为没人预先建文件而失败。
func NewJSONFileGroupStore(path string) (*JSONFileGroupStore, error) {
	if path == "" {
		path = DefaultGroupsPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create group store directory failed: %w", err)
	}

	store := &JSONFileGroupStore{
		path:   path,
		groups: make(map[string]Group),
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

// load 读取磁盘内容；空文件与缺失文件都当作空集合。
func (s *JSONFileGroupStore) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read group file failed: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}

	var groups []Group
	if err := json.Unmarshal(data, &groups); err != nil {
		return fmt.Errorf("parse group file failed: %w", err)
	}
	for _, group := range groups {
		s.groups[strings.ToLower(group.Name)] = group
	}
	return nil
}

func (s *JSONFileGroupStore) List() ([]Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	items := make([]Group, 0, len(s.groups))
	for _, group := range s.groups {
		items = append(items, group)
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items, nil
}

func (s *JSONFileGroupStore) Get(name string) (Group, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	group, ok := s.groups[strings.ToLower(name)]
	return group, ok, nil
}

func (s *JSONFileGroupStore) Save(group Group) error {
	if err := ValidateGroupName(group.Name); err != nil {
		return err
	}
	// 颜色给前端色板用，格式错了会渲染成不可见的样式，宁可在写入时就拒绝
	if err := ValidateGroupColor(group.Color); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := strings.ToLower(group.Name)
	now := time.Now()
	group.UpdatedAt = now
	if existing, ok := s.groups[key]; ok {
		// 覆盖式更新不该把创建时间冲掉，除非调用方显式带了更早的值
		if group.CreatedAt.IsZero() {
			group.CreatedAt = existing.CreatedAt
		}
	} else {
		if group.CreatedAt.IsZero() {
			group.CreatedAt = now
		}
	}

	s.groups[key] = group
	return s.flushLocked()
}

func (s *JSONFileGroupStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := strings.ToLower(name)
	if _, ok := s.groups[key]; !ok {
		return ErrGroupNotFound
	}
	delete(s.groups, key)
	return s.flushLocked()
}

// flushLocked 原子重写整个分组文件。调用者需持有 s.mu。
//
// 低频实体直接全量重写：与 jobs 快照同源的"临时文件 + rename"保证读者
// 不会看到写了一半的文件。
func (s *JSONFileGroupStore) flushLocked() error {
	items := make([]Group, 0, len(s.groups))
	for _, group := range s.groups {
		items = append(items, group)
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	return nil
}

// isHexColor 接受 #rgb 与 #rrggbb 两种写法，与前端色板保持一致。
func isHexColor(value string) bool {
	if !strings.HasPrefix(value, "#") {
		return false
	}

	digits := value[1:]
	if len(digits) == 3 {
		// #abc 展开成 #aabbcc，避免前端拿到两种格式各写一套解析
		digits = string([]byte{digits[0], digits[0], digits[1], digits[1], digits[2], digits[2]})
	}
	return len(digits) == 6 && hexDigitsPattern.MatchString(digits)
}

var hexDigitsPattern = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)
