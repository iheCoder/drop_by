# 模型费用估算

2026-11-07 核对官方 Standard 文本 token 价格，单位为美元／百万 tokens：

| 模型 | 普通输入 | 缓存读取 | 缓存写入 30 分钟 | 缓存写入 12 小时 | 输出 |
| --- | ---: | ---: | ---: | ---: | ---: |
| GPT-6 Luna | 0.10 | 0.01 | 0.125 | 0.25 | 0.50 |
| GPT-6.1 Terra | 1.00 | 0.05 | 1.25 | 2.50 | 5.00 |
| GPT-6.1 Sol | 2.00 | 0.10 | 2.50 | 5.00 | 10.00 |

来源：[Luna](https://developers.openai.com/api/docs/models/gpt-6-luna)、[Terra](https://developers.openai.com/api/docs/models/gpt-6.1-terra)、[Sol](https://developers.openai.com/api/docs/models/gpt-6.1-sol)、[缓存计费说明](https://developers.openai.com/api/docs/guides/prompt-caching)。输入超过 272,000 tokens 时，整次请求的输入和缓存价格乘 2，输出价格乘 1.5；恰好 272,000 不加价。

Gemini 按 Google Standard 文本费率估算，单位为美元／百万 token。表中的输出包含思考 token。费率生效日按 UTC 计算，同一次调用的预算预留和结算使用同一费率。 [Google 定价](https://aistudio.google.com/docs/pricing) · [Google 推理参数](https://aistudio.google.com/docs/thinking?generate-content=true)

| 模型 ID | `thinkingLevel` | 普通输入 | 缓存命中输入 | 输出 |
| --- | --- | ---: | ---: | ---: |
| `gemini-4-hydrogen` | `low`、`medium`、`high`、`xhigh` | 1.50 | 0.075 | 7.50 |
| `gemini-4-argon`、`gemini-4-carbon` | `low`、`medium`、`high` | 2.00 → 4.00 | 0.10 → 0.20 | 10.00 → 20.00 |
| `gemini-3.8-flash` | `low`、`medium`、`high` | 0.75 → 1.50 | 0.075 → 0.15 | 3.75 → 7.50 |
| `gemini-3.5-flash-lite` | `minimal`、`low`、`medium`、`high` | 0.30 | 0.03 | 2.50 |

Argon 和 Carbon 于 2026 年 12 月 1 日切换到箭头右侧的费率，Flash 于 2027 年 1 月 1 日切换。`promptTokenCount` 包含缓存部分，`toolUsePromptTokenCount` 计入输入，`candidatesTokenCount` 和 `thoughtsTokenCount` 相加得到输出总量。请求前按无缓存命中预留输入费用及最大输出费用。 [原生用量定义](https://aistudio.google.com/api-reference/generate-content#Resource:GenerateContentResponse)

## 如何计入账本

先确定这次调用实际使用的模型，再使用该型号的价格。角色选择不会改变账本归属，不能把 Luna 判断或 Terra 发言按主模型 Sol 的价格结算。原生压缩也采用当前模型的费率，输入计数请求不当成一次生成重复收费。

OpenAI 响应中的 `input_tokens` 包含普通输入、缓存读取与缓存写入，三部分互斥：普通输入 = 总输入 − cached_tokens − cache_write_tokens。按各部分价格计算后，加上 output_tokens 的费用。输出总量已经包含推理 tokens，不再额外添加 reasoning_tokens。缓存写入有时长细分时按 30 分钟、12 小时分别计算。仅定时个人探索对跨活动原样重复的历史请求 12 小时保留。如果网关只返回写入总数，有长标记的请求按 12 小时写入价保守估算，其他请求按默认 30 分钟估算。

例如 1,000 个输入（400 普通、400 缓存读、200 缓存写）和 100 个输出：Luna 为 $0.000119，Terra 为 $0.001170，Sol 为 $0.002340。先以整数纳美元计算整次请求，最后向上取整到微美元，避免每部分各自取整或浮点误差。

请求开始前仍做预算预留。无法预知缓存分布，先按该模型最贵的缓存写入情况和最大输出留额，返回有效用量后释放多预留部分；请求失败、没有 usage 或用量明细矛盾时保留预留，不把未知计费当成免费。总预算、自主预算、跨周结算仍使用现有事务账本。

每个已接入型号都有自己的费率表。选用其他型号需要先登记费率。

## 历史与估算边界

历史账本只有每日汇总，没有逐次模型、缓存和计费层级归属，无法可靠重算。升级保留原账，从更新后的调用开始按模型分别计价，不清空历史，也不凭猜测降低已累计费用。个人中心继续显示现有周汇总，因此升级当周包含原历史估算与新计价。

这是按模型官方 Standard 标价计算的本地预算。网关折扣、加价和服务档位可能改变实付金额。搜索和网页读取继续独立按次估算。

## 已验证

- 同一份用量在三个模型下的具体金额、缓存读取／两种写入时长、输出推理不重复计费及长上下文边界。
- 真实 HTTPModel 角色选择、响应解析、SQLite 预留与结算；旧账保留，总额与自主额度一致。
- 原生压缩加两次续接生成的账本总额，覆盖缓存明细。
- 未知模型、非法缓存分布、请求失败和缺少用量不会被错误地低价结算。

上述检查使用本地 HTTP 测试服务和真实 SQLite，不发起新的付费模型评测。
