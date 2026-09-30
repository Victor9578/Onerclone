代码全部读完了（engine / state / adapters / main / rc / daemon / namemap 逻辑 + PROGRESS 全部踩坑史）。先说结论：**架构骨架是对的，不用推翻**——快照即回声、exec 前复查、Cloud/Local 两条接缝 + fake 单测、幂等队列、分类退避，这些都是真功夫。下面的方案全部是「在你自己的设计原则上走得更彻底」，分四档，共 16 条。

---

## 〇、先说已经做对的（别动）

- **快照即回声防护**（结构性消灭 selfWrite 窗口）——比 aliyunpan 的还干净
- **exec 前复查**（upload 查云端 / download 查本地 / 删除查双侧复活）——这其实是完整的乐观并发要素，后面 P8 会用到它
- **engine 不 import cfapi**，Cloud/Local 接缝 + fake 可测——深模块的雏形已经在了
- namemap 的「engine 键 = 云端名」设计——快照/队列/冲突逻辑零感知，locality 极好

---

## 一、正确性隐患（优先，都是「违反你自己定的原则」）

**P1 · `refreshCloudSnapFromLocal` 违反坑 #9 的「各归各」原则**
[engine.go:655](internal/engine/engine.go#L655) 上传成功后把**本地 stat 写进 cloud_snap**。但你们自己的铁律是 local_snap=本地实测、cloud_snap=云端观测。quark「不能改已存在远端文件 mtime」——若上传后云端上报的 mtime 与本地不同，下轮 Poll 就判「云端变了」→ **把刚上传的文件原样再下载一遍**。本地后端测试测不出（local 保留 mtime）。方案：上传成功后补一次 `cloud.Stat`，把**观测值**写进 cloud_snap。一次廉价的 RC 调用，消灭一类回环，对刚开放的 80+ 后端（各自 mtime 语义不同）尤其值钱。建议先在真机 quark 上验证一次上传后的 mtime 语义。

**P2 · `WasHydrated` 启发式失真，Q6 语义名不副实**
[adapters.go:305](cmd/spike/adapters.go#L305) 用 `size>0` 判「已水合」——但占位符的 stat size 就是创建时给的逻辑 FileSize，**未水合占位符也恒 >0**。结果：`execDownload` 里 `hydrate := lok && WasHydrated` 对「从未打开过的占位符」也成立 → 云端每次改动都全量重拉。Q6 的本意「曾水合才重拉、不白拉流量」实际只对新文件成立。方案（二选一）：
- **优雅版**：水合状态自己记账——所有水合必然经过你的 FETCH_DATA 回调（Explorer 的「始终保留在此设备」pin 也走它），在回调完成时给 local_snap 打 hydrated 标记，ApplyDownload 重建/脱水时清掉。零新 syscall，状态本来就在你手里
- 保险版：绑 `CfGetPlaceholderInfo` 拿权威水合状态（你们进程已 Connect，读得到真实状态，坑 #7 结论），做 12h 对账用

**P3 · 首轮同步全量重传**
首启时 local_snap 为空 → Scan 把**所有**本地文件入队 upload；execUpload 的复查只比「云端 vs cloud_snap」——一致就照样覆盖上传（[engine.go:630-641](internal/engine/engine.go#L630)）。云端已有逐字节相同的文件也重传。10 万文件首启 = 全量上传。方案（两件套，都在 execUpload 里）：
1. 复查加一条：`ce != nil && size 相等 && mtime 在容忍窗内` → 跳过上传，只做收尾
2. **基线采纳**（OneDrive 首同步同款行为）：size 相等但 mtime 有偏差时，`os.Chtimes` 把本地占位符 mtime 对齐云端观测值——元数据收敛代替数据传输

**P4 · 两个小项**
- `conflictName` 秒级时间戳：同秒两次冲突、或与用户既有文件撞名 → copyfile 静默覆盖冲突副本。加存在性探测 + 序号后缀
- queue 的 done 行没有 GC（PROGRESS 说「保留 7 天」，但没有清理代码）。行数被 UNIQUE(path,kind) 限住不会无限涨，但已删文件的 done 行永久滞留

---

## 二、性能 / 10 万文件目标（结构性，O(N) → O(Δ)）

**P5 · `writeSnap` 逐行无条件 upsert 且无事务** —— 当前最大单点开销
[engine.go:235](internal/engine/engine.go#L235) 每轮把 nowMap 里**所有条目**（含未变的）逐条 `Exec`，每条一个自动提交事务。10 万文件 = 每轮 10 万次独立事务（每次都是 WAL 写）。而 old snap 就在内存里，diff 是免费的。方案：只写变化行 + 整批一个事务。预计两个数量级提升，改动半径只有这一个函数。

**P6 · `AllSnap` 每轮全表加载 ×3**
一次 Poll = scanLocked 读 1 次 local_snap + Poll 自己再读 cloud_snap、local_snap（[engine.go:265-292](internal/engine/engine.go#L265)）。10 万文件 ≈ 20MB×3 分配，60s 一轮，GC 压力可观。方案：Engine 内做持锁世代缓存（本轮内复用），或 store 提供增量游标读。

**P7 · `execDeleteLocal` 为找一个文件做全量 Scan**
[engine.go:808](internal/engine/engine.go#L808) 调 `e.local.Scan()`（含 convergeNames 整树 walk + 改名收敛）然后线性搜一个路径。`e.local.Stat(rel)` 就是干这个的，O(1)。疑似顺手写出，直接换。

**P8 · 引擎全局锁跨网络 I/O** —— 并发模型的核心改造
[engine.go:544](internal/engine/engine.go#L544) `exec()` 持 `e.mu` 期间执行 `cloud.Upload()`——50GB 一传几十分钟，期间 Scan / Poll / 防抖 flush 全阻塞（watcher 的 AfterFunc goroutine 会越积越多）。而**前复查机制已经把乐观并发的全部要素备齐了**，这把锁只是历史保险。方案：把「单写者」收缩为「单写者=状态写入」，执行拆三段——持锁复查+标记 inflight → **放锁做 I/O** → 持锁回写结果。前复查从兜底升级为唯一防线（它本来就是干这个的）。配套两件事：
- worker 并发（现在 batch=8 仍串行传，多小文件场景吞吐上不去）
- 并发后 `ReapInflight(2min)` 会误杀活着的大传输——执行期间要心跳刷新 inflight 的 updated 时间戳

**P9 · 事件驱动增量扫描** —— 「优雅」的最大单步
现在 watcher 攒了事件路径、防抖结算后**全部丢掉**只触发全量 Scan（[main.go:760-782](cmd/spike/main.go#L760)），而且 Poll 每轮也先跑全量 scanLocked——等于本地侧本来就是轮询全扫，watcher 只省延迟。方案分两级：
- **近期**：防抖结算把脏路径集合传给 `Scan(dirty)`，只 Stat 这些路径；全量扫描降级为 12h 对账兜底
- **远期（正路）**：NTFS **USN Change Journal**——增量、可靠、重启可续、天然带重命名事件（OneDrive 同款）。fsnotify 的 ReadDirectoryChangesW 缓冲在大批量拷入时会丢事件，现在靠 Poll 全扫兜着；USN 之后这个兜底也能省。DR1 里你们本来就提过 USN，后来被快照方案替代——其实两者不冲突：USN 管「发现」，快照管「对账」

**P10 · 云端侧增量（Q15c 落地）**
`ListRecursive` 逐目录全量列举。quark 无 changelog，但目录列举带 mtime——适配器可做**目录剪枝**（父目录 mtime 未变 → 跳过整棵子树）。接口先不动（一个 adapter 的接缝是假设性的）；等 10 万压测数据说话，再考虑把 `List()` 升级成 `Changes(since)` 语义。

**P11 · 水合数据面流式化**
`RangeGet` 每区间一次 HTTP GET + 整段 `[]byte` 进内存（[rc.go:140](internal/rclone/rc.go#L140)）。FETCH_DATA 是并发回调（DR7），大文件并行水合时内存峰值 = 并发数 × 区间大小。方案：`TransferData` 出流式变体（io.Reader 分块回填）；可选相邻区间合并/预读。优先级中——先等 50GB 单文件真机表现再定。

---

## 三、模块深度（把知识挪到正确的接缝）

**P12 · `classify` 字符串匹配下沉到 rclone 适配器**
[engine.go:583](internal/engine/engine.go#L583) 靠 `strings.Contains(s, "401")`、`"cookie"` 分类——rclone 的错误面知识散落在引擎里，是 locality 的反例；开放 80+ 后端后每个后端的报错措辞都是新的坑。方案：`cloudRC` 适配层把 RC 错误映射成类型化错误（`ErrAuth / ErrRateLimit / ErrNotFound / …`），engine.classify 变 type switch。**错误分类是 rclone 的知识，就该住在 rclone 适配器里。**

**P13 · cmd/spike 巨包拆分**
engine 的接缝很干净，但两个 adapter（cloudRC/localFS）+ 水合回调 + 面板 + 登录全挤在 package main。方案：`localFS` → `internal/localfs`；**`handleFetchData` → `internal/hydrator`**（注入 RangeGet 函数即可单测——现在这条最关键的路径是零测试的）；cmd 只留装配。改动就是搬文件 + 换 import，风险低。

**P14 · 脱水/更新走官方 API**
现在脱水 = 删文件 + 重建占位符（`download(rel, false)`）。语义可行，但有窗口期（文件短暂消失，正开着它的应用会懵）+ 元数据 churn（坑 #22 的教训：占位符创建者必须唯一——现在是串行的所以安全，但这是靠锁撑着的）。方案：cfapi 层补 `CF_OPERATION_TYPE_DEHYDRATE` / `CfUpdatePlaceholder`；「内容未变只有元数据变」的场景也可原地更新而非删重建。

**P15 · rclone 客户端加共享调度**
水合（交互式，用户正盯着等文件打开）和队列传输（批量）打同一个 rcd/quark，无优先级；quark 风控（1.5s 节流）也没有统一收口。方案：在 `rclone.Client` 这个接缝加轻量优先级/限流层——FETCH_DATA 优先、批量让路。P8 的 worker 并发也该由它来 gate。

**P16 · 重命名检测（省流量大招，带风险）**
本地改名 = 删除 + 新建 → 云端删 + **全量重传**。quark 无 hash，但「删 A 建 B 且 size 相同 + mtime 精确相等」可启发式判为改名 → 服务端 move（零流量）。风险：同 size 同 mtime 的不同内容被误 move——把条件收紧到 mtime **精确相等**（不是 1s 容忍窗）可把概率压到可忽略。aliyunpan 的「size+mtime 未变复用捷径」同思路。

---

## 四、产品级补全（顺手档）

- **忽略规则**：`.onerclone-ignore` 选择性同步——`isTempPath` 的调用位置正好就是通用过滤器的天然接缝（watcher / Scan / convergeNames 三处统一）
- **带宽控制**：面板接 `core/bwlimit` 动态设置（rcd 本来就支持）
- **state 读写分离**：`SetMaxOpenConns(1)` 让面板读和引擎写在同一条连接上排队；WAL 本就支持单写多读——开写者+读者两条连接，面板就不会被 P5 的大事务卡住（注意与 P5 一起落地）
- 面板 2s 轮询 → SSE（纯 polish，最后再说）

---

## 建议的实施顺序

| 批次 | 内容 | 理由 |
|---|---|---|
| ① 止血 | P1 + P2 + P4 | 都是几行~几十行的改动，消灭回环下载和白拉流量 |
| ② 提速 | P5 + P6 + P7 + P20(读写分离) | 集中在 state/engine 两个文件，收益立竿见影，为 10 万压测铺路 |
| ③ 并发 | P8 + P15 | 动引擎核心循环，前复查机制已在，风险可控 |
| ④ 优雅 | P9(增量) + P13 + P12 | 结构性升级，做完同步才算「事件驱动」 |
| ⑤ 远期 | P3、P10、P11、P14、P16 | 按真机压测数据排优先级 |

要不要我把这份方案整理成 `PROPOSALS.md` 落进仓库（或按你们的习惯追加到 PROGRESS.md 新章节）？另外 P1（quark 上传后 mtime 语义）需要一次真机验证才能定案——你跑一次上传后 `rclone lsl quark:` 对比本地 mtime 就能确认。