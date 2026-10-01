package executor

import (
	"strings"

	"godelayq/core"
)

// ProfileWarning 是一条没能从档位文件进到登记表的记录，以及为什么。
//
// 它不进表但仍然要说出来：页面上建好、重启后不见了的档位，如果启动日志一行都不留，
// 现场就只能靠手改文件复现。调用方（装配层）负责把它记成 warn。
type ProfileWarning struct {
	// Name 是记录里的档位名，原样带出，便于运维对着文件找
	Name string
	// Reason 可直接展示，取自校验/转换链路的错误文本
	Reason string
}

// MergeStoreProfiles 把档位文件里的记录换成待登记的 store 条目：逐条按宽松路径模式
// 校验、探测，并给每条标上来源。
//
// 它与 config 侧的差别只在**错误范围**，规则本身一份都没多（设计文档 §4 的 I1）：
//   - config 侧走 LoadProfiles，任一条非法即整个进程起不来（executors.commands 是人手写的
//     部署配置，写错了必须当场暴露）。
//   - store 侧一条非法只跳过那一条，其余照常进表，被跳过的进 warnings。
//     这条不对称是故意的：档位文件由页面写入，一次保存不该让整个队列停摆，
//     而"保存时已经过同一个 BuildProfile 校验"意味着走到这里还非法的记录，
//     多半是文件被手改坏或换机器后配置对不上——那种情况下把其余档位跑起来更有用。
//
// 与 config 侧同名（注册键相同）的条目保留在结果里，但标成 Degraded：
// config 赢、store 不注册处理函数（D4 与 §5.2，即待拍板 P1 取的答案）。
// 降级原因由 Registry.ApplyStore 统一给文案，这里留空免得两处各写一句。
//
// 顺序沿用 core.ExecutorProfileStore.List 的口径（按档位名忽略大小写的字典序），
// 不再二次排序；登记表自身的键有序性由 snapshot 负责。
//
// cfg 里 executors 一节的取值以归一化后为准（workspace、runtime_allow 都会参与校验）。
func MergeStoreProfiles(cfg core.Config, stored []core.ExecutorProfileRecord) ([]StoreEntry, []ProfileWarning) {
	executors := cfg.Normalized().Executors

	// 判撞名只需要 config 侧声明了哪些名字，不需要把它们校验一遍——
	// 校验是 LoadProfiles/NewRegistry 的事，这里再跑一遍等于把 config 侧的连坐规则
	// 搬到 store 侧的合并路径上，那条不对称正是本函数要守的东西。
	configKeys := make(map[string]bool, len(executors.Commands))
	for _, cmd := range executors.Commands {
		if strings.TrimSpace(cmd.Name) == "" {
			continue
		}
		configKeys[HandlerKeyPrefix+cmd.Name] = true
	}

	entries := make([]StoreEntry, 0, len(stored))
	var warnings []ProfileWarning
	for _, record := range stored {
		cmd, err := record.Command()
		if err != nil {
			warnings = append(warnings, ProfileWarning{Name: record.Name, Reason: err.Error()})
			continue
		}
		profile, err := BuildProfile(cmd, executors, PathAnywhere)
		if err != nil {
			warnings = append(warnings, ProfileWarning{Name: record.Name, Reason: err.Error()})
			continue
		}

		entries = append(entries, StoreEntry{
			Profile:  profile,
			Probe:    Probe(profile),
			Source:   SourceStore,
			Degraded: configKeys[profile.HandlerKey()],
		})
	}

	return entries, warnings
}
