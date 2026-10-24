# ADR 0008：按运行绑定的生产 run_python 与离线运行时租约

## 状态与范围

已完成 #43（2026-10-23），最终 PR50 合并 468f355 的 PR/main CI 与三角色验收通过，记录见 lantern-notes W3-05。依赖 #41 的 Linux Runner 与 #42 的 JSON-cell CSV/产物链。不含 #44 完整平台 eval，也不包含 W7 桌面图表展示或持久化。

## 显式 SQL 引用

使用 `core/pythonx.Session` 为一个 workspace/run 注册真实 run_sql wrapper 和 run_python。只有成功的 SQL Content 经适配层验证后才按调用 ID 缓存，Python 参数可选 sql_call_id 引用。拒绝跨 workspace/run，不采用全局 latestSQL，也不让模型提交整份任意 JSON 冒充真实查询结果。

缓存上限 8 MiB/32 条；正在执行的 SQL 预留单次最坏 1 MiB，结果完成后按实际字节计量。重复 ID 不覆盖，失败不产生引用，满额明确失败而非静默换数据。Session 在 Close 前拥有缓存，多个并发工具共用最多两个执行槽，取消排队同样有效。

## 权限与执行

Schema 仅 code 与 sql_call_id，未知字段直接在 Registry 阶段拒绝。网络、额外只读目录、限制和运行时路径不由模型控制，因此默认 Policy 无需假装审批一个未实现的扩权入口；现有 Policy/Approver 仍可拒绝或要求确认本工具。未来开放权限选项须先完成 W6 的对应审批，不能直接透传到 Spec。

可信调用方提供已显式 Ensure 的 Runtime，Execute 不调用 Ensure。缺失或过期环境返回 runtime_unavailable，无宿主执行兜底。`AcquirePrepared` 对既有 lock 文件取得只读共享锁，验证 marker/嵌入锁哈希、base/venv/prefix/stdlib/版本、全部锁定包版本及固定 imports，持锁覆盖 Sandbox.Run，阻止协作的 Ensure 替换环境。验证只用固定 -I -B 片段和白名单环境，临时 HOME/MPLCONFIGDIR 独立且清理，不修改 venv、不 pip、不 sweep。marker 不是抵御同用户任意篡改包代码的内容鉴证。

仅将已验证 Runtime、只读 CSV/metadata/helper 交给真实 Runner；工作区专属执行根、Agg、沙箱内字体缓存、禁网与单任务限制沿用前两轮。Python 通过固定 load_table() 读取位置列与截断信息，不经过 pandas 自动猜测类型。

## 图表返回与生命周期

结果观察值仅有界 stdout/stderr、Usage 与不透明 ID manifest，不把合计 20 MiB base64 塞入模型上下文，也不交付已删临时路径。图表字节在 Session 中保留，GetArtifact 再次核对 workspace/run/ID 并复制。Agent.Run 返回后仍可取图；显式 Close 才结束数据所有权。

Session artifact payload 上限 64 MiB，沿用单次 5 MiB/20 MiB/16 件验证；一批超量整体失败，不插入半套、不驱逐旧图。64 MiB 不是总 heap/RSS 配额。Close 取消并等待活跃/排队工具完成回收与 lease 释放，禁止关闭后提交新产物，再清空字节和数据库连接。

稳定错误从 sandbox/pyrt 的 errcode.Coded 桥接到 tools.Error，保留 oom/timeout/runtime_unavailable/sandbox_unavailable 等码；MaxAttempts=1，不把执行错误算成连续参数失败，也不把原始 traceback、路径或代码写进错误/audit 消息。

## 验收出口

独立测试须在真实 Agent loop 注册真实名为 run_python 的生产工具，完成工具声明→SQL→Python→可取回 PNG/结论，验证失败反馈/取消/恢复和 Schema/Policy 前置拦截。真实已准备 venv 中 pandas/Agg 运行；普通单测不联网，真实开关不可 skip。覆盖缓存/归属/批次预算/Get 副本/Close 并发与共享 lease，关键变异目标红。最终提交 CI、审查正文、产品/开发/独立 QA、合并/main 与 lantern-notes 同轮完成后关闭 #43。
