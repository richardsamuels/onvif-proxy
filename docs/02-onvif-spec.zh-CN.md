# 02 · ONVIF 规范符合性设计

[English](02-onvif-spec.md) | **简体中文**

本项目"按规范做完善"的具体落点。引用的规范:

| 规范 | 用途 |
|------|------|
| ONVIF Core Specification | Device 服务、发现、安全、Fault 语义 |
| ONVIF Media Service Specification (ver10) | Media 服务、Profile/StreamUri/SnapshotUri |
| ONVIF Profile S | 必选功能集的判定基准 |
| ONVIF Event Service Specification / OASIS WS-BaseNotification 1.3 | Events 服务、PullPoint 订阅、移动侦测 topic |
| W3C WS-Addressing 1.0 | 事件消息的 `wsa:Action` / `wsa:To` 头 |
| W3C SOAP 1.2 | Envelope 结构、Fault 结构、HTTP 状态码映射 |
| WS-Discovery (2005/04 draft,ONVIF 指定版本) | UDP 3702 组播发现 |
| OASIS WS-Security UsernameToken Profile 1.0 | WSSE PasswordDigest 认证 |
| RFC 2326 (RTSP 1.0) / RFC 7826 (RTSP 2.0) | RTSP 探测客户端(实现按 1.0,兼容绝大多数摄像头) |
| RFC 7616 / RFC 2617 (HTTP Digest) | RTSP 探测的 401 认证 |
| RFC 4566 (SDP) | DESCRIBE 结果解析 |
| RFC 4122 (UUID) | 设备 EndpointReference 地址 |

## 1. SOAP 层规范

### 1.1 请求处理

- 仅接受 `POST`;`Content-Type` 接受 `application/soap+xml` 与 `text/xml`(兼容 SOAP 1.1 客户端习惯,报文按 1.2 处理)。
- 方法路由依据 **Body 首个子元素的 localName + namespace**,不依赖 SOAPAction/WS-Addressing Action 头(部分客户端不发)。
- 用 `encoding/xml` token 流提取,不做全量反序列化;参数(如 `ProfileToken`)按需提取。

### 1.2 响应

- 统一 `Content-Type: application/soap+xml; charset=utf-8`。
- 响应模板逐条对照 ONVIF WSDL 的 element 定义手写,命名空间前缀固定声明在 Envelope 上(`tds`/`trt`/`tt`/`ter`/`tev`/`wsnt`/`wsa`/`tns1`/`wstop`)。事件响应还带 `<wsa:Action>` 头。
- 所有动态字段经 XML 转义。

### 1.3 Fault 语义(核心差异点)

上游项目的问题:未注册方法 → soap 库抛非规范 500。本项目按 ONVIF Core Spec 的 fault 表实现:

| 场景 | SOAP Code/Subcode | HTTP 状态码 |
|------|-------------------|-------------|
| 方法未实现(可选方法) | `env:Receiver` / `ter:ActionNotSupported` | 500(SOAP 1.2 规定 Receiver→500;**报文体是规范的 Fault XML**,客户端可正确降级) |
| 请求格式非法 / 参数缺失 | `env:Sender` / `ter:InvalidArgVal` | 400 |
| 未认证 / 认证失败 | `env:Sender` / `ter:NotAuthorized` | 400 |
| 未知 Profile token | `env:Sender` / `ter:InvalidArgVal` | 400 |

Fault 报文结构(SOAP 1.2 §5.4):`Code/Subcode/Reason/Detail` 齐全,`Reason` 带 `xml:lang="en"`。

## 2. 认证(WSSE)

- 实现 OASIS UsernameToken Profile 的 **PasswordDigest** 校验:
  `Digest = Base64( SHA-1( Base64Decode(Nonce) + Created + Password ) )`
- PasswordDigest 强制要求合法 Base64 `Nonce` 与可解析的 `Created`;时间偏差容忍 ±5 分钟(编译期硬编码常量,当前不支持配置修改)。通过校验的 nonce/时间戳组合会在偏差窗口内缓存,重复使用将被拒绝。
- **`GetSystemDateAndTime` 永远免认证**(Core Spec 明确要求:客户端靠它校时后才能算出合法 Digest)。
- 每设备可配置 `auth.username/password`;不配置则**全放行**(接受任意/缺失 WSSE 头)—— 与上游行为一致,便于内网使用。
- 配置设备认证后,`GetSnapshotUri` 返回的 HTTP 快照地址也要求使用同一组账号密码进行 HTTP Basic 认证。明文 HTTP 上的 Basic 凭证不加密,仅应在可信网络使用,或置于 TLS 反向代理之后。
- PasswordText 模式一并接受(部分客户端只发明文)。

