package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func newProfileStore(t *testing.T) (*JSONFileExecutorProfileStore, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "exec-profiles.json")
	store, err := NewJSONFileExecutorProfileStore(path)
	if err != nil {
		t.Fatalf("创建档位存储失败: %v", err)
	}
	return store, path
}

func sampleRecord(name string) ExecutorProfileRecord {
	return ExecutorProfileRecord{
		Name:       name,
		Kind:       "script",
		Runtime:    "python",
		Script:     "scripts/report.py",
		ArgsRender: []string{"--day={day}"},
		Args: []ExecutorArgRecord{{
			Name:     "day",
			Required: true,
			Pattern:  `^(yesterday|today)$`,
		}},
		Timeout:     "10m",
		MaxParallel: 1,
		RetryOnExit: []int{75},
	}
}

func TestExecutorProfileStore_SaveListGet(t *testing.T) {
	store, _ := newProfileStore(t)

	if err := store.Save(sampleRecord("nightly_report")); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if err := store.Save(ExecutorProfileRecord{Name: "Ping-2", Kind: "http"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("应有 2 条档位，实际 %d", len(items))
	}
	// 顺序是按小写名字典序：大小写混着写也不该换位置
	if items[0].Name != "nightly_report" || items[1].Name != "Ping-2" {
		t.Errorf("List 应按名称排序，实际 %s, %s", items[0].Name, items[1].Name)
	}
	if items[0].Timeout != "10m" || items[0].Runtime != "python" {
		t.Errorf("字段应原样保存，实际 timeout=%q runtime=%q", items[0].Timeout, items[0].Runtime)
	}
	if items[0].CreatedAt.IsZero() || items[0].UpdatedAt.IsZero() {
		t.Error("Save 应补齐创建与更新时间")
	}

	// 名字查找忽略大小写：URL 段里手打大小写不该造出第二条档位
	record, ok, err := store.Get("NIGHTLY_REPORT")
	if err != nil || !ok {
		t.Fatalf("忽略大小写查找失败: ok=%v err=%v", ok, err)
	}
	if record.Name != "nightly_report" {
		t.Errorf("应返回原书写名称，实际 %q", record.Name)
	}

	if _, ok, _ := store.Get("missing"); ok {
		t.Error("不存在的档位不该查得到")
	}
}

// List 返回的是副本：调用方改返回值不该动到存储。
func TestExecutorProfileStore_ListReturnsCopy(t *testing.T) {
	store, _ := newProfileStore(t)
	if err := store.Save(sampleRecord("nightly_report")); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	items[0].Runtime = "tampered"
	items[0].Args = append(items[0].Args, ExecutorArgRecord{Name: "injected"})
	again, err := store.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if again[0].Runtime != "python" {
		t.Errorf("改动返回值不该影响存储，实际 runtime=%q", again[0].Runtime)
	}
	if _, ok, _ := store.Get("injected"); ok {
		t.Error("改动返回值的切片不该在存储里造出新档位")
	}
}

func TestExecutorProfileStore_RejectsBadNames(t *testing.T) {
	store, _ := newProfileStore(t)

	for _, name := range []string{"", "with space", "点", "a/b", "too-long-" + strings.Repeat("x", 60), "日-志"} {
		err := store.Save(ExecutorProfileRecord{Name: name, Kind: "script"})
		if err == nil {
			t.Errorf("档位名 %q 应当被拒", name)
			continue
		}
		if !errors.Is(err, ErrProfileNameInvalid) {
			t.Errorf("档位名 %q 应报 ErrProfileNameInvalid，实际 %v", name, err)
		}
	}

	// 合法写法：字母数字下划线连字符，最长 64
	for _, name := range []string{"a", "A_b-9", strings.Repeat("x", 64)} {
		if err := store.Save(ExecutorProfileRecord{Name: name, Kind: "http"}); err != nil {
			t.Errorf("档位名 %q 应当合法，实际 %v", name, err)
		}
	}
}

func TestExecutorProfileStore_UpdateKeepsCreatedAt(t *testing.T) {
	store, _ := newProfileStore(t)

	first := sampleRecord("nightly_report")
	if err := store.Save(first); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	created, _, err := store.Get("nightly_report")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}

	time.Sleep(2 * time.Millisecond)
	// 覆盖式更新：调用方不带 CreatedAt，存储要自己把原来的值留住
	updated := ExecutorProfileRecord{Name: "nightly_report", Kind: "script", Runtime: "node", Script: "scripts/a.mjs"}
	if err := store.Save(updated); err != nil {
		t.Fatalf("覆盖 Save 失败: %v", err)
	}

	got, _, err := store.Get("nightly_report")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("Created_at 应保留 %v，实际 %v", created.CreatedAt, got.CreatedAt)
	}
	if !got.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("UpdatedAt 应刷新，实际 %v", got.UpdatedAt)
	}
	// 整体覆盖语义：没带的字段就是清空，不是沿用旧值
	if got.Timeout != "" || got.Runtime != "node" {
		t.Errorf("覆盖后应是新值而不是旧值残留，实际 timeout=%q runtime=%q", got.Timeout, got.Runtime)
	}
}

