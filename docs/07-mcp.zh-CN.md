# 07 — MCP 服务端点(Model Context Protocol)

[English](07-mcp.md) | **简体中文**

把 onvif-proxy 的管理能力(设备增删改查、RTSP 探测、快照、ONVIF 自检等)以
MCP(Model Context Protocol)工具的形式暴露给 AI 客户端(Claude Code / Claude
Desktop 等),使其可以直接管理本代理:例如"帮我把这个 RTSP 流加成虚拟摄像机
并跑一遍自检"。

- 实现:**官方 Go SDK** `github.com/modelcontextprotocol/go-sdk`(当前锁定
  **v1.7.0**,Google 合作维护)。这是项目"第三方依赖仅 yaml.v3"约束的
  **唯一例外**,理由:MCP 协议面(生命周期/会话/SSE/多版本协商)由官方库长期
  跟进,自造收益低;该例外已同步写入 CLAUDE.md 与 docs/01/06。
- 依赖账目:`go.mod` 中**直接**依赖恰好两个 —— `gopkg.in/yaml.v3` 与
  `github.com/modelcontextprotocol/go-sdk`。`go.mod`/`go.sum` 里其余所有条目
  (`golang.org/x/sync`、`golang.org/x/time`、`golang.org/x/oauth2`、
  `golang.org/x/sys`、`github.com/google/jsonschema-go`、
  `github.com/segmentio/asm`、`github.com/segmentio/encoding`、
  `github.com/yosida95/uritemplate/v3`)都标注为 `// indirect`——它们是
  go-sdk 自己的间接依赖,随 go-sdk 升到 v1.7.0 自动带入,并非本项目自行新增
  的直接依赖。**这不违反 CLAUDE.md 中"第三方依赖仅 yaml.v3 与 go-sdk"这条
  硬约束**(CLAUDE.md 第 24 行);go-sdk 升级导致 go.sum 变长是预期行为,不能
  作为约束被破坏的证据。
- 传输:Streamable HTTP,单端点 `/mcp`,挂在现有 Web UI 的 mux 上
  (与 REST API 同端口),**无状态模式**(`Stateless: true`,不签发
  `Mcp-Session-Id`)。协议版本协商、405/会话/SSE 细节全部由 SDK 处理;
  请求体上限为 1 MiB。
- 认证:复用 `withAuth` —— 配置了 `web.username` 时全站 Basic 认证,`/mcp`
  自动被覆盖。
- Go 版本:SDK 要求 Go ≥ 1.25,go.mod 与 Dockerfile 构建镜像同步上调
  (`golang:1.26.5-alpine`,见 docs/05)。

客户端接入示例:

```bash
claude mcp add --transport http onvif-proxy http://<host>:8080/mcp
# 开启了 web basic auth 时:
claude mcp add --transport http onvif-proxy http://<host>:8080/mcp \
  --header "Authorization: Basic $(printf '%s:%s' user pass | base64)"
```

## 1. 能力声明

`initialize` 响应由 SDK 生成;`serverInfo` 为
`{name: "onvif-proxy", version: <Status().Version>}`;仅注册 tools 能力
(不提供 resources/prompts);`ServerOptions.Instructions` 设为:

> onvif-proxy management server: wrap RTSP streams as virtual ONVIF cameras.
> Use list_devices/get_status to inspect, probe_rtsp/get_stream_info before
> adding a device, add_device/update_device/delete_device to manage,
> run_onvif_selftest to verify.

## 2. 工具集

12 个工具,全部注册在同一个 `mcp.Server` 上。JSON 结果以 text 块承载(序列化
后的 JSON 字符串);快照以 image 块承载。输入结构体用 `jsonschema` tag 描述,
schema 由 SDK 自动生成。

