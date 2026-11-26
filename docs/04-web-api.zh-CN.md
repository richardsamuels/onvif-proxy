# 04 · Web 后端 API 与 UI 设计

[English](04-web-api.md) | **简体中文**

Web 服务默认监听 `:8080`,提供 REST API 与内嵌单页 UI。可选 HTTP Basic 认证(`web.username/password`)。

## 1. REST API

统一约定:JSON 响应;错误格式 `{"error": "...", "detail": "..."}`;写操作仅 `PUT/POST`。配置/设备请求体超过 1 MiB、探测 JSON 超过 64 KiB 时返回 HTTP 413,不会再接受被静默截断的前缀。

### 1.1 配置管理

| 方法/路径 | 说明 |
|-----------|------|
| `GET /api/config` | 返回当前 config.yaml 原文(`text/plain`),UI 编辑器直接展示 |
| `PUT /api/config` | Body = 完整 YAML 文本。流程:解析 → 校验(语法/端口冲突/字段)→ 事务式切换运行态 → 原子落盘。候选 SOAP/RTSP 端口会同步绑定;绑定或写入失败会恢复旧运行态并保持旧文件不变。校验失败返回 400 + 逐条错误。`web.enabled`/`web.port` 变更会以“需重启”拒绝,Web 凭证可实时更新 |
| `GET /api/devices` | 结构化设备列表 + 运行时状态:`{name, uuid, soap_port, rtsp_port, running, error?, motion, auth_user, endpoints:{device_service, snapshot, streams:[{name, profile_token, rtsp_uri, rtsp, codec, width, height, framerate, bitrate, proxy_port, has_credentials}]}}`。上游 `rtsp` URL **不返回 userinfo**;`has_credentials` 表示服务端是否保存了凭证。其他流字段用于预填编辑表单。`motion` 为报警接收器给出的当前移动侦测状态(见 08 文档;未收到报警时恒为 `false`)。`GET /api/config` 仍是明确返回完整 YAML 的认证入口 |
| `POST /api/devices` | Body JSON DeviceSpec `{name, soap_port, rtsp_port, auth?, streams:[{name,rtsp,codec,width,height,framerate,bitrate,proxy_port?}], snapshot?}`。合入当前 YAML 后走与 `PUT /api/config` 相同的事务式流程;uuid/mac/serial 在准备阶段生成。校验失败(含端口冲突)返回 400,成功 `{"status":"applied"}` |
| `PUT /api/devices/{uuid}` | Body 与 POST 相同的 DeviceSpec。按 uuid 定位设备(未知 404)→ 用表单值构建新 Device,**原位替换**,但**保留原 uuid/mac/serial 与 info**(表单不携带这些)→ 校验 → 落盘 → 热重载。成功 `{"status":"applied"}`,校验/端口冲突 400 透传。**凭证保留语义**:保留 ONVIF 用户名且密码留空时维持原 ONVIF 密码;RTSP URL 不带 userinfo 时按流的唯一 name 保留原 userinfo,显式填写新 userinfo 则替换 |
| `DELETE /api/devices/{uuid}` | 按 uuid 从当前配置删除设备 → 校验 → 落盘 → 热重载。未知 uuid 返回 404,成功 `{"status":"applied"}` |

### 1.2 测试工具(对应 UI 测试面板)

| 方法/路径 | 说明 |
|-----------|------|
| `POST /api/test/rtsp` | Body `{"url": "rtsp://..."}`。原生 RTSP 探测(OPTIONS + DESCRIBE + Digest/Basic 认证),返回:`{"ok":true, "status":200, "auth":"digest", "server":"...", "tracks":[{"type":"video","codec":"H264","fmtp":"..."}], "latency_ms": 43}`。错误分类:`dial_timeout` / `auth_failed` / `not_found` / `no_video_track` / `protocol_error` |
| `POST /api/test/streaminfo` | Body `{"url": "rtsp://..."}`。ffprobe 探测首个视频流,返回 `{"codec":"h264","width":1920,"height":1080,"fps":25,"bitrate":2048}`,供新增设备表单回填。`bitrate`(kbps):源声明了就用声明值,否则 ffmpeg 拉流 3 秒按实际字节实测;测不出为 0(表单保留手填值)。ffprobe 不可用返回 501 |
| `GET /api/test/snapshot?device=<uuid>&stream=<name>` | 调 ffmpeg 从指定流抓一帧(`stream` 省略取主流),返回 `image/jpeg`。失败返回 JSON 错误(含 ffmpeg stderr 摘要) |
| `GET /api/preview?device=<uuid>&stream=<name>` | **MJPEG 实时预览**:ffmpeg 拉指定流 → `-f mpjpeg`(缩至 640 宽、5fps、静音)→ `multipart/x-mixed-replace` 推给浏览器,`<img>` 直接播。客户端断开即杀 ffmpeg;每设备并发预览数上限 2 |
| `POST /api/test/onvif?device=<uuid>` | **ONVIF 自检**:服务端以 ONVIF 客户端身份调用自己的 SOAP 端点,逐方法返回 `{method, http_status, soap_fault, pass}`。覆盖方法:GetSystemDateAndTime、GetCapabilities、GetServices、GetScopes、GetNetworkInterfaces、GetDeviceInformation、GetProfiles、GetStreamUri、GetSnapshotUri、GetEventProperties、CreatePullPointSubscription(TTL 10s,任其过期)+ 一个故意不存在的方法(验证 Fault 规范性) |
| `GET /api/discovery/log` | 最近 50 条 WS-Discovery 交互(谁在 Probe、回了什么),排查"客户端发现不了设备"用 |