func TestExecutorProfileStore_Delete(t *testing.T) {
	store, _ := newProfileStore(t)

	if err := store.Save(sampleRecord("nightly_report")); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if err := store.Delete("NIGHTLY_REPORT"); err != nil {
		t.Fatalf("忽略大小写删除失败: %v", err)
	}
	if _, ok, _ := store.Get("nightly_report"); ok {
		t.Error("删除后不该查得到")
	}
	if err := store.Delete("nightly_report"); !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("删除不存在的档位应报 ErrProfileNotFound，实际 %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("删除后文件里不该还剩条目，实际 %d 条", len(items))
	}
}

func TestExecutorProfileStore_PersistsAcrossReopen(t *testing.T) {
	store, path := newProfileStore(t)

	if err := store.Save(sampleRecord("nightly_report")); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if err := store.Save(ExecutorProfileRecord{
		Name: "status_probe", Kind: "http", Method: "GET",
		URLTemplate: "https://example.invalid/{id}", AllowedHosts: []string{"example.invalid"},
	}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	reopened, err := NewJSONFileExecutorProfileStore(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	items, err := reopened.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("重开应有 2 条，实际 %d", len(items))
	}
	// 排序按小写名字：nightly_report 在 status_probe 之前
	if items[0].Script != "scripts/report.py" || items[0].Args[0].Name != "day" {
		t.Errorf("嵌套字段应原样读回，实际 script=%q arg0=%q", items[0].Script, items[0].Args[0].Name)
	}

	// 磁盘上就应该是可读的 snake_case 写法：这份文件运维会打开看
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	text := string(raw)
	for _, want := range []string{`"args_render"`, `"max_parallel"`, `"retry_on_exit"`, `"url_template"`, `"allowed_hosts"`} {
		if !strings.Contains(text, want) {
			t.Errorf("文件里应有 %s，实际内容：%s", want, text)
		}
	}
	if strings.Contains(text, `"created_at": "0001-01-01"`) {
		t.Errorf("时间戳不该以零值写出去：%s", text)
	}

	// 顶层必须是数组，条目顺序稳定（按名字排序），否则两份文件的 diff 毫无意义
	var onDisk []map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("文件应是合法 JSON 数组: %v", err)
	}
	if len(onDisk) != 2 || onDisk[0]["name"] != "nightly_report" {
		t.Errorf("磁盘条目顺序应为字典序，实际 %v", onDisk)
	}
}

func TestExecutorProfileStore_DoesNotCreateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "exec-profiles.json")

	if _, err := NewJSONFileExecutorProfileStore(path); err != nil {
		t.Fatalf("文件不存在应视为空集合: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("构造不该创建文件（只有页面写过之后才存在），实际 stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Errorf("父目录应当被创建出来，否则第一次保存必失败: %v", err)
	}
}