## 3. WS-Discovery

- 单 UDP socket 加入组播 `239.255.255.250:3702`,代答本进程内全部虚拟设备。
- **Hello**:每设备启动/热重载后组播发送;**Bye**:优雅退出/设备移除时发送。
- **Probe → ProbeMatches**:
  - 校验 `d:Types`:为空或包含 `dn:NetworkVideoTransmitter`/`tds:Device` 才应答;
  - 按规范在 `0 ~ APP_MAX_DELAY(500ms)` 随机延迟后单播回复,多设备逐台 ProbeMatch;
  - `RelatesTo` 回填请求 `MessageID`;`XAddrs` 为 `http://<ip>:<port>/onvif/device_service`。
- Scopes(同时用于 `GetScopes`):
  ```
  onvif://www.onvif.org/type/video_encoder
  onvif://www.onvif.org/type/Network_Video_Transmitter
  onvif://www.onvif.org/Profile/Streaming
  onvif://www.onvif.org/name/<设备名>
  onvif://www.onvif.org/hardware/<型号>
  onvif://www.onvif.org/location/
  ```
- 设备 EPR 地址:`urn:uuid:<设备uuid>`,UUID 首次生成后持久化到 YAML,保证客户端重启后仍认得同一台设备。

## 4. Device Service 方法清单

路径 `POST /onvif/device_service`。✅=完整实现,◽=空/固定值响应(合法但无内容),✗=规范 Fault。

| 方法 | 实现 | 说明 |
|------|:---:|------|
| GetSystemDateAndTime | ✅ | UTC + 本地时间,`DateTimeType=NTP`;免认证 |
| SetSystemDateAndTime | ◽ | 接受并返回空响应(虚拟设备无系统时钟可设) |
| GetCapabilities | ✅ | Device/Events/Media 三节;Events 声明 `WSPullPointSupport=true`;Media 含 `StreamingCapabilities(RTP_TCP, RTP_RTSP_TCP)`、快照;PTZ/Analytics 不声明 |
| GetServices | ✅ | Device + Events + Media 三个 entry,含版本号;`IncludeCapability` 支持 |
| GetServiceCapabilities | ✅ | Network/System/Security 最小合法集 |
| GetDeviceInformation | ✅ | Manufacturer/Model/FirmwareVersion/SerialNumber/HardwareId,来自配置(有默认值) |
| GetScopes | ✅ | 见上节 Scopes 列表(上游 500 的方法之一) |
| SetScopes / AddScopes | ✗ | ActionNotSupported |
| GetNetworkInterfaces | ✅ | 单接口 `eth0`:HwAddress=配置 MAC、IPv4 手动地址+前缀(上游 500 的方法之一) |
| GetNetworkProtocols | ✅ | HTTP(soap端口)+ RTSP(代理端口) |
| GetHostname | ✅ | FromDHCP=false,Name=设备名 slug |
| GetDNS / GetNTP | ◽ | FromDHCP=true 的最小合法响应 |
| GetUsers | ✅ | 配置了 auth → 返回该用户(Administrator);未配置 → 空列表 |
| CreateUsers/DeleteUsers/SetUser | ✗ | ActionNotSupported |
| GetWsdlUrl | ✅ | `http://www.onvif.org/` |
| SystemReboot | ◽ | 返回 `Message: Rebooting...`,实际 no-op |
| GetSystemLog / GetSystemBackup | ✗ | ActionNotSupported |
| 其余未列方法 | ✗ | ActionNotSupported(规范 Fault) |

## 5. Media Service 方法清单

路径 `POST /onvif/media_service`(`GetCapabilities`/`GetServices` 中如实通告)。

数据模型(**多 Profile**,数量由配置的 `streams[]` 决定,N ≥ 1):每设备 1 个 VideoSource(token `src`)、1 个 VideoSourceConfiguration(token `vsc`,全体 Profile 共享)、每条流 1 个 VideoEncoderConfiguration(token `vec_<name>`)、每条流 1 个 Profile(token `profile_<name>`,`fixed=true`)。ONVIF 对同一 VideoSource 挂任意多个 Profile 是规范允许的;Unifi Protect 会取前两个作为高/低流,其他客户端(Frigate、Home Assistant)可任选 Profile。

