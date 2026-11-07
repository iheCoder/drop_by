# 接入与依赖审查

日期：2026-11-06。范围：本项目实际使用的模型 API、推理与工具协议、运行限制、网络路径、Go/SQLite 依赖。不是对所有可能技术方案的全面认证。

## 确认的问题与处理

| 项目 | 之前的问题 | 本轮处理与证据 |
| --- | --- | --- |
| OpenAI API | 沿用了 Chat Completions，只验证简单生成，未先验证推理加工具的组合；Sol 开启推理后真实返回 HTTP 400 | 官方 OpenAI 地址的所有调用统一使用 Responses；真实完成初步回复、后台工具行动、保存成果和后续分享 |
| 模型选择 | 默认 gpt-5.4-mini，把成本因素放在了用户希望的体验之前，没有对初步回应和深入研究分配不同推理量 | 使用账号可见且实际调用成功的 gpt-5.6-sol，首句与内部简短判断用 none，后续行动用 high；不把官网最新模型等同于该账号可调用模型 |
| 工具往返 | 旧消息结构只能携带文本和 function call，无法保留 Responses 推理项目 | 保留完整 output 项目，并回传 function_call_output/call_id；加密推理项目仅用于同一行动的后续请求，不公开给用户 |
| 输出额度 | 2200 token 与关闭推理的旧配置没有为深入推理预留空间 | 快速请求 8000，深入请求 25000，均为输出与推理总额度；不要求公开回复写满这些额度 |
| 请求预算 | 旧的 1 美元、120 次调用限制与用户目标不匹配 | 按用户明确确认调整为每日 3 美元；600 次是第二道执行上限，费用限额仍先约束请求；预留基于最终实际发送的请求体 |
| 超时 | 75 秒来自旧的即时生成路径，对深入行动可能过短 | 单个模型 HTTP 请求最多 4 分钟，仍受整项工作期限与用户取消控制；不会无限等待 |
| 系统代理 | Python 验证自动走 macOS 系统代理，但 Go 后台没有读取系统代理，造成验证路径与运行路径不同 | 当前后台显式继承已有本机 HTTP(S) 代理，README 记录手动重启方式；随后通过应用生成接口和完整真实流程验证 |
| SQLite 驱动 | 最初固定在 modernc.org/sqlite v1.38.2；本次模块查询确认已有 v1.61.1 | 升级到 v1.61.1，按驱动依赖升级 libc、memory 等，并运行持久化测试与原有数据库读取检查 |
| 间接依赖 | x/sys v0.34.0 命中 GO-2026-5024，影响 Windows 的 NewNTUnicodeString；当前 macOS 应用没有调用受影响代码 | 随 SQLite 升级到 x/sys v0.49.0；升级后 govulncheck 没有报告已知漏洞 |

## 核查后保留的选择

- **Chat Completions 兼容代码**仍留给其他兼容服务，官方 OpenAI 默认路径不再使用它。官方说明该接口仍受支持，但新项目和推理工具工作流应优先使用 Responses。依据：[OpenAI 迁移说明](https://developers.openai.com/api/docs/guides/migrate-to-responses)。
- **JSON object 输出**仍受支持，不因为名称较早就一律替换。当前应用检查 JSON 解析、工具权限、引用范围与实际落盘结果；尚未声称对每一种模型输出都实现了严格 schema 保证。
- **DeepSeek Search/Fetch**仍是当前官方独立工具接口，请求路径和每千次成功请求 0.75 美元的费率均已核对。保留是因为已有真实搜索、网页读取和执行记录链路；它不会使用 OpenAI 赠送额度。依据：[DeepSeek 官方文档](https://api-docs.deepseek.com/guides/search/)。
- **Go 标准库 HTTP、原生前端和 SSE**没有因使用传统组件而失效。此处没有引入旧版 OpenAI SDK、Assistants API 或已废弃 Completions API。前端 SSE 传送的是持久化内容变更通知，不是模型的逐 token 流。
- **本地会话管理**仍保存在 SQLite。Responses 使用 `store=false` 并回传加密推理项目；这控制响应状态保存方式，不等于对供应商全部日志政策作出零保留承诺。依据：[推理上下文说明](https://developers.openai.com/api/docs/guides/reasoning)。

模型费用现按 Luna、Terra、Sol 各自的 Standard 费率及响应中的缓存明细估算，详见[模型费用估算](model-pricing.md)。应用没有读取赠送余额、到期日或正式账单；供应商报价变化时需要更新费率表。

## 检查记录与局限

- 常规 24 个测试通过竞态检查；Go vet、构建、前端语法检查与 diff 检查通过。
- Responses 的并发配置隔离、推理项目续传、工具 call_id 对应和首句工具限制有确定性回归测试。
- 实际 Sol 样例：15.37 秒首句、43.21 秒保存草稿、55.22 秒分享结果。属于单样例观察，不是秒回或性能分位数保证。
- `govulncheck -show verbose ./...` 扫描 Go 1.27.3 标准库和当前依赖，升级后没有已知漏洞报告。该结果不代表没有未知漏洞，也不替代完整安全审计。
- 仍需持续观察群聊自然度、跨场景隐私判断、长时间运行与实际额度消耗。不能用一次 API 成功或一个短流程代替这些验收。

本机证据在被 Git 忽略的 `test-results/openai-responses-pacing.txt`、`vulnerability-audit-before.txt`、`vulnerability-audit-after.txt`。代码提交不包含 API 密钥或运行数据库。
