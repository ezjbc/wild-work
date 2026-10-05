# wild-work v2.6.2 — 修复「面板列出却调不动」：渠道本地模型校验漏看动态目录

> 发布主题：**修复 raccoon / loomy 两个渠道的模型名校验只认静态表**，
> 导致上游新增的模型在 `/v1/models` 正常列出、调用时却被本地 400 拒。
> 本次由**实测嗅探发现**（见下「问题现场」），v2.6.0 / v2.6.1 均受影响。

---

## 🐛 修复

### 问题现场（2026-10-05 实测）

小浣熊渠道加完账号后，把 `127.0.0.1:7863` 的接口逐个嗅了一遍，
`/v1/models` 列出 9 个模型：

```
raccoon-8c4485  raccoon-19b265  raccoon-405a1c
sn-sensenova-6-8-flash        ← 调不动
sn-sensenova-6-8-flash-lite
sn-glm-5-3  sn-kimi-k3  sn-glm-5-3-flash  sn-deepseek-v4-1-flash
```

但逐个调对话时：

| 模型 | 本地网关 | 直连上游 |
| --- | --- | --- |
| `raccoon-8c4485` | 200 OK | — |
| `sn-sensenova-6-8-flash` | **400 `model_not_found`** | **200 正常回答** |
| `sn-sensenova-6-8-flash-lite` | 200 OK | — |

### 根因

两个渠道都做**本地模型名校验**——因为上游对未知模型名**不报错**，
而是**静默回落到默认模型**并返回 200（不校验会让用户以为在用 A 模型、实际消耗 B 模型的额度）。
但校验实现**只查静态表**：

```go
if m := modelOf(body); m != "" && !KnownModel(m) {   // KnownModel 只看 staticModels
    return 400 unknownModelBody(m)
}
```

而渠道的**动态模型列表**（`FetchModels` → `/v1/models` 与费率面板）是**另一条路径**，
它读的是上游 `model_catalog`。于是：

- `/v1/models` 列的是**动态目录**（9 个，含上游新增的）
- `ChatStream` 只认**静态表**（8 个，基于 2026-09 实测）

差集里的模型就成了「**面板列出却调不动**」，且**无声**——
用户看到模型在列表里，调用却 400，而错误文案还说「未知模型」。

> 注：这个 bug 与 v2.6.0/v2.6.1 无关，是 PR #64/#63 本身带来的；
> 但**只有真的把接口跑一遍**才会现形（两边看起来都「正常」）。

### 修法

`fetchCatalog` / `fetchModels` 成功后，把目录里的模型名记进 Client；
`ChatStream` 的本地校验改查 **静态表 ∪ 动态目录**：

- `liveIDs` **单调扩大、只增不减**：目录瞬时拉取失败或上游临时抽掉某模型时，
  不应把已确认可用的模型判成未知。
- 两个渠道同款修复（`internal/raccoon`、`internal/loomy`），都带
  `liveMu sync.RWMutex` 保护（`ChatStream` 会被并发调用）。
- 顺带给两个 Client 加 `Base` 字段（默认走常量端点，测试注入 httptest 假上游），
  避免为了写测试而把 `LLMBase`/`GatewayBase` 从 `const` 改成 `var`。

### 实测验证（修复后）

9 个模型**全部 200**（含此前被误拒的 `sn-sensenova-6-8-flash`）：

```
raccoon-8c4485   200   raccoon-19b265   200   raccoon-405a1c   200
sn-sensenova-6-8-flash   200   sn-sensenova-6-8-flash-lite   200
sn-glm-5-3   200   sn-kimi-k3   200   sn-glm-5-3-flash   200
sn-deepseek-v4-1-flash   200
```

并且都能真正出内容（不是空壳 200）。

---

## 🧪 回归测试

两个渠道各加 `TestKnownModelAcceptsLiveCatalog`：

- 构造一个「不在静态表里」的模型名，断言 `knownModel()` 初始为 false（前提）
- 用 httptest 假上游返回含该模型的目录，跑一次 `FetchModels`
- 断言此后 `knownModel()` 为 true，且 `ChatStream` 不再返回本地 400 `model_not_found`
- 断言**单调扩大**：目录里没出现的已知模型不会被遗忘

去掉并集逻辑（退回只查静态表）该测试即失败。

---

## 📎 升级提示

- **强烈建议升级**：若你用的是 `raccoon/*` 或 `loomy/*`，凡是「上游目录里有、静态表里没有」的模型
  在 v2.6.1 及之前都无法调用（且只会看到一个误导性的「未知模型」400）。
- 无需迁移任何数据。
- 其余渠道（workbuddy / traework / qoder 系 / qwenwork / glm / monkeycode / oczen）不受影响：
  它们要么不在渠道层做本地模型校验，要么其校验口径本就与目录同源。
