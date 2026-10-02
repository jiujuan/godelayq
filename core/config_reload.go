package core

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// 本文件是配置热重载的判据部分（设计文档 §6、§7.6）：每个叶子键属于哪一档、
// 两份配置比出来算不算改动、改动该进哪三份清单。
//
// 这里没有任何副作用：不建监听器、不改任何子系统、还没有调用方——
// 监听器是 R05、把新值交给各 setter 的重载链是 R06。三样交付物都是纯判据，
// 为的是"哪些取值允许运行期换"这件事只有一处答案，并由 TestEveryLeafKeyIsClassed 钉住。

// commandsPath 是档位列表的键路径。它是唯一一个按元素摊开的列表：
// "改了哪一条的哪个字段"必须能被单独分类（设计文档 §6.4）。
const commandsPath = "executors.commands"

// ConfigClass 是一个叶子键在重载里的归属（设计文档 R1）。
type ConfigClass int

const (
	// ClassHot 重载时应用。
	ClassHot ConfigClass = iota
	// ClassRestart 重载时接受但不应用，逐条记入 ignored_keys。
	ClassRestart
	// ClassReject 变化即整次作废。
	ClassReject
)

// String 返回 hot|restart|reject，供日志与测试输出使用。
func (c ConfigClass) String() string {
	switch c {
	case ClassHot:
		return "hot"
	case ClassRestart:
		return "restart"
	case ClassReject:
		return "reject"
	default:
		return fmt.Sprintf("ConfigClass(%d)", int(c))
	}
}

// configClasses 是三档分类的唯一权威表：叶子路径 → 档位。
// 表里有、结构体里没有的键（前缀规则命中）也要能分类，所以判定时先看精确路径，
// 再看 rejectPrefixes 里的前缀，最后看档位字段名。
//
// 新增配置键必须在这里登记：没登记的键由 TestEveryLeafKeyIsClassed 报错，
// 而不是悄悄永远不会被重载。漏登记与留着旧路径两个方向都守。
var configClasses = map[string]ConfigClass{
	// 热更：取值型参数，改动不扩大任何能力边界
	"logging.level":                        ClassHot,
	"scheduler.workers":                    ClassHot,
	"scheduler.max_retry_delay":            ClassHot,
	"store.history_limit":                  ClassHot,
	"store.history_ttl":                    ClassHot,
	"observability.events.retention_count": ClassHot,
	"observability.events.retention_age":   ClassHot,
	"observability.audit.retention_count":  ClassHot,
	"observability.audit.retention_age":    ClassHot,
	// 条目增删本身是热更（设计文档 §6.4）：新增一条档位由 executor 侧在应用时再校验一次，
	// 改已有档位的身份字段则另按 permissionCommandFields 判（R04 在 executor 侧挡第二层）。
	// 这一项只在列表为空时以容器路径出现；有条目时摊成 executors.commands.<name>.<field>。
	"executors.commands": ClassHot,
	"reload.debounce":    ClassHot,

	// 重启：绑着 Start 期建立的通道/ticker/连接，或属于重新装配
	"server.port":                ClassRestart,
	"scheduler.queue_capacity":   ClassRestart,
	"scheduler.shutdown_timeout": ClassRestart,
	"logging.format":             ClassRestart,
	"reload.enabled":             ClassRestart,
	// 其余全部落在重启档：用一张默认表反而看不清"哪些键想过关"，
	// 所以下面这些也逐条列出（值相同，作用是让读的人知道它们被认真归过类）。
	"store.type": ClassRestart, "store.path": ClassRestart,
	"store.flush_interval": ClassRestart, "store.groups_path": ClassRestart,
	"observability.enabled": ClassRestart, "observability.path": ClassRestart,
	"observability.flush_interval": ClassRestart, "observability.queue_capacity": ClassRestart,
	"observability.busy_timeout": ClassRestart, "observability.synchronous": ClassRestart,
	"observability.events.enabled": ClassRestart, "observability.artifacts.enabled": ClassRestart,
	"observability.audit.enabled": ClassRestart,
	"executors.enabled":           ClassRestart, "executors.required_role": ClassRestart,
	"executors.workspace": ClassRestart, "executors.runtime_allow": ClassRestart,
	"executors.env_allow": ClassRestart, "executors.concurrency": ClassRestart,
	"executors.queue_capacity": ClassRestart, "executors.default_timeout": ClassRestart,
	"executors.max_timeout": ClassRestart, "executors.restore_policy": ClassRestart,
	"executors.loader_allow": ClassRestart, "executors.web_enabled": ClassRestart,
	"executors.profiles_path":         ClassRestart,
	"executors.output.inline_preview": ClassRestart, "executors.output.max_bytes": ClassRestart,
	"executors.output.dir": ClassRestart, "executors.output.ttl": ClassRestart,
	"server.cors.allow_origins": ClassRestart, "server.cors.allow_credentials": ClassRestart,
	"server.auth.jwt.access_ttl": ClassRestart, "server.auth.jwt.refresh_ttl": ClassRestart,
}