| 方法 | 实现 | 说明 |
|------|:---:|------|
| GetServiceCapabilities | ✅ | `SnapshotUri`/`StreamingCapabilities(RTP_TCP, RTP_RTSP_TCP)`/`ProfileCapabilities(MaximumNumberOfProfiles=流数量)` |
| GetProfiles / GetProfile | ✅ | 每条配置流一个 Profile(N 个);含 VSC + VEC 完整节点 |
| CreateProfile / DeleteProfile | ✗ | ActionNotSupported(Profile 固定) |
| GetVideoSources | ✅ | 分辨率/帧率取主流(streams[0])配置 |
| GetVideoSourceConfigurations / ~Configuration | ✅ | Bounds = 完整画面 |
| GetVideoEncoderConfigurations / ~Configuration | ✅ | 逐流输出 H264、JPEG 或 MPEG4 `Encoding` 与对应编码专属节点;分辨率/帧率/码率/GOP 来自配置。Profile S 未定义 H.265,因此拒绝 H.265/HEVC 配置 |
| GetVideoEncoderConfigurationOptions | ✅ | 仅列出已配置编码及其分辨率/帧率档位;提供 configuration/profile token 时只返回该流的选项 |
| SetVideoEncoderConfiguration | ◽ | 接受并忽略(返回空成功;客户端收编流程常会调用) |
| GetStreamUri | ✅ | `rtsp://<ip>:<该流代理端口>/<原始path>`;按 ProfileToken 找到对应流 |
| GetSnapshotUri | ✅ | `http://<ip>:<soapPort>/onvif/snapshot?token=<profile>`;配置设备 `auth` 后需用同一组凭证进行 HTTP Basic 认证。后端两种模式:真实摄像头快照 URL 透传,或 ffmpeg 抓帧(带 TTL 缓存),见 03 文档 `snapshot` 节 |
| GetGuaranteedNumberOfVideoEncoderInstances | ✅ | `TotalNumber` 为流数量;可选 `JPEG`/`H264`/`MPEG4` 计数按配置编码统计 |
| GetAudioSources / GetAudioEncoderConfigurations 等音频类 | ◽ | 空列表(合法响应,声明无音频) |
| GetCompatibleVideoSourceConfigurations / GetCompatibleVideoEncoderConfigurations 等 Compatible 类 | ✅ | 与对应 Get~Configuration(s) 共用渲染函数,返回同一固定配置 |
| GetOSDs | ◽ | 空列表 |
| 其余未列方法 | ✗ | ActionNotSupported |

## 6. Event Service 方法清单

路径 `POST /onvif/event_service`(在 `GetCapabilities` 的 `<tt:Events>` 与 `GetServices` 中声明)。只实现 **PullPoint** 模型(`WSPullPointSupport=true`;`WSSubscriptionPolicySupport` 与可暂停订阅为 `false`)。移动侦测状态来自报警接收器,见 [08-alarm-server.zh-CN.md](08-alarm-server.zh-CN.md)。Events 服务始终存在;从未收到报警的设备只会报告"无移动"。

每个响应都带 WS-Addressing `<wsa:Action>` 头(WSDL 中的响应 action)。每个订阅拥有管理地址 `http://<Host>/onvif/subscription/<16 位 hex>`;PullMessages/Renew/Unsubscribe/SetSynchronizationPoint 既可发到该路径,也可发到 `/onvif/event_service` 并在 `wsa:To` 头里携带订阅地址。认证与其他服务相同(WSSE)。

