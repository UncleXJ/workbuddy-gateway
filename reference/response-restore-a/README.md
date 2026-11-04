# A 参考方案：只还原自称句式

状态：实验性参考实现，不是本次生产 PR 的合并候选。

## 设计目标

现有 `identity.replacements` 会在入站请求中做较大范围的文字替换。如果响应侧直接做完全对称的反向替换，也可能把正文中真实出现的产品名一起改写。

A 尝试采用更窄的策略：

- 从原始请求推断客户端身份；
- 只还原出现在自称句式中的 target；
- 不处理普通产品名引用。

当前快照支持的表达包括 `I am X`、`I'm X`、`We are X`、`my name is X`、`我是 X`、`我叫 X`，以及少量 Markdown 包裹形式。

## 实现结构

参考实现分为三层：

1. 请求身份推断
   - 只读取原始 `instructions`、`system`、`developer`。
   - 从 `You are X`、`You're X` 或对应中文结构提取客户端名称。
   - 不读取 user 或 tool 消息。

2. 整段文本还原
   - 用正则匹配完整的自称句式。
   - 只替换捕获组中的 target。
   - 普通正文中的产品名不进入替换。

3. 流式暂存
   - 被分片切开的引导词或身份名需要暂存到下一分片。
   - 每个通道分别维护 pending 文本、最近原文上下文和对应还原长度。
   - `content` 和 `reasoning_content` 相互隔离。
   - 流结束时通过 flush 释放尚未完成的候选。

参考实现接入了三条响应路径：

- Chat Completions 流式与聚合；
- OpenAI Responses 流式与聚合；
- Anthropic Messages 流式与聚合。

## 实现困难

### 自称表达不是有限集合

模型还可能使用 `running as`、`this is`、`speaking as`、`As X` 或完整的修辞句。固定词表无法全部覆盖，继续扩充规则也会增加误判和维护成本。

### 产品名与身份的歧义

方案 A 试图避免产品名误伤，但前提是身份引用一定落在已知自称结构中。这个前提无法保证。

### 流式状态协调

识别跨分片句式时，必须同时维护：

- 当前句式尚未完成的文本；
- 已经发出的最近原文上下文；
- 原文在改写结果中的长度。

这些状态必须在分片切换、正文/推理通道切换和 flush 时保持一致。

### 重复和重叠身份短语

重复身份片段跨分片时，修补一个位置可能让另一个切分方式失败。当前参考快照仍存在这类问题。

### 身份来源不完整

当前快照仍使用自己的 `You are X` 解析器，没有把现有字面 `identity.replacements` 作为客户端身份的唯一事实来源。因此自定义或特殊客户端声明仍可能无法识别。

### 跨协议聚合

流式结果必须与同一快照的整段还原结果一致。Chat、Responses 和 Messages 的事件边界与 reasoning 通道不同，只测试一条路径不够。

## 实测结果

参考文件：`source/identity_restore.go`

SHA256：

```text
9c34e9ae0b48e4de6d16986ffc3c3d8471735483083528b2edbb5626709fb879
```

测试条件：seed `7`，3000 组生成文本，随机 1 到 4 字节切片，对照同一快照的 `rewrite(text)`。

结果：`10 / 3000` 个流式一致性失败，占 `0.33%`。

首个失败：

```text
input="**WorkBuddy**Codex,workbuddy我叫WorkBuddyI am WorkBuddy这是普通正文。OK"
got="**WorkBuddy**Codex,workbuddy我叫CodexuddyI am WorkBuddy这是普通正文。OK"
want="**WorkBuddy**Codex,workbuddy我叫WorkBuddyI am WorkBuddy这是普通正文。OK"
```

## 包内文件

- `a-reference.patch`：可应用到 clean v1.14.43 的完整参考 patch。
- `source/identity_restore.go`：参考还原实现。
- `source/identity_restore_test.go`：原参考测试。
- `source/a_recheck_test.go`：3000 组自有语义一致性诊断。
- `source/bench_test.go`：参考实现性能测试。
- `source/identity.go`、`source/main.go`、`source/messages.go`、`source/responses.go`、`source/runtimeconfig.go`：集成修改。

## 应用和测试

在 clean v1.14.43 源码目录中：

```sh
git apply /path/to/a-reference.patch
go test ./... -run TestResponse -count=1
go test ./... -run TestARecheckStreamingParity -count=1
```

第一条命令应通过。第二条命令会复现已知的 `10 / 3000` 失败，用于暴露限制，不代表 A 已经是绿色实现。