// rejectPrefixes 是凭据那一组：整棵子树拒绝。
//
// 现在是**等值匹配就命中**：只有 executors.commands 按元素摊开，server.auth.users
// 整份账号列表摊出来是 server.auth.users 这一个叶子（见卡 §3.3 的落地口径）。
// 带点子路径的匹配（path == prefix || HasPrefix(path, prefix+".")）是给将来
// 真的按元素摊用户（server.auth.users.#0.password_bcrypt 那一类）留的口子：
// 真摊开的那天，新子路径不用改表也落在拒绝档，不会退化成"改了凭据静默不生效"。
var rejectPrefixes = []string{
	"server.auth.token",
	"server.auth.users",
	"server.auth.jwt.secret",
}

// permissionCommandFields 是档位内的可执行体身份、目标与凭据字段（设计文档 §6.4、待拍板 P2）。
// "可执行体身份"含两层：这个任务类型到底是哪一类（kind）、它具体跑哪个文件/程序（runtime、
// script、program 等）。这些字段变了 → 整次作废。
//
// 清单必须显式列字段，且与 hotCommandFields 合起来**恰好覆盖** core.ExecutorCommand 摊出的
// 每一个字段名：将来给 ExecutorCommand 加字段时，新字段不会默认落进任何一侧——它两边都不在，
// classify 回 ok=false，由 TestEveryLeafKeyIsClassed 逼着人显式回答"它落在哪一侧"。
// 这是本卡修的一条守卫：首版实现里"字段名不在拒绝清单就归热更"的写法会让新字段静默落进
// 热更档、守卫正向放行，卡面承诺的"显式归档"因此不成立（见卡 §10.2 第 12 条、D-R0104）。
//
// 判据只用 core 自己看得见的字段名，不借 executor.LoadProfiles 的归一化结果
// ——core 不许 import executor（依赖方向红线，待拍板 P2）。
var permissionCommandFields = map[string]bool{
	// 是哪一类可执行体（script|binary|http）：它决定走进程执行器还是 HTTP 执行主体，
	// 换了它等于换掉"这个任务类型到底执行什么"。与档位在线管理的 D7 同向，
	// 见 web-profile-design.md:38 与 api/handlers_executor_profiles.go:514 的 immutableFieldChange。
	"kind": true,
	// 跑哪个可执行体
	"runtime":    true,
	"script":     true,
	"program":    true,
	"fixed_args": true,
	"cwd":        true,
	// 以什么身份跑
	"env":       true, // 固定注入的凭据材料：控制台从不回显它的取值
	"env_allow": true,
	// 把请求发到哪里、怎么发
	"method":              true,
	"url_template":        true,
	"allowed_hosts":       true,
	"headers":             true,
	"header_allow":        true,
	"deny_private_ranges": true,
	"max_redirects":       true,
}

// hotCommandFields 是档位内允许热更的取值型字段（设计文档 §6.4 第一条），与
// permissionCommandFields 显式对偶：两份清单合起来恰好覆盖 core.ExecutorCommand 的每一个字段名，
// 命中的叶子归 ClassHot。这里的都是可调参数：body 只决定 payload 的 body 按 json/raw/none 解释，
// 既不换可执行体也不换目标主机。kind 不在这里——它选的是执行主体本身，归 permissionCommandFields。
//
// 加字段时必须显式选一份清单登记，否则 classify 回 ok=false、守卫用例红（见 permissionCommandFields 的说明）。
var hotCommandFields = map[string]bool{
	"name":             true, // 重命名等价于删一条加一条
	"timeout":          true,
	"max_parallel":     true,
	"retry_on_exit":    true,
	"args":             true, // payload 参数声明
	"args_render":      true,
	"positional":       true,
	"body":             true,
	"expect_status":    true,
	"capture_response": true,
	"max_body_bytes":   true,
}

