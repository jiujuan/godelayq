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

// profileNamePattern 与 executor 侧的档位名规则是同一条（`executor/profile.go` 的
// profileNamePattern）。档位名会变成注册键 exec.<name>，而那个键要出现在 URL 段里，
// 因此空格与中文在这里就不合法。
//
// core 不许 import executor（executor 依赖 core），所以这里是独立的一份；
// 两边对同一批样本的结论必须一致，由 executor 包的跨包用例守住。
var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrProfileNameInvalid 表示档位名不合规则。
var ErrProfileNameInvalid = errors.New("invalid executor profile name")

// ErrProfileNotFound 表示指定档位不存在。
var ErrProfileNotFound = errors.New("executor profile not found")

// ErrProfileDuplicate 表示档位文件里同一名字出现两次。大小写不同也算重复：
// 注册键的比较是区分大小写的，而这里的查找忽略大小写，留着两条只会让"改哪一条"没有答案。
var ErrProfileDuplicate = errors.New("duplicate executor profile")

// ExecutorArgRecord 是档位的一个具名参数在磁盘上的写法，与 core.ExecutorArg 逐字段对应。
// 单列一份的理由与 ExecutorProfileRecord 同一条：配置结构只带 mapstructure 标签。
type ExecutorArgRecord struct {
	Name      string `json:"name"`
	Required  bool   `json:"required,omitempty"`
	Default   string `json:"default,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	Secret    bool   `json:"secret,omitempty"`
	AllowDash bool   `json:"allow_dash,omitempty"`
}

// ExecutorPositionalRecord 是位置参数规则在磁盘上的写法。
type ExecutorPositionalRecord struct {
	Max     int    `json:"max"`
	Pattern string `json:"pattern,omitempty"`
}

// ExecutorProfileRecord 是一条档位在档位文件里的写法，字段与 ExecutorCommand 一一对应，
// 另加两个时间戳。
//
// 为什么不直接给 ExecutorCommand 加 json 标签：那份结构是配置面，受 YAML 键名守卫
// （UnmarshalExact）与两份模板同步的约束，与存储面的演进方向不同；把两者绑在同一个结构上，
// 将来加一个存储专用字段就会多一个"配置里也能写但没意义"的键。
//
// Timeout 在文件里是字符串（"10m"）而不是纳秒数：这份文件是人会打开看的，
// 与 config.yaml 里的写法保持一致才不至于"配置里写 10m、文件里存 600000000000"。
type ExecutorProfileRecord struct {
	Name string `json:"name"`
	Kind string `json:"kind"`

	Runtime   string   `json:"runtime,omitempty"`
	Script    string   `json:"script,omitempty"`
	Program   string   `json:"program,omitempty"`
	FixedArgs []string `json:"fixed_args,omitempty"`

	Args       []ExecutorArgRecord       `json:"args,omitempty"`
	ArgsRender []string                  `json:"args_render,omitempty"`
	Positional *ExecutorPositionalRecord `json:"positional,omitempty"`

	Cwd      string            `json:"cwd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	EnvAllow []string          `json:"env_allow,omitempty"`

	Timeout     string `json:"timeout,omitempty"`
	MaxParallel int    `json:"max_parallel,omitempty"`
	RetryOnExit []int  `json:"retry_on_exit,omitempty"`

	Method          string              `json:"method,omitempty"`
	URLTemplate     string              `json:"url_template,omitempty"`
	AllowedHosts    []string            `json:"allowed_hosts,omitempty"`
	Headers         map[string][]string `json:"headers,omitempty"`
	HeaderAllow     []string            `json:"header_allow,omitempty"`
	Body            string              `json:"body,omitempty"`
	ExpectStatus    []int               `json:"expect_status,omitempty"`
	CaptureResponse bool                `json:"capture_response,omitempty"`
	MaxBodyBytes    int                 `json:"max_body_bytes,omitempty"`
	MaxRedirects    int                 `json:"max_redirects,omitempty"`
	DenyPrivate     *bool               `json:"deny_private_ranges,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewExecutorProfileRecord 把一条配置写法转成存储记录，供往返与比较用。
// 时间戳留空由 Save 补齐。
func NewExecutorProfileRecord(cmd ExecutorCommand) ExecutorProfileRecord {
	record := ExecutorProfileRecord{
		Name: cmd.Name,
		Kind: cmd.Kind,

		Runtime:   cmd.Runtime,
		Script:    cmd.Script,
		Program:   cmd.Program,
		FixedArgs: cmd.FixedArgs,

		ArgsRender: cmd.ArgsRender,
		Cwd:        cmd.Cwd,
		Env:        cmd.Env,
		EnvAllow:   cmd.EnvAllow,

		MaxParallel: cmd.MaxParallel,
		RetryOnExit: cmd.RetryOnExit,

		Method:          cmd.Method,
		URLTemplate:     cmd.URLTemplate,
		AllowedHosts:    cmd.AllowedHosts,
		Headers:         cmd.Headers,
		HeaderAllow:     cmd.HeaderAllow,
		Body:            cmd.Body,
		ExpectStatus:    cmd.ExpectStatus,
		CaptureResponse: cmd.CaptureResponse,
		MaxBodyBytes:    cmd.MaxBodyBytes,
		MaxRedirects:    cmd.MaxRedirects,
		DenyPrivate:     cmd.DenyPrivate,
	}

	if cmd.Timeout > 0 {
		record.Timeout = cmd.Timeout.String()
	}
	for _, arg := range cmd.Args {
		record.Args = append(record.Args, ExecutorArgRecord{
			Name:      arg.Name,
			Required:  arg.Required,
			Default:   arg.Default,
			Pattern:   arg.Pattern,
			Secret:    arg.Secret,
			AllowDash: arg.AllowDash,
		})
	}
	if cmd.Positional != nil {
		record.Positional = &ExecutorPositionalRecord{
			Max:     cmd.Positional.Max,
			Pattern: cmd.Positional.Pattern,
		}
	}
	return record
}

// Command 转回配置写法，供 executor 侧校验与探测用。
//
// 返回错误只有一种可能：timeout 不是合法的时长写法。文件是会被人手改的，
// 而 time.ParseDuration 的失败点如果留到执行器里再炸，报错位置已经和"哪一条档位"无关了。
func (r ExecutorProfileRecord) Command() (ExecutorCommand, error) {
	cmd := ExecutorCommand{
		Name: r.Name,
		Kind: r.Kind,

		Runtime:   r.Runtime,
		Script:    r.Script,
		Program:   r.Program,
		FixedArgs: r.FixedArgs,

		ArgsRender: r.ArgsRender,
		Cwd:        r.Cwd,
		Env:        r.Env,
		EnvAllow:   r.EnvAllow,

		MaxParallel: r.MaxParallel,
		RetryOnExit: r.RetryOnExit,

		Method:          r.Method,
		URLTemplate:     r.URLTemplate,
		AllowedHosts:    r.AllowedHosts,
		Headers:         r.Headers,
		HeaderAllow:     r.HeaderAllow,
		Body:            r.Body,
		ExpectStatus:    r.ExpectStatus,
		CaptureResponse: r.CaptureResponse,
		MaxBodyBytes:    r.MaxBodyBytes,
		MaxRedirects:    r.MaxRedirects,
		DenyPrivate:     r.DenyPrivate,
	}

	if strings.TrimSpace(r.Timeout) != "" {
		timeout, err := time.ParseDuration(strings.TrimSpace(r.Timeout))
		if err != nil {
			return ExecutorCommand{}, fmt.Errorf("executor profile %q has an invalid timeout %q: %w",
				r.Name, r.Timeout, err)
		}
		if timeout < 0 {
			// 负数在执行侧另有解释（与 0 一样回落到档位/全局默认），存进文件只会留下歧义：
			// 手改文件的人写不出"负超时"是什么意思。
			return ExecutorCommand{}, fmt.Errorf("executor profile %q has a negative timeout %q", r.Name, r.Timeout)
		}
		cmd.Timeout = timeout
	}
	for _, arg := range r.Args {
		cmd.Args = append(cmd.Args, ExecutorArg{
			Name:      arg.Name,
			Required:  arg.Required,
			Default:   arg.Default,
			Pattern:   arg.Pattern,
			Secret:    arg.Secret,
			AllowDash: arg.AllowDash,
		})
	}
	if r.Positional != nil {
		cmd.Positional = &ExecutorPositional{
			Max:     r.Positional.Max,
			Pattern: r.Positional.Pattern,
		}
	}
	return cmd, nil
}

// ExecutorProfileStore 保存页面上建出来的档位。
//
// 刻意不含 Flush/Close：档位是低频实体，每次改动同步落盘即可，
// 没有后台合并协程需要收尾（同 GroupStore 的那段理由）。
//
// 这里只管"文件级不变量"：名字合规、文件内不重名、timeout 写法合法。
// 字段组合的合法性（kind 与 runtime/script/program 的配套、参数与渲染模板的引用关系等）
// 一律归 executor.LoadProfiles / BuildProfile——两处各判一遍迟早分叉，
// 而分叉的表现是"页面上保存成功、重启后启动失败"。
type ExecutorProfileStore interface {
	// List 按档位名字典序返回全部记录，顺序稳定以便直接铺到 UI 上
	List() ([]ExecutorProfileRecord, error)
	// Get 忽略大小写查找一条档位
	Get(name string) (ExecutorProfileRecord, bool, error)
	// Save 新建或整体覆盖一条档位；已存在时保留其 CreatedAt
	Save(record ExecutorProfileRecord) error
	// Delete 忽略大小写删除；不存在返回 ErrProfileNotFound
	Delete(name string) error
}

// ValidateProfileName 检查档位名是否合规，错误信息里带上可接受的形状。
func ValidateProfileName(name string) error {
	if !profileNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q, expected 1-64 characters of [A-Za-z0-9_-]", ErrProfileNameInvalid, name)
	}
	return nil
}

// checkProfileRecord 是存储侧唯一的校验入口：名字 + timeout 写法。
// 通过它不等于这条档位能执行——字段组合还归 executor 侧判。
func checkProfileRecord(record ExecutorProfileRecord) error {
	if err := ValidateProfileName(record.Name); err != nil {
		return err
	}
	if _, err := record.Command(); err != nil {
		return err
	}
	return nil
}

// 编译期钉住接口实现：ExecutorProfileStore 的方法集一旦改动，这里立刻报错，
// 而不是等到 HTTP 层接线时才发现漏了方法。
var _ ExecutorProfileStore = (*JSONFileExecutorProfileStore)(nil)

// JSONFileExecutorProfileStore 是把档位存成单个 JSON 文件的 ExecutorProfileStore。
type JSONFileExecutorProfileStore struct {
	path string

	mu      sync.Mutex
	records map[string]ExecutorProfileRecord // key 为小写档位名，值保留用户书写的大小写
}

// NewJSONFileExecutorProfileStore 打开（必要时创建父目录）档位文件。
// 文件不存在视为空集合而不是错误——还没人在页面上建过档位是正常状态，
// 首次启动不该因为没人预先建文件而失败。
func NewJSONFileExecutorProfileStore(path string) (*JSONFileExecutorProfileStore, error) {
	if path == "" {
		path = DefaultExecProfilesPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create executor profile store directory failed: %w", err)
	}

	store := &JSONFileExecutorProfileStore{
		path:    path,
		records: make(map[string]ExecutorProfileRecord),
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

// load 读取磁盘内容；空文件与缺失文件都当作空集合，损坏的文件是错误。
//
// 损坏不能当成空集合：那会让下一次 Save 把整个文件重写掉，
// 页面上建好的档位就此消失，而现场看起来"一切正常"。
func (s *JSONFileExecutorProfileStore) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read executor profile file failed: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}

	var records []ExecutorProfileRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return fmt.Errorf("parse executor profile file %q failed: %w", s.path, err)
	}

	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if err := checkProfileRecord(record); err != nil {
			return fmt.Errorf("executor profile file %q: %w", s.path, err)
		}
		key := strings.ToLower(record.Name)
		if seen[key] {
			return fmt.Errorf("%w: %q appears more than once in %q", ErrProfileDuplicate, record.Name, s.path)
		}
		seen[key] = true
		s.records[key] = record
	}
	return nil
}

func (s *JSONFileExecutorProfileStore) List() ([]ExecutorProfileRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	items := make([]ExecutorProfileRecord, 0, len(s.records))
	for _, record := range s.records {
		items = append(items, record)
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items, nil
}

func (s *JSONFileExecutorProfileStore) Get(name string) (ExecutorProfileRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.records[strings.ToLower(name)]
	return record, ok, nil
}

func (s *JSONFileExecutorProfileStore) Save(record ExecutorProfileRecord) error {
	if err := checkProfileRecord(record); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := strings.ToLower(record.Name)
	now := time.Now()
	record.UpdatedAt = now
	if existing, ok := s.records[key]; ok {
		// 覆盖式更新不该把创建时间冲掉，除非调用方显式带了更早的值
		if record.CreatedAt.IsZero() {
			record.CreatedAt = existing.CreatedAt
		}
	} else {
		if record.CreatedAt.IsZero() {
			record.CreatedAt = now
		}
	}

	s.records[key] = record
	return s.flushLocked()
}

func (s *JSONFileExecutorProfileStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := strings.ToLower(name)
	if _, ok := s.records[key]; !ok {
		return ErrProfileNotFound
	}
	delete(s.records, key)
	return s.flushLocked()
}

// flushLocked 原子重写整个档位文件。调用者需持有 s.mu。
//
// 低频实体直接全量重写：与分组、jobs 快照同源的"临时文件 + rename"保证读者
// 不会看到写了一半的文件。
func (s *JSONFileExecutorProfileStore) flushLocked() error {
	items := make([]ExecutorProfileRecord, 0, len(s.records))
	for _, record := range s.records {
		items = append(items, record)
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