func TestExecutorProfileStore_EmptyFileAndCorruptFile(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte("   \n"), 0644); err != nil {
		t.Fatalf("预置空文件失败: %v", err)
	}
	if store, err := NewJSONFileExecutorProfileStore(empty); err != nil {
		t.Errorf("空白文件应视为空集合，实际 %v", err)
	} else if items, _ := store.List(); len(items) != 0 {
		t.Errorf("空白文件应得到空集合，实际 %d 条", len(items))
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0644); err != nil {
		t.Fatalf("预置损坏文件失败: %v", err)
	}
	if _, err := NewJSONFileExecutorProfileStore(corrupt); err == nil {
		t.Error("损坏的档位文件必须报错，而不是静默当成空集合把已建好的档位覆盖掉")
	} else if !strings.Contains(err.Error(), filepath.Base(corrupt)) {
		// 路径在错误里是带引号的写法，比对文件名部分即可（Windows 的分隔符会被转义）
		t.Errorf("错误信息应带文件路径便于定位，实际 %v", err)
	}
}

func TestExecutorProfileStore_RejectsDuplicateNamesInFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exec-profiles.json")

	dup := `[{"name":"a_one","kind":"http"},{"name":"A_ONE","kind":"http"}]`
	if err := os.WriteFile(path, []byte(dup), 0644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}
	if _, err := NewJSONFileExecutorProfileStore(path); !errors.Is(err, ErrProfileDuplicate) {
		t.Errorf("文件内重名（忽略大小写）必须报错，实际 %v", err)
	}
}

// 坏 timeout 要在打开文件时就指出是哪一条档位：留到执行器里再炸，
// 报错位置已经和"哪一条"无关了。
func TestExecutorProfileStore_InvalidTimeoutNamesProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exec-profiles.json")

	bad := `[{"name":"good_one","kind":"script","timeout":"10m"},{"name":"bad_one","kind":"script","timeout":"10 minutes"}]`
	if err := os.WriteFile(path, []byte(bad), 0644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}
	_, err := NewJSONFileExecutorProfileStore(path)
	if err == nil {
		t.Fatal("非法 timeout 必须报错")
	}
	if !strings.Contains(err.Error(), "bad_one") {
		t.Errorf("错误信息应含档位名，实际 %v", err)
	}

	store, _ := newProfileStore(t)
	if err := store.Save(ExecutorProfileRecord{Name: "bad_two", Kind: "script", Timeout: "5min"}); err == nil {
		t.Error("Save 也应拒掉非法 timeout 写法")
	}
	if err := store.Save(ExecutorProfileRecord{Name: "neg_one", Kind: "script", Timeout: "-1m"}); err == nil {
		t.Error("负数 timeout 不该被收下：执行侧对它另有解释，存进文件只会留下歧义")
	}
}

// 存储不校验字段组合：那是 executor.LoadProfiles / BuildProfile 的职责（I1 一份规则）。
// 这条用例是"这里故意不收"的反证，别在后续卡里顺手加严。
func TestExecutorProfileStore_SaveDoesNotValidateFieldCombination(t *testing.T) {
	store, _ := newProfileStore(t)

	// kind: script 却没有 runtime——executor 侧一定拒，但存储这里必须收下
	if err := store.Save(ExecutorProfileRecord{Name: "half_baked", Kind: "script", Script: "scripts/x.py"}); err != nil {
		t.Fatalf("字段组合的合法性不该由存储判，实际 %v", err)
	}
	// kind 拼错同理
	if err := store.Save(ExecutorProfileRecord{Name: "wrong_kind", Kind: "scirpt"}); err != nil {
		t.Fatalf("kind 取值也不该由存储判，实际 %v", err)
	}

	record, ok, err := store.Get("half_baked")
	if err != nil || !ok {
		t.Fatalf("读回应存在: ok=%v err=%v", ok, err)
	}
	if _, err := record.Command(); err != nil {
		t.Errorf("转回配置写法不该报错，实际 %v", err)
	}
}