### 1.3 系统

| 方法/路径 | 说明 |
|-----------|------|
| `GET /api/status` | 版本、启动时长、ffmpeg 是否可用、advertise_ip 探测结果 |
| `GET /healthz` | liveness,200 即可 |

### 1.4 MCP 端点

`/mcp`(Streamable HTTP,JSON-RPC,非 REST 风格)把管理能力以 MCP 工具暴露给
AI 客户端;与 REST API 同端口、同 Basic 认证。协议、工具清单与实现契约见
`docs/07-mcp.zh-CN.md`。

## 2. UI 页面设计(单页,三个区块)

**技术栈**:Preact + TSX,由 esbuild 打包成 `static/dist/{app.js,app.css}`(提交进仓库,`go:embed` 进二进制,运行时零 Node)。源码在 `internal/web/ui/`,`index.html` 为薄壳(`<div id="app">` + `/dist/app.js`)。开发:`cd internal/web/ui && npm install && npm run check && npm run build`。

**健壮性约定(全局)**:所有异步操作统一走 `useAsync` + `AsyncButton` 机制 —— 按钮点击后**立即禁用并显示转圈文案**,请求完成/失败后恢复;`useAsync` 内部有再入守卫,天然防双击/防表单重复提交;删除类操作 `confirm()` 通过后按钮即锁。`fetch` 统一封装(`api.ts`):20s 超时(AbortController)、非 2xx 抛 `ApiError`、解析 `{error,detail}` 错误信封。没有裸 `onClick` 异步。

### 2.1 设备列表(首页)

每台设备一张卡片:

```
┌────────────────────────────────────────────────┐
│ 车库摄像头                      ● 运行中        │
│ ONVIF: http://192.168.1.10:8081/onvif/device_service
│ RTSP:  rtsp://192.168.1.10:8554/h264/ch1/main…  │
│ [测试连接] [快照] [预览] [ONVIF 自检] [编辑] [删除] │
└────────────────────────────────────────────────┘
```

- **测试连接** → `/api/test/rtsp`,卡片内显示结果:连通/认证/编码/延迟,错误按类别给中文提示("TCP 连不通,检查 IP 与端口" / "认证失败,检查用户名密码" / "路径 404");
- **快照** → 卡片内直接显示 JPEG;
- **预览** → 弹层 `<img src=/api/preview…>` 实况,关闭(点遮罩/按钮/Esc)即清空 src 断流杀 ffmpeg;
- **ONVIF 自检** → 渲染方法×状态表格(绿✅/红❌),即本项目立项时那张对比表的自动化版;
- **编辑** → 打开与"新增设备"同一表单组件的**编辑模式**,用 `GET /api/devices` 补充的字段(soap/rtsp 端口、`auth_user`、每流 `rtsp/width/height/framerate/bitrate`)预填,提交走 `PUT /api/devices/{uuid}`;ONVIF 密码框留空即保持原密码;
- **删除** → `confirm` 后 `DELETE /api/devices/{uuid}`,成功后刷新设备列表与配置编辑器。

### 2.2 配置编辑

- YAML 全文编辑器(textarea + 等宽字体,不引前端依赖);
- "校验"按钮:dry-run 调 `PUT /api/config?dry_run=1`,只报错不落盘;
- "保存并生效"按钮:事务式切换运行态并原子落盘,展示校验、绑定或回滚结果;
- 顶部提示条:配置文件路径、上次保存时间。

### 2.3 新增设备向导(表单)

1. 填 RTSP URL → 点"探测" → 自动带出编码/分辨率(SDP 有则回填,无则手填);
2. 填名称、端口(自动建议下一个空闲端口对)、可选低码流;
3. "添加" → 后端把设备节点合入 YAML → 校验 → 落盘 → 热重载。

## 3. 安全考虑

- Web UI 面向内网管理员;开启 Basic 认证后,`/api/*` 与静态页全部走认证;
- `/api/test/rtsp` 仅接受 `rtsp://` scheme,禁止借道探测任意 TCP 端口之外的协议(SSRF 面收敛:目标端口不限,但协议握手必须是 RTSP);
- 配置文件中的摄像头密码:`GET /api/config` 原样返回(编辑器要能改),因此**开启 web 认证是使用预设密码场景的前提**,文档与 UI 均给出提示;
- ffmpeg 命令行参数全部程序拼装,RTSP URL 经 `exec.Command` 参数传递,无 shell 注入面。