| 工具 | 参数 | 结果 | 对应能力 | Annotations |
|---|---|---|---|---|
| `get_status` | 无 | Status JSON(version/advertise_ip/uptime_seconds/ffmpeg) | `Backend.Status` | readOnly |
| `list_devices` | 无 | deviceView 数组 JSON(与 `GET /api/devices` 同构) | `Backend.Devices` | readOnly |
| `get_config` | 无 | config.yaml 原文(text) | `Backend.ConfigYAML` | readOnly |
| `apply_config` | `yaml` string 必填;`dry_run` bool 默认 false | 成功:`"applied"` / `"valid"`;校验失败:isError + 错误原文 | `Backend.ApplyConfig` | idempotent |
| `add_device` | `device` object 必填(结构同 `POST /api/devices` body,见 docs/04) | 成功:`"applied"`;校验失败:isError | 复用 `buildDevice` + `ApplyConfig` | *(无 —— 非幂等)* |
| `update_device` | `uuid` string 必填;`device` object 必填 | 同上;uuid 不存在:isError | 复用编辑逻辑(保留 uuid/mac/serial/info;auth 密码留空时保留;同名流省略 RTSP userinfo 时保留) | idempotent |
| `delete_device` | `uuid` string 必填 | 同上 | 复用删除逻辑 | destructive, idempotent |
| `probe_rtsp` | `url` string 必填,须是合法 `rtsp://` URL(否则被拒,见下文) | rtsp.Result JSON | `rtsp.Probe` | readOnly, openWorld |
| `get_stream_info` | `url` string 必填,须是合法 `rtsp://` URL(否则被拒,见下文) | ffprobe 流信息 JSON;ffprobe 缺失:isError | 同 `POST /api/test/streaminfo` | readOnly, openWorld |
| `take_snapshot` | `uuid` string 必填;`stream` string 可选 | image 块(base64 JPEG);失败/无 ffmpeg:isError | `Backend.Snapshot` | readOnly, openWorld |
| `run_onvif_selftest` | `uuid` string 必填 | `[]onvifCheck` JSON(method/http_status/soap_fault/pass) | `runONVIFSelfTest` | readOnly |
| `get_discovery_log` | 无 | WS-Discovery 日志 JSON | `Backend.DiscoveryLog` | readOnly |

`probe_rtsp` 与 `get_stream_info` 在做任何事之前都先把 `url` 参数交给
`validateRTSPURL`(mcp.go:211/223)校验:凡不能解析为 `rtsp://<host>...`
(scheme 不对或缺 host)的输入一律作为业务错误(isError)拒绝,因此这两个工具
不会被用作针对任意 URL 的通用端口扫描器。

错误语义:

- **参数非法 / 未知工具** → SDK 层协议错误(客户端收到 JSON-RPC error)。
- **业务失败**(探测失败、配置校验不通过、ffmpeg 缺失、uuid 不存在、
  `rtsp://` URL 非法等)→ 工具结果 `isError: true`,content 为错误原文。在
  go-sdk 中,handler 返回非 nil error 即映射为 isError 结果,直接利用该机制。

### 2.1 工具 annotations

上表每个工具都携带一个显式的 `ToolAnnotations`(`mcp.go` 中
`mcpInstructions` 之后的一组 `anno*` 变量)。这么做的原因是 MCP 规范里
**未标注**的默认值恰好是最坏情况:`readOnlyHint` 默认 `false`、
`destructiveHint` 默认 `true`——不标注就意味着客户端会把该工具当成"可能有
破坏性",纯只读工具也不例外。给全部 12 个工具标注就是为了消除这种误判噪音:

- **只读**(`readOnlyHint: true`):`get_status`、`list_devices`、
  `get_config`、`get_discovery_log`——只读本地代理状态。
- **只读且访问外部世界**(`readOnlyHint: true, openWorldHint: true`):
  `probe_rtsp`、`get_stream_info`、`take_snapshot`——仍是读操作,但这个读会
  触达一个外部实体(调用方给定/配置的 RTSP 摄像机),而不只是本地状态。
- **只读且显式标为封闭世界**(`readOnlyHint: true, openWorldHint: false`):
  `run_onvif_selftest`——它调用的是代理自身在 `127.0.0.1` 上的 ONVIF SOAP
  端点(见 `onvif_selftest.go`),不是外部/陌生实体,所以把 `openWorldHint`
  显式钉在 `false`,不让它落回默认的 `true`。
- **有副作用、非破坏**(`readOnlyHint: false, destructiveHint: false`):
  `add_device`、`update_device`、`apply_config`。其中 `update_device` 与
  `apply_config` 还带 `idempotentHint: true`(按 uuid 替换 / 应用完整配置,
  重复执行都收敛到同一状态);`add_device` **不是**幂等的——每次调用都会
  追加一个在 reload 时重新生成 uuid/mac/serial 的新设备,同样的参数调用两次
  会产生两个设备而不是一个。
- **破坏性**(`readOnlyHint: false, destructiveHint: true`):
  `delete_device`。仍标 `idempotentHint: true`:对一个已经删除过的 uuid
  再删一次,最终状态收敛一致(设备都不存在),即便第二次调用本身会报业务
  错误。

**警告:annotations 只是给客户端 UI 用的提示,不是安全边界。** MCP 规范明确
要求客户端不得依据来自不受信任服务端的 `ToolAnnotations` 做工具使用的
**决策**,因为恶意服务端完全可以谎报这些值。加上这些 annotations 不会改变
`mcp.go` 里任何实际的安全校验——`validateRTSPURL` 与 `withOriginCheck`
在每次调用时依旧照常执行,不受某个工具 annotations 内容的影响。