// classify 返回一个叶子路径的档位。ok 为 false 表示既没有精确命中也没有前缀命中，
// 属于"新增键忘了归档"，调用方（守卫用例）据此报错。
//
// 判定顺序：精确路径 → 凭据前缀 → executors.commands.<name>.<field> 的字段名（三态：
// 命中 permissionCommandFields 归拒绝档、命中 hotCommandFields 归热更档、两边都不在则
// ok=false——新增字段必须显式归档，否则守卫红，不会静默落进热更档）。
// Diff 与守卫用例共用这一份规则，避免两套判据。
func classify(path string) (class ConfigClass, ok bool) {
	if class, found := configClasses[path]; found {
		return class, true
	}
	for _, prefix := range rejectPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+".") {
			return ClassReject, true
		}
	}
	if _, field, isCommand := splitCommandLeaf(path); isCommand {
		switch {
		case permissionCommandFields[field]:
			return ClassReject, true
		case hotCommandFields[field]:
			return ClassHot, true
		default:
			// 档位新增字段没归档：与未知顶层键同一脸色，落到下面的 ok=false。
			return ClassRestart, false
		}
	}
	// 没归档：ok=false 让调用方（守卫用例）报错。档位返回重启档而不是热更档，
	// 这样忽略 ok 的调用方最坏只是"改了没生效但有记录"，不会把未归档的键当成免重启通道。
	return ClassRestart, false
}

// leafKind 是叶子的形状标记（标量 / 列表 / 映射），测试用它断言摊平的形状。
//
// 比较本身不看它：sameLeaf 只用 reflect.DeepEqual，因为同一个类型摊出的同一条路径
// Kind 恒等，比对 Kind 是个不可达分支（曾经的写法，见卡 §10.2 第 14 条）。
// "空列表"与"没有这个键"确实是两次不同的改动，但它们的区分不靠 Kind：
//   - 空列表 vs nil 列表：两侧的 map 里都有这个键，reflect.DeepEqual 对 nil slice
//     与空 slice 返回 false；
//   - 没有这个键：一侧的 map 里根本没有这个路径，由 Diff 的遍历（两侧路径并集）发现。
type leafKind int

const (
	leafScalar leafKind = iota
	leafSlice
	leafMap
)

// leafValue 是摊平出来的一个取值。Kind 只是测试可见的形状标记（见 leafKind 的说明），
// 比较与分类都只看 Val 和路径。
type leafValue struct {
	Kind leafKind
	Val  reflect.Value
}

// any 返回叶子的原值。路径只在一侧出现时另一侧没有值，返回 nil。
func (l leafValue) any() any {
	if !l.Val.IsValid() {
		return nil
	}
	return l.Val.Interface()
}

// flattenLeaves 把 Config 结构体摊成叶子清单。规则：
//   - 结构体字段按 mapstructure 标签拼成点分路径，递归展开；
//   - executors.commands 的切片按元素展开成 executors.commands.<name>.<field>
//     （name 取该条的 Name 字段，空名的条目按索引 executors.commands.#<i>.<field> 兜底，
//     否则两条无名档位会互相覆盖同一个路径），因为"改了哪一条的哪个字段"必须能被分类；
//     列表为空（含 nil）时容器路径本身作为一个叶子出现，承载"条目增删"这件事；
//   - 其余切片、映射与标量（含 time.Duration 这类命名标量，它的 Kind 是 Int64 不是 Struct）
//     都是叶子，值用 reflect.DeepEqual 比；
//   - 摊不出任何导出字段的结构体（time.Time、不透明封装类型、空占位节）整体作为一个叶子
//     出现在它的容器路径上，否则这个键会从分类表和 Diff 里彻底消失（见 flattenStruct）；
//   - 指针（deny_private_ranges、positional）也当一个叶子：nil 与"指向 false"是两次不同的
//     改动，DeepEqual 顺着指针比内容，所以指针背后的取值改动照样能被发现。
//
// 不复用 core/config_test.go 的 configKeys：那份从 YAML 文件读键名、只给路径不给值，
// 且要求文件存在；这里要对两份内存里的 Config 比取值。
func flattenLeaves(cfg Config) map[string]leafValue {
	leaves := make(map[string]leafValue)
	flattenStruct(leaves, "", reflect.ValueOf(cfg))
	return leaves
}