| 方法 | 状态 | 说明 |
|------|:---:|------|
| GetServiceCapabilities | ✅ | `WSPullPointSupport=true`,其余为 `false`,`MaxPullPoints=16` |
| GetEventProperties | ✅ | TopicSet 声明下表两个 topic,各带 `MessageDescription IsProperty="true"`;TopicExpression 方言 ConcreteSet/Concrete;MessageContentFilter 方言 ItemFilter |
| CreatePullPointSubscription | ✅ | `InitialTerminationTime` 支持 xsd:duration(`PT60S`)或绝对 dateTime;默认 60s,钳制在 10s–1h。`Filter` 接受但忽略(投递全部 topic)。每设备最多 16 个 pull point;创建第 17 个时淘汰最早到期的那个。创建时立即入队一条携带当前状态的 `Initialized` 消息 |
| PullMessages | ✅ | 长轮询:有消息入队即返回,否则 `Timeout` 后返回(默认 10s,上限 60s;`PT0S` 立即返回)。`MessageLimit` 默认 10。每次调用按 TTL 续期订阅。响应含 `CurrentTime`/`TerminationTime` 及零或多个 `wsnt:NotificationMessage` |
| Renew | ✅ | `TerminationTime` 为 duration 或 dateTime,钳制规则同上 |
| Unsubscribe | ✅ | 删除订阅。幂等:未知 id(已过期、被淘汰或因重启丢失)同样返回成功,并记录日志 |
| SetSynchronizationPoint | ✅ | 重新入队一条携带当前状态的 `Initialized` 消息 |
| Subscribe / GetCurrentMessage / Seek / … | ✗ | ActionNotSupported |
| 对未知或已过期订阅调用 PullMessages / Renew / SetSynchronizationPoint | ✅ | **复活**:若 id 格式合法(16 位小写 hex),则以同一 id、默认 60s TTL 重建 pull point,入队一条 `Initialized` 消息,调用照常处理。订阅只保存在内存中,否则重启、热重载、过期或淘汰都会让客户端失联(Unifi Protect 会一直拉取原地址,只会"最终"重新订阅)。唯一需要的状态就是 id,且由客户端持有,因此无需持久化。复活的订阅同样计入 16 个 pull point 上限 |
| PullMessages / Renew / SetSynchronizationPoint 使用格式非法的订阅 id | ✗ | `env:Sender` / `ter:InvalidArgVal`,HTTP 400 |

Topic(每次变化两者都发,`Dialect=…/ConcreteSet`):

| Topic | Source SimpleItem | Data SimpleItem |
|------|------|------|
| `tns1:RuleEngine/CellMotionDetector/Motion` | `VideoSourceConfigurationToken=vsc`、`VideoAnalyticsConfigurationToken=vac`、`Rule=MotionDetectorRule` | `IsMotion`(xs:boolean) |
| `tns1:VideoSource/MotionAlarm` | `Source=src` | `State`(xs:boolean) |

`tt:Message` 带 `UtcTime` 与 `PropertyOperation`(订阅/同步点时为 `Initialized`,每次状态变化为 `Changed`)。每个订阅最多缓存 64 条消息,溢出时丢弃最旧的。

## 7. RTSP 探测客户端(Web UI"测试连接")

原生实现(不依赖 ffmpeg),按 RFC 2326:

1. TCP 连接(超时 5s)→ `OPTIONS`(带 `CSeq`、`User-Agent`);
2. `DESCRIBE`(`Accept: application/sdp`);401 时按 `WWW-Authenticate` 依次支持 **Digest(RFC 7616,MD5)** 与 **Basic**;
3. 解析 SDP(RFC 4566):`m=video/audio` 轨道、`a=rtpmap`(编码名 H264/H265/…)、`a=fmtp` 中 H264 `sprop-parameter-sets` 可解出 profile-level、`a=framerate`/`a=framesize`(如有);
4. 输出结构化结果:`连通性 / 认证结果 / 状态码 / 编码 / 轨道数 / server 头`,UI 按类别显示错误(TCP 不通 ≠ 密码错 ≠ 路径 404)。

## 8. 与上游对照的验收基准

实现完成后,自检(Web UI"ONVIF 自检"按钮,详见 04 文档)必须全绿:

| 方法 | p10tyr 镜像 | daniela-hase 上游 | 本项目目标 |
|------|:---:|:---:|:---:|
| GetCapabilities | ❌ 500 | ✅ | ✅ 200 |
| GetServices | ✅ | ✅ | ✅ 200 |
| GetSystemDateAndTime | ✅ | ✅ | ✅ 200(免认证) |
| GetScopes | ❌ 500 | ❌ | ✅ 200 |
| GetNetworkInterfaces | ❌ 500 | ❌ | ✅ 200 |
| GetDeviceInformation | — | ✅ | ✅ 200 |
| GetProfiles / GetStreamUri / GetSnapshotUri | — | 部分 | ✅ 200 |
| GetEventProperties / CreatePullPointSubscription | — | — | ✅ 200 |
| 任意未实现方法 | 裸 500 | 裸 500 | 规范 ActionNotSupported Fault |