## 3. 实现契约(internal/web)

新文件 `mcp.go` + `mcp_test.go`,`devices.go` 两处小重构;REST 行为不变。

**`mcp.go`**:

- `func (s *Server) mcpHandler() http.Handler`:构建 `mcp.Server`
  (`mcp.NewServer(&mcp.Implementation{...}, &mcp.ServerOptions{Instructions: ...})`),
  `mcp.AddTool` 注册 §2 全部工具,返回
  `mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { ... }, &mcp.StreamableHTTPOptions{Stateless: true})`。
- 工具 handler 统一形态:输入为带 `json`/`jsonschema` tag 的结构体;文本结果用
  小助手 `mcpJSON(v any) *mcp.CallToolResult` 包装(string 原样,其余
  `json.Marshal`)。
- 每设备工具(take_snapshot / run_onvif_selftest / update / delete)先
  `s.findDevice(uuid)`,不存在返回 error("no configured device with that uuid")。
- `probe_rtsp` 与 `get_stream_info` 先调用 `validateRTSPURL(in.URL)`
  (mcp.go:211/223),在发生任何网络 I/O 之前拒绝非合法 `rtsp://` URL——为何
  这条校验不因 annotations 而弱化,见 §2.1。
- `getServer` 中注册的每个 `mcp.Tool{}` 都带 `Annotations` 字段,指向
  `mcpInstructions` 旁声明的共享 `anno*` 变量之一(`annoReadOnly`、
  `annoReadOnlyOpenWorld`、`annoSelfTest`、`annoWrite`、
  `annoWriteIdempotent`、`annoDestructive`)——分类与理由见 §2.1。
- **API 以 `go doc github.com/modelcontextprotocol/go-sdk/mcp <Symbol>` 输出为
  准**(AddTool 泛型签名、CallToolResult/Content 字段、StreamableHTTPOptions),
  不要凭记忆写。
- SDK 若未内置 Origin 校验(防 DNS rebinding,MCP 规范 MUST 项),在挂载处包一
  层:带 `Origin` 头且其 hostname 既非回环(localhost/127.0.0.1/::1)也不等于
  请求 `Host` 的 hostname 时回 403;不带 `Origin` 放行。

**`web.go` 注册**(`New` 中):`mux.Handle("/mcp", s.mcpHandler())`
(不限方法,GET/DELETE 语义由 SDK 处理)。

**`devices.go` 重构**:

- 提取 `func (s *Server) deviceViews() []deviceView`,`handleDevices` 与
  `list_devices` 共用。
- 提取 `func (s *Server) applyParsedConfig(cfg *config.Config) error`
  (marshal + `Backend.ApplyConfig`),原 `applyConfig(w, cfg)` 改为调用它;
  MCP 的 add/update/delete_device 也调用它。

并发安全:一切配置变更都经 `Backend.ApplyConfig`(main 的 manager 内有锁),
MCP 不引入新的写路径。

**`mcp_test.go`** 覆盖(经 `httptest` + SDK 客户端
`mcp.NewClient` + `mcp.StreamableClientTransport` 走真实 HTTP):

- initialize 成功,serverInfo.name = "onvif-proxy";
- tools/list 恰好 12 个工具、名称齐全;
- `list_devices` / `get_status` 返回与 fake Backend 一致;
- `apply_config` dry_run 校验失败 → isError;
- `delete_device` 未知 uuid → isError;
- 未知工具调用 → 错误;
- 直接 `GET /mcp`(无会话)不会 panic,返回 4xx;
- 带恶意 `Origin` 的 POST → 403(若实现了包装层);
- `take_snapshot` 返回的 image 块字节与 fake backend 产出的 JPEG 完全一致
  (`TestMCPTakeSnapshot`);
- tools/list 携带 §2.1 所述的 annotation 提示——例如 `get_status` 是
  `readOnlyHint: true` 且无 `openWorldHint`,`probe_rtsp` 额外带
  `openWorldHint: true`,`delete_device` 是唯一 `destructiveHint: true` 的
  工具,`add_device` 是唯一非幂等的写工具(`TestMCPToolAnnotations`)。

## 4. 验收

1. `go build ./... && go vet ./... && go test ./...` 全绿;`go.mod` 仅新增
   go-sdk 及其间接依赖。
2. 本地起服务后,用真实客户端接入:
   `claude mcp add --transport http onvif-proxy http://127.0.0.1:8080/mcp`,
   能完成 initialize、列出 12 个工具、成功调用 `get_status` 与
   `list_devices`。
3. Web UI 原有 REST API 与页面行为不变(回归:内置 ONVIF 自检全绿)。