// flattenStruct 展开一个结构体的每个导出字段。
func flattenStruct(leaves map[string]leafValue, prefix string, structValue reflect.Value) {
	structType := structValue.Type()
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if field.PkgPath != "" {
			continue // 非导出字段不是配置项
		}
		path := joinLeafPath(prefix, leafKey(field))
		value := structValue.Field(i)

		switch {
		case isCommandList(value):
			flattenCommands(leaves, path, value)
		case value.Kind() == reflect.Struct:
			// time.Duration 等命名标量的 Kind 不是 Struct，走不到这一支
			before := len(leaves)
			flattenStruct(leaves, path, value)
			// 摊不出任何导出字段的结构体整体算一个叶子：time.Time 的三个字段
			// （wall/ext/loc）全是未导出的，不透明封装类型与空占位节同理。
			// 不做这个兜底，这一支会安静地产出 0 个叶子，于是这个配置键既不进 leaves
			// 也不进 Diff，正反向两条守卫都看不见它——改了它既不会热更、也不会提示要重启，
			// 正是本卡要消灭的那种静默。当成一个整体叶子之后，改它的任何可见表现都会
			// 让 DeepEqual 报出差异，键名也会以容器路径出现在清单里、必须显式归档。
			if len(leaves) == before {
				addLeaf(leaves, path, value)
			}
		default:
			addLeaf(leaves, path, value)
		}
	}
}

// flattenCommands 按元素展开档位列表；空列表退化成容器路径这一个叶子。
func flattenCommands(leaves map[string]leafValue, path string, list reflect.Value) {
	if list.Len() == 0 {
		addLeaf(leaves, path, list)
		return
	}
	for i := 0; i < list.Len(); i++ {
		item := list.Index(i)
		flattenStruct(leaves, path+"."+commandEntryName(item, i), item)
	}
}

// isCommandList 判断一个取值是不是 executors.commands 那类档位列表。
// 按元素类型判断而不是按路径写死：将来再加一份同类型的列表时，摊平与分类仍然一致。
func isCommandList(value reflect.Value) bool {
	return value.Kind() == reflect.Slice && value.Type().Elem() == reflect.TypeOf(ExecutorCommand{})
}

// commandEntryName 取档位的路径名。
//
// 配置加载链不校验档位名（ValidateProfileName 只挂在档位存储的写入入口上），所以这里自己保证
// 路径段里不含点：splitCommandLeaf 用两次 strings.Cut 反解 name 与 field，前提就是
// "名字那一段不含点"。带点的名字（例如 report.timeout）会让它下面 26 个字段路径被误切成
// name=report field=timeout，全部按"字段 timeout 在热更清单"归类、归因署错名。
// 与空名同一个处置：兜底成 #<i> 索引，合法名字的取值原样返回、行为不变。
func commandEntryName(item reflect.Value, index int) string {
	name := item.FieldByName("Name")
	if name.IsValid() && name.Kind() == reflect.String {
		if trimmed := strings.TrimSpace(name.String()); trimmed != "" && !strings.Contains(trimmed, ".") {
			return trimmed
		}
	}
	return fmt.Sprintf("#%d", index)
}

// addLeaf 记录一个叶子。同一路径重复出现时后者覆盖前者——档位重名属于 executor 侧
// 启动就拒的写法，这里不值得为它再造一套规则。
func addLeaf(leaves map[string]leafValue, path string, value reflect.Value) {
	leaves[path] = leafValue{Kind: kindOfLeaf(value), Val: value}
}

func kindOfLeaf(value reflect.Value) leafKind {
	switch value.Kind() {
	case reflect.Slice, reflect.Array:
		return leafSlice
	case reflect.Map:
		return leafMap
	default:
		return leafScalar
	}
}

// leafKey 返回结构体字段在配置里的键名：取 mapstructure 标签（去掉后面的选项），
// 没有标签时退回小写字段名——漏写标签不该让这个键从分类表里消失。
func leafKey(field reflect.StructField) string {
	key, _, _ := strings.Cut(field.Tag.Get("mapstructure"), ",")
	if key == "" {
		return strings.ToLower(field.Name)
	}
	return key
}

func joinLeafPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// splitCommandLeaf 把 executors.commands.<name>.<field>[.子字段] 拆成名字与字段名。
// ok 为 false 表示这不是档位内部的叶子（含容器路径 executors.commands 本身）。
//
// 两次 Cut 能反解的前提是"名字那一段不含点"。这个前提由 commandEntryName 在摊平时
// 自己保证（含点与空名一起兜底成 #<i>），不是由 ValidateProfileName 保证的——
// 配置加载链不调它，它只挂在档位存储的写入入口上（D-R0106）。
// 字段名取剩下的首段，所以 positional.max 归到 positional 一项。
func splitCommandLeaf(path string) (name, field string, ok bool) {
	rest, found := strings.CutPrefix(path, commandsPath+".")
	if !found {
		return "", "", false
	}
	name, rest, found = strings.Cut(rest, ".")
	if !found || name == "" {
		return "", "", false
	}
	field, _, _ = strings.Cut(rest, ".")
	if field == "" {
		return "", "", false
	}
	return name, field, true
}

// isCommandPath 判断路径是否属于档位那一节（容器路径本身或它的任一子路径）。
func isCommandPath(path string) bool {
	return path == commandsPath || strings.HasPrefix(path, commandsPath+".")
}

// ChangedKey 是一次改动的路径与两侧取值。Val 为 any（叶子原值），日志与
// /admin/runtime 只输出路径，不输出值——配置里可能有凭据。
type ChangedKey struct {
	Path string
	Old  any
	New  any
}

// ConfigChange 是 Diff 的结果，三份清单的键都按路径字典序排好。
type ConfigChange struct {
	Hot      []ChangedKey
	Restart  []ChangedKey
	Reject   []ChangedKey
	Commands []ChangedKey // Hot 里属于 executors.commands 的那批，单列供 R04/R06 判定
}

// HasChanges 表示两侧取值是否有任何差别，含"只改了重启档"的情况。
func (c ConfigChange) HasChanges() bool {
	return len(c.Hot) > 0 || len(c.Restart) > 0 || len(c.Reject) > 0
}

// HasRejections 表示这次改动是否触碰了拒绝档：调用方据此整次作废，一项都不应用。
func (c ConfigChange) HasRejections() bool {
	return len(c.Reject) > 0
}

// Diff 比较两份配置，得出"该应用的、需要重启的、导致作废的"三份清单。
//
// 两份都必须是 Normalized() 之后的取值，否则 0 值与默认值的差别会被当成一次改动
// （签名不返回 error 但要断言：入参来自 Config.Normalized()，见 §9 风险表）。
// Diff 不改动任何一方，candidate 换入 applied 由调用方负责。
//
// Reject 非空时调用方整次作废；这里仍然把 Hot 填好，方便日志说清"本来会应用哪些"，
// 但那不代表它们被应用了。
func Diff(applied, candidate Config) ConfigChange {
	oldLeaves := flattenLeaves(applied)
	newLeaves := flattenLeaves(candidate)

	// 两侧各有哪些档位条目：条目增删按热更判，不看新条目内部的字段（设计文档 §6.4）
	oldEntries := commandEntryNames(oldLeaves)
	newEntries := commandEntryNames(newLeaves)

	var change ConfigChange
	// 按字典序遍历并逐份追加，三份清单自然就是排好序的，不需要事后排序
	for _, path := range sortedLeafPaths(oldLeaves, newLeaves) {
		oldLeaf, inOld := oldLeaves[path]
		newLeaf, inNew := newLeaves[path]
		if inOld && inNew && sameLeaf(oldLeaf, newLeaf) {
			continue
		}

		key := ChangedKey{Path: path, Old: oldLeaf.any(), New: newLeaf.any()}
		switch classifyChange(path, oldEntries, newEntries) {
		case ClassHot:
			change.Hot = append(change.Hot, key)
			if isCommandPath(path) {
				change.Commands = append(change.Commands, key)
			}
		case ClassRestart:
			change.Restart = append(change.Restart, key)
		case ClassReject:
			change.Reject = append(change.Reject, key)
		}
	}
	return change
}

