package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newGroupStore(t *testing.T) (*JSONFileGroupStore, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "groups.json")
	store, err := NewJSONFileGroupStore(path)
	if err != nil {
		t.Fatalf("创建分组存储失败: %v", err)
	}
	return store, path
}

func TestGroupStore_SaveListGet(t *testing.T) {
	store, _ := newGroupStore(t)

	if err := store.Save(Group{Name: "billing", Description: "账务组", Color: "#2563EB"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if err := store.Save(Group{Name: "nightly_ops"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("应有 2 个分组，实际 %d", len(items))
	}
	// 顺序必须是稳定的字典序，否则每次刷新列表都在换位置
	if items[0].Name != "billing" || items[1].Name != "nightly_ops" {
		t.Errorf("List 应按名称排序，实际 %s, %s", items[0].Name, items[1].Name)
	}
	if items[0].Color != "#2563EB" {
		t.Errorf("颜色应原样保存，实际 %q", items[0].Color)
	}
	if items[0].CreatedAt.IsZero() || items[0].UpdatedAt.IsZero() {
		t.Error("Save 应补齐创建与更新时间")
	}

	// 名称查找忽略大小写：URL 里手打大小写不该造出第二个组
	group, ok, err := store.Get("Billing")
	if err != nil || !ok {
		t.Fatalf("忽略大小写查找失败: ok=%v err=%v", ok, err)
	}
	if group.Name != "billing" {
		t.Errorf("应返回原书写名称，实际 %q", group.Name)
	}

	if _, ok, _ := store.Get("missing"); ok {
		t.Error("不存在的分组不该查得到")
	}
}

func TestGroupStore_RejectsBadNames(t *testing.T) {
	store, _ := newGroupStore(t)

	for _, name := range []string{"", "has space", "中文组", "slash/es", "dot.name", "#hash", strings.Repeat("a", 65)} {
		err := store.Save(Group{Name: name})
		if !errors.Is(err, ErrGroupNameInvalid) {
			t.Errorf("分组名 %q 应被拒绝，实际返回 %v", name, err)
		}
	}

	// 边界：1 与 64 字符都合法
	if err := store.Save(Group{Name: "a"}); err != nil {
		t.Errorf("单字符名称应合法: %v", err)
	}
	if err := store.Save(Group{Name: strings.Repeat("a", 64)}); err != nil {
		t.Errorf("64 字符名称应合法: %v", err)
	}
}

func TestGroupStore_RejectsBadColors(t *testing.T) {
	store, _ := newGroupStore(t)

	for _, color := range []string{"red", "2563EB", "#12345", "#GGGGGG", "#2563EBextra"} {
		err := store.Save(Group{Name: "colors", Color: color})
		if err == nil {
			t.Fatalf("颜色 %q 应被拒绝", color)
		}
		// HTTP 层按这个哨兵区分 400 与 500，错误必须是它而不是随手 fmt 出来的串
		if !errors.Is(err, ErrGroupColorInvalid) {
			t.Errorf("颜色 %q 的错误应能识别为 ErrGroupColorInvalid，实际 %v", color, err)
		}
	}

	// 两种写法都收，#rgb 由前端归一化即可
	if err := store.Save(Group{Name: "colors", Color: "#abc"}); err != nil {
		t.Errorf("#abc 应合法: %v", err)
	}
	if err := store.Save(Group{Name: "colors", Color: "#2563EB"}); err != nil {
		t.Errorf("#2563EB 应合法: %v", err)
	}
	// 颜色可选：留空表示"用主题默认色"
	if err := store.Save(Group{Name: "plain"}); err != nil {
		t.Errorf("不带颜色应合法: %v", err)
	}
}

func TestGroupStore_UpdateKeepsCreatedAt(t *testing.T) {
	store, _ := newGroupStore(t)

	original := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	if err := store.Save(Group{Name: "ops", CreatedAt: original, Description: "第一版"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	// 覆盖式更新：调用方通常只带新描述，不该顺手把创建时间冲成 now
	if err := store.Save(Group{Name: "ops", Description: "第二版"}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	group, ok, err := store.Get("ops")
	if err != nil || !ok {
		t.Fatalf("查找失败: ok=%v err=%v", ok, err)
	}
	if !group.CreatedAt.Equal(original) {
		t.Errorf("CreatedAt 应保留 %v，实际 %v", original, group.CreatedAt)
	}
	if group.Description != "第二版" {
		t.Errorf("描述应被覆盖，实际 %q", group.Description)
	}
	if group.UpdatedAt.Before(original) {
		t.Errorf("UpdatedAt 应前进，实际 %v", group.UpdatedAt)
	}
}

func TestGroupStore_Delete(t *testing.T) {
	store, _ := newGroupStore(t)

	if err := store.Save(Group{Name: "temp"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	// 删除忽略大小写
	if err := store.Delete("TEMP"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if _, ok, _ := store.Get("temp"); ok {
		t.Error("删除后不该还查得到")
	}
	if err := store.Delete("temp"); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("重复删除应返回 ErrGroupNotFound，实际 %v", err)
	}
}

func TestGroupStore_PersistsAcrossReopen(t *testing.T) {
	store, path := newGroupStore(t)

	if err := store.Save(Group{Name: "billing", Description: "账务"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if err := store.Save(Group{Name: "nightly"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	reopened, err := NewJSONFileGroupStore(path)
	if err != nil {
		t.Fatalf("重开分组文件失败: %v", err)
	}
	items, err := reopened.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("重开后应有 2 个分组，实际 %d", len(items))
	}

	// 文件形状要人类可读（数组 + 缩进），便于 diff 与手工修
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取分组文件失败: %v", err)
	}
	var decoded []Group
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("分组文件应是 JSON 数组: %v", err)
	}
	if !strings.Contains(string(data), "\n  {") {
		t.Errorf("分组文件应缩进输出，实际 %s", data)
	}
}

func TestGroupStore_MissingFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "groups.json")

	// 首次启动不该因为目录/文件不存在而失败，但目录要替它建好
	store, err := NewJSONFileGroupStore(path)
	if err != nil {
		t.Fatalf("新建分组存储失败: %v", err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("新存储应为空，实际 %d 条", len(items))
	}
	if err := store.Save(Group{Name: "first"}); err != nil {
		t.Fatalf("首次 Save 失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Save 后文件应存在: %v", err)
	}
}

func TestGroupStore_EmptyFileAndCorruptFile(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte("   \n"), 0644); err != nil {
		t.Fatalf("预置空文件失败: %v", err)
	}
	if store, err := NewJSONFileGroupStore(empty); err != nil {
		t.Errorf("空白文件应视为空集合，实际 %v", err)
	} else if items, _ := store.List(); len(items) != 0 {
		t.Errorf("空白文件应得到空集合，实际 %d 条", len(items))
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0644); err != nil {
		t.Fatalf("预置损坏文件失败: %v", err)
	}
	if _, err := NewJSONFileGroupStore(corrupt); err == nil {
		t.Error("损坏的分组文件必须报错，而不是静默当成空集合把数据覆盖掉")
	}
}

func TestGroupStore_DefaultPathWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("读取工作目录失败: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换工作目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	store, err := NewJSONFileGroupStore("")
	if err != nil {
		t.Fatalf("空路径应回落到默认位置: %v", err)
	}
	if filepath.Base(store.path) != filepath.Base(DefaultGroupsPath) {
		t.Errorf("默认文件名应为 %s，实际 %s", DefaultGroupsPath, store.path)
	}
}

// 分组读写都走同一把锁：并发下不该出现数据竞争或半写文件。
func TestGroupStore_ConcurrentSaveAndDelete(t *testing.T) {
	store, path := newGroupStore(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("group-%02d", i%7)
			if err := store.Save(Group{Name: name, Description: fmt.Sprintf("第 %d 次", i)}); err != nil {
				t.Errorf("并发 Save 失败: %v", err)
			}
			if i%3 == 0 {
				// 忽略 ErrGroupNotFound：并发下删除本来就可能扑空
				if err := store.Delete(name); err != nil && !errors.Is(err, ErrGroupNotFound) {
					t.Errorf("并发 Delete 失败: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()

	items, err := store.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) > 7 {
		t.Errorf("分组数不应超过写入过的名称数，实际 %d", len(items))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取分组文件失败: %v", err)
	}
	var decoded []Group
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("并发后文件应仍是完整 JSON（原子重命名保证）: %v", err)
	}
}