func TestExecutorProfileRecord_CommandRoundTrip(t *testing.T) {
	cmd := ExecutorCommand{
		Name:        "nightly_report",
		Kind:        "script",
		Runtime:     "python",
		Script:      "scripts/report.py",
		Args:        []ExecutorArg{{Name: "day", Required: true, Pattern: `^(yesterday|today)$`}},
		ArgsRender:  []string{"--day={day}"},
		Positional:  &ExecutorPositional{Max: 2, Pattern: `^[A-Za-z0-9._:/=,-]{1,256}$`},
		Cwd:         "work",
		Env:         map[string]string{"REPORT_HOME": "/srv/report"},
		EnvAllow:    []string{"TRACE_ID"},
		Timeout:     10 * time.Minute,
		MaxParallel: 1,
		RetryOnExit: []int{75, 76},
	}

	record := NewExecutorProfileRecord(cmd)
	back, err := record.Command()
	if err != nil {
		t.Fatalf("Command 失败: %v", err)
	}
	if !reflect.DeepEqual(back, cmd) {
		t.Errorf("往返应无损：\n期望 %#v\n实际 %#v", cmd, back)
	}

	// 空切片与 nil 的区分：配置侧 nil 表示"没写这一项"，往返不该把它变成空切片
	empty := ExecutorCommand{Name: "sparse", Kind: "http"}
	sparseRecord := NewExecutorProfileRecord(empty)
	sparseBack, err := sparseRecord.Command()
	if err != nil {
		t.Fatalf("Command 失败: %v", err)
	}
	if sparseBack.FixedArgs != nil || sparseBack.Args != nil || sparseBack.RetryOnExit != nil {
		t.Errorf("nil 切片往返后仍是 nil，实际 %#v", sparseBack)
	}
	if sparseBack.Timeout != 0 {
		t.Errorf("没写 timeout 应转成 0（由执行侧取默认），实际 %v", sparseBack.Timeout)
	}
	// 零值 timeout 不该在文件里写出 "0s" 这种噪声
	if sparseRecord.Timeout != "" {
		t.Errorf("timeout 为 0 时记录里应是空串，实际 %q", sparseRecord.Timeout)
	}

	// http 那一组字段也要过一遍（Record 与 Command 的字段清单容易只对着 script 那组写）
	denyPrivate := false
	httpCmd := ExecutorCommand{
		Name: "ping", Kind: "http", Method: "POST", URLTemplate: "https://example.invalid/{id}",
		AllowedHosts: []string{"example.invalid"}, Headers: map[string][]string{"X-A": {"1"}},
		HeaderAllow: []string{"X-Trace"}, Body: "json", ExpectStatus: []int{200, 204},
		CaptureResponse: true, MaxBodyBytes: 4096, DenyPrivate: &denyPrivate,
	}
	httpBack, err := NewExecutorProfileRecord(httpCmd).Command()
	if err != nil {
		t.Fatalf("Command 失败: %v", err)
	}
	if !reflect.DeepEqual(httpBack, httpCmd) {
		t.Errorf("http 字段往返不一致：\n期望 %#v\n实际 %#v", httpCmd, httpBack)
	}
}

func TestExecutorProfileStore_DefaultPathWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("读取工作目录失败: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换工作目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	store, err := NewJSONFileExecutorProfileStore("")
	if err != nil {
		t.Fatalf("空路径应回落到默认位置: %v", err)
	}
	if filepath.Base(store.path) != filepath.Base(DefaultExecProfilesPath) {
		t.Errorf("默认文件名应为 %s，实际 %s", DefaultExecProfilesPath, store.path)
	}
}

// 读写都走同一把锁：并发下不该出现数据竞争或半写文件。
func TestExecutorProfileStore_ConcurrentSaveAndDelete(t *testing.T) {
	store, path := newProfileStore(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("profile-%02d", i%7)
			record := sampleRecord(name)
			record.Runtime = "node"
			record.Script = "scripts/a.mjs"
			record.Args = nil
			record.ArgsRender = nil
			record.RetryOnExit = nil
			record.Timeout = ""
			if err := store.Save(record); err != nil {
				t.Errorf("并发 Save 失败: %v", err)
			}
			if i%3 == 0 {
				// 忽略 ErrProfileNotFound：并发下删除本来就可能扑空
				if err := store.Delete(name); err != nil && !errors.Is(err, ErrProfileNotFound) {
					t.Errorf("并发 Delete 失败: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	var onDisk []ExecutorProfileRecord
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("并发写完之后文件必须仍是合法 JSON: %v", err)
	}
	seen := map[string]bool{}
	for _, record := range onDisk {
		key := strings.ToLower(record.Name)
		if seen[key] {
			t.Errorf("磁盘上出现重名 %q", record.Name)
		}
		seen[key] = true
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != len(onDisk) {
		t.Errorf("内存与磁盘应一致，实际内存 %d 磁盘 %d", len(items), len(onDisk))
	}
}