// classifyChange 在 classify 之上加一条：条目增删走"条目增删本身"那一档热更，
// 不按字段名判。只在一侧出现的档位，它的全部字段叶子都算这次增删带来的。
func classifyChange(path string, oldEntries, newEntries map[string]bool) ConfigClass {
	if name, _, ok := splitCommandLeaf(path); ok && !(oldEntries[name] && newEntries[name]) {
		return ClassHot
	}
	class, ok := classify(path)
	if !ok {
		// 没归档的键（只可能出现在表与实现失去同步的中间态）落到重启档：
		// 接受但不应用、逐条记入 ignored_keys，既不会悄悄换取值也不会让整次作废。
		// 这个方向由 TestEveryLeafKeyIsClassed 保证走不到，运行期兜底只是不让它变成静默。
		return ClassRestart
	}
	return class
}

// commandEntryNames 收集摊出的路径里出现过的档位条目名。
func commandEntryNames(leaves map[string]leafValue) map[string]bool {
	names := make(map[string]bool)
	for path := range leaves {
		if name, _, ok := splitCommandLeaf(path); ok {
			names[name] = true
		}
	}
	return names
}

// sortedLeafPaths 返回两侧路径的并集，按字典序排好。
func sortedLeafPaths(oldLeaves, newLeaves map[string]leafValue) []string {
	paths := make([]string, 0, len(oldLeaves)+len(newLeaves))
	for path := range oldLeaves {
		paths = append(paths, path)
	}
	for path := range newLeaves {
		if _, ok := oldLeaves[path]; ok {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// sameLeaf 判断一个叶子的取值是否没变。
// 空切片与 nil 在这里算两次不同的取值——它们各自表达"没有条目"的写法不同，
// 而 Diff 的调用方只关心"有没有差别",宁可多报一次也不能漏报。
//
// 这里不看 leafKind：同一条路径在两侧都来自同一个 Config 类型，Kind 必然相同，
// 比对它是个不可达分支（M-1 的结论，卡 §10.2 第 14 条）。只有一侧有这个键时
// 另一侧是零值 leafValue，由下面的有效性判断兜住。
func sameLeaf(oldLeaf, newLeaf leafValue) bool {
	if !oldLeaf.Val.IsValid() || !newLeaf.Val.IsValid() {
		return oldLeaf.Val.IsValid() == newLeaf.Val.IsValid()
	}
	return reflect.DeepEqual(oldLeaf.Val.Interface(), newLeaf.Val.Interface())
}

// ReloadResult 是最近一次重载尝试的结论（设计文档 §5.4）。
type ReloadResult string

const (
	// ReloadOK 重载成功，Hot 里的键都已应用。
	ReloadOK ReloadResult = "ok"
	// ReloadUnchanged 读通了配置，但与当前生效的取值没有差别。
	ReloadUnchanged ReloadResult = "unchanged"
	// ReloadRejected 触碰了拒绝档，整次作废，一项都没应用。
	ReloadRejected ReloadResult = "rejected"
	// ReloadFailed 读取、校验或应用失败，applied 保持旧值。
	ReloadFailed ReloadResult = "failed"
	// ReloadDegraded 失败后回滚也没回干净：进程里的取值是混合状态，必须人来看。
	ReloadDegraded ReloadResult = "degraded"
)

// ReloadState 是重载状态的读出形状：产出它的一方是 core.ConfigWatcher（R05）
// 与 cmd/server 的重载链（R06），api 只是把它转写进 /admin/runtime 的响应——
// 所以结构体落在依赖方向的下层（设计文档 §5.4）。本卡只定义类型与五个结论名，
// 不写入、不读取。
//
// 三条约束（设计文档 §5.4）：不含任何凭据内容（键名可以有，值不能有）；Result 是封闭枚举；
// 这是进程内状态，不入库。
type ReloadState struct {
	Enabled       bool         `json:"enabled"`
	WatchedPath   string       `json:"watched_path,omitempty"`
	LastAttemptAt time.Time    `json:"last_attempt_at,omitempty"`
	LastAppliedAt time.Time    `json:"last_applied_at,omitempty"`
	Result        ReloadResult `json:"result"`
	Error         string       `json:"error,omitempty"`
	AppliedKeys   []string     `json:"applied_keys,omitempty"`
	IgnoredKeys   []string     `json:"ignored_keys,omitempty"`
	RejectedKeys  []string     `json:"rejected_keys,omitempty"`
	WatcherError  string       `json:"watcher_error,omitempty"`
}
