# 06 · 包间接口契约(实现对齐基准)

[English](06-internal-api.md) | **简体中文**

> 本文档是并行开发各包时的**签名契约**。实现必须精确匹配这里的导出签名;若实现中发现契约不合理,改动前先更新本文档。`internal/config` 已实现,其余包以它为地基。

模块路径:`github.com/Aiaid/onvif-proxy`。依赖规则:所有包可 import `internal/config` 与标准库;**禁止新增第三方依赖**(go.mod 直接依赖仅 yaml.v3,唯一例外:`internal/web` 可 import 官方 `github.com/modelcontextprotocol/go-sdk/mcp` 实现 `/mcp` 端点,契约见 `docs/07-mcp.zh-CN.md`);包之间除下述声明外不得互相 import。

## internal/config(已实现,只读参考)

```go
type Config struct { Server ServerConfig; Web WebConfig; Alarm AlarmConfig; Devices []*Device }
type AlarmConfig struct { Enabled bool; Port int; HoldSeconds *int } // yaml: alarm.enabled/port/hold_seconds
func (a AlarmConfig) ListenPort() int // Port;为 0 时取 DefaultAlarmPort(8899)
func (a AlarmConfig) Hold() int       // 秒;nil -> DefaultAlarmHoldSeconds(60),0 = 关闭
func Load(path string) (*Config, error)          // 读+校验+生成 uuid/mac 并写回
func Parse(data []byte) (*Config, error)         // 严格解析+校验(dry-run 用)
func Prepare(data []byte) (*Config, []byte, error) // 解析+校验+生成身份,返回规范 YAML 且不落盘
func Save(path string, cfg *Config) error        // 原子写
func WriteDefault(path string) error             // 以 0600 权限排他创建初始配置
func ApplyEnvOverrides(cfg *Config) ([]string, error) // ONVIF_* env 覆盖(仅内存,覆盖后重校验);返回已应用项供日志
func DetectAdvertiseIP() string

type Device struct { Name, UUID, MAC, Serial string; Ports Ports; Info Info;
                     Auth *Auth; Streams []*Stream; Snapshot Snapshot }
func (d *Device) PrimaryStream() *Stream
func (d *Device) StreamByName(name string) *Stream
func (d *Device) StreamByProfileToken(token string) *Stream
func (d *Device) ProxyPortFor(s *Stream) int
func (d *Device) ProxyBindings() []ProxyBinding  // {ListenPort int; Target string}
func (d *Device) SnapshotStream() *Stream
func (d *Device) SnapshotCacheSeconds() int

type Stream struct { Name, RTSP, Codec string; Width, Height, Framerate, Bitrate, ProxyPort int }
func (s *Stream) CodecName() string // 规范化 h264(默认)、jpeg 或 mpeg4
func (s *Stream) TargetAddr() string             // "host:port"(默认 554)
func (s *Stream) PathQuery() string              // "/path?query"
func (s *Stream) SourceURL() string              // 含凭证的完整上游 URL
func (s *Stream) ProfileToken() string           // "profile_<name>"
func (s *Stream) EncoderToken() string           // "vec_<name>"
```

## internal/soap(SOAP 1.2 基础层)

```go
package soap

// 解析后的请求
type Request struct {
    Action    string            // Body 首子元素 localName,如 "GetStreamUri"
    Namespace string            // 该元素的 namespace URI
    Body      []byte            // Body 的 innerxml(供提取参数)
    Security  *UsernameToken    // 无 WSSE 头时为 nil
    To        string            // WS-Addressing To 头(缺省为空);用于 PullPoint 调用路由
}
type UsernameToken struct { Username, Password, PasswordType, Nonce, Created string }

func ParseRequest(body []byte) (*Request, error)
// 从 innerxml 提取首个匹配 localName 的元素文本(如 ProfileToken)
func ExtractElement(inner []byte, localName string) (string, bool)

// WSSE PasswordDigest / PasswordText 校验,clockSkew 为 Created 容忍偏差
func (t *UsernameToken) Verify(username, password string, clockSkew time.Duration) bool

// 响应渲染:body 为已渲染的 Body 内部 XML;Envelope/命名空间由本层统一加
func Envelope(body string) []byte
// 同上,带 header(已渲染的 env:Header 内部 XML,如 <wsa:Action>);header 为空则不输出 env:Header
func EnvelopeWithHeader(header, body string) []byte
// 命名空间常量新增 NSEvents(tev)、NSWSNT(wsnt)、NSWSA(wsa)、NSTopics(tns1)、NSWSTOP(wstop)

// Fault:写入 w(设置 Content-Type 与 HTTP 状态码)并返回
// code: "Sender"/"Receiver";subcode 如 "ter:ActionNotSupported"
func WriteFault(w http.ResponseWriter, code, subcode, reason string)
// 便捷:
func WriteActionNotSupported(w http.ResponseWriter, action string) // Receiver, HTTP 500
func WriteNotAuthorized(w http.ResponseWriter)                     // Sender, HTTP 400
func WriteInvalidArg(w http.ResponseWriter, reason string)         // Sender, HTTP 400

func XMLEscape(s string) string
```

Envelope 统一声明命名空间前缀:`env`(SOAP 1.2)、`tds`、`trt`、`tt`、`ter`、`tev`、`wsnt`、`wsa`、`tns1`、`wstop`。Fault 结构含 Code/Subcode/Reason(xml:lang="en")。

## internal/onvif(Device + Media + Events 服务,每设备一个 HTTP server)

```go
package onvif // 允许 import: internal/config, internal/soap

type Options struct {
    AdvertiseIP  string // 请求无 Host 头时构造 URI 的兜底
    Version      string // firmware 版本展示
    // 快照回调,由 main 注入(透传/ffmpeg/缓存都在 main 侧组合)
    SnapshotFunc func(ctx context.Context, streamName string) (data []byte, contentType string, err error)
}

func NewServer(dev *config.Device, opts Options) *Server
func (s *Server) Run(ctx context.Context) error // 监听 dev.Ports.SOAP,ctx 取消时优雅退出
func (s *Server) Serve(ctx context.Context, ln net.Listener) error // 在已绑定 listener 上服务

func (s *Server) SetMotion(active bool) // 更新移动侦测状态;变化时向所有存活的 pull point 入队 "Changed" 事件(并发安全)
func (s *Server) Motion() bool          // 当前移动侦测状态

// 路由:
//   POST /onvif/device_service
//   POST /onvif/media_service
//   POST /onvif/event_service
//   POST /onvif/subscription/{id}               (PullPoint 订阅管理器)
//   GET  /onvif/snapshot?token=<profileToken>   (配置 dev.Auth 时要求 HTTP Basic)
```

行为要求(细节见 docs/02):方法矩阵按 02 文档第 4/5/6 节;关闭时释放挂起的 PullMessages 长轮询;`GetSystemDateAndTime` 免认证;`dev.Auth == nil` 全放行;PasswordDigest 要求有效且新鲜的时间戳/nonce,并拒绝 nonce 重放;URI 构造优先 `r.Host`;未知方法 → `soap.WriteActionNotSupported`。SOAP 请求体上限 1 MiB,HTTP server 设置 header/read/idle 超时上限。

## internal/alarm(TVT / ENS XML 报警服务器接收器)

```go
package alarm // 允许 import:仅标准库

type Options struct {
    Hold     time.Duration                 // 通道超过此时长未再报 true 则清除;0 = 不超时
    OnChange func(channel int, active bool) // 每次状态变化时调用(不持有内部锁);channel = 从 1 开始的 motionAlarm id
}

func NewServer(opts Options) *Server
func Parse(body []byte) (map[int]bool, error) // motionAlarm id -> 状态;非法条目跳过;XML 格式错误/空文档 -> error
func (s *Server) Apply(motion map[int]bool)   // 快照语义:true = 触发,false/缺失 = 未触发;变化按通道号排序回调
func (s *Server) Active() []int               // 当前触发的通道(已排序)
func (s *Server) Handler() http.Handler       // POST /SendAlarmStatus;其他方法 405,其他路径 404
func (s *Server) Serve(ctx context.Context, ln net.Listener) error // 在已绑定的 listener 上服务,并运行 hold 超时循环(1s 一次)
```

报文格式与状态语义见 docs/08-alarm-server.zh-CN.md。成功 → 200 `<config status="success"/>`;XML 格式错误或请求体 >64 KiB → 400 `<config status="failed" errorCode="2"/>`。无认证。

## internal/discovery(WS-Discovery)

```go
package discovery

type Device struct { UUID, Name, Hardware, XAddr string } // XAddr 完整 URL
type LogEntry struct { Time time.Time; Remote, Kind, Detail string } // Kind: probe/hello/bye/match

func New(devices []Device) *Server
func (s *Server) Run(ctx context.Context) error  // 绑定 :3702、加组播、Hello;ctx 取消时发 Bye
func Listen() (*net.UDPConn, *net.UDPAddr, error) // 同步绑定并加入组播
func (s *Server) Serve(ctx context.Context, conn *net.UDPConn, addr *net.UDPAddr) error
func (s *Server) SetDevices(devices []Device)    // 热重载:删除、新增或元数据变化均发 Bye/Hello
func (s *Server) Log() []LogEntry                // 最近 50 条,新在前
```

## internal/rtspproxy(TCP 透传)

```go
package rtspproxy

// 监听 127.0.0.1 之外全接口的 listenPort,双向 io.Copy 到 target("host:port")。
// ctx 取消时关闭 listener 与全部在途连接;每个代理最多 128 个活动连接,并执行
// 全双工共享空闲超时。
func Run(ctx context.Context, listenPort int, target string) error
func Serve(ctx context.Context, ln net.Listener, target string) error
```

## internal/rtsp(原生 RTSP 探测客户端)

```go
package rtsp

type ErrKind string
const (
    ErrDialTimeout   ErrKind = "dial_timeout"
    ErrAuthFailed    ErrKind = "auth_failed"
    ErrNotFound      ErrKind = "not_found"
    ErrNoVideoTrack  ErrKind = "no_video_track"
    ErrProtocol      ErrKind = "protocol_error"
)
type Track struct { Type, Codec, Fmtp string }     // Type: "video"/"audio"
type Result struct {
    OK        bool
    Status    int           // 最终 RTSP 状态码
    Auth      string        // "none"/"basic"/"digest"
    Server    string        // Server 头
    LatencyMS int64
    Tracks    []Track
    ErrKind   ErrKind       // OK=false 时有效
    ErrDetail string
}
func Probe(ctx context.Context, rawURL string) *Result
// OPTIONS → DESCRIBE;401 时按 WWW-Authenticate 支持 Digest(MD5, RFC 7616)与 Basic;
// SDP 解析 m=/a=rtpmap/a=fmtp;整体超时 5s(可被 ctx 提前取消)
```

## internal/mediautil(ffmpeg 封装)

```go
package mediautil

func Available() bool // PATH 中是否有 ffmpeg
func ProbeAvailable() bool // PATH 中是否有 ffprobe

// 抓单帧 JPEG:-rtsp_transport tcp -frames:v 1;stderr 摘要进 error
func Grab(ctx context.Context, rtspURL string) ([]byte, error)

// TTL 缓存 + singleflight(并发同 key 只跑一个 fetch)
type Cache struct{ /* ... */ }
func NewCache(ttl time.Duration) *Cache
func (c *Cache) Get(key string, fetch func() ([]byte, error)) ([]byte, error)

// MJPEG 预览:缩至 maxWidth、fps 帧率、静音,multipart/x-mixed-replace 写入 w;
// 客户端断开(r.Context 取消)即杀 ffmpeg 进程组
func ServeMJPEG(w http.ResponseWriter, r *http.Request, rtspURL string, maxWidth, fps int) error

// 单路视频流的编码/分辨率/帧率摘要(ffprobe 提取)。FPS 无帧率信息时为 0;
// Bitrate 单位 kbps,ffprobe 元数据未声明时降级为拷流实测,仍取不到则为 0。
type StreamInfo struct {
    Codec   string // ffprobe codec_name
    Width   int
    Height  int
    FPS     int
    Bitrate int
}

// 对 rtspURL 跑 ffprobe(-rtsp_transport tcp,选首个视频流),取不到 bit_rate 时
// 退化为拷流几秒实测码率;URL 仅作为 exec 参数传递,不经 shell。ctx 无 deadline
// 时套 15s 超时。供 POST /api/test/streaminfo 与 MCP get_stream_info 工具调用。
func ProbeInfo(ctx context.Context, rtspURL string) (*StreamInfo, error)
```

## internal/web(REST API + 内嵌 UI)

```go
package web // 允许 import: internal/config, internal/rtsp, internal/mediautil, internal/discovery

// 由 main 实现
type Backend interface {
    ConfigYAML() ([]byte, error)
    ApplyConfig(raw []byte, dryRun bool) error   // Parse→(落盘+热重载);错误原样返回给前端
    Devices() []DeviceRuntime                    // 运行中的设备快照
    Snapshot(ctx context.Context, dev *config.Device, streamName string) ([]byte, string, error)
    DiscoveryLog() []discovery.LogEntry
    Status() Status
}
type DeviceRuntime struct { Device *config.Device; Running bool; Error string; Motion bool }
type Status struct {
    Version, AdvertiseIP string
    UptimeSeconds        int64
    FFmpeg               bool
}

func New(cfg config.WebConfig, backend Backend) *Server
func (s *Server) Run(ctx context.Context) error
func (s *Server) Serve(ctx context.Context, ln net.Listener) error // 在已绑定 listener 上服务
```

REST 路由与语义见 docs/04(`/api/config`、`/api/devices`、`/api/test/rtsp`、`/api/test/streaminfo`、`/api/test/snapshot`、`/api/preview`、`/api/test/onvif`、`/api/discovery/log`、`/api/status`、`/healthz`)。要点:

- 探测直接调 `rtsp.Probe`;预览直接调 `mediautil.ServeMJPEG`(每设备并发上限 2,web 层管信号量);快照走 `Backend.Snapshot`(缓存/透传由 main 组合);
- **ONVIF 自检**在 web 包内实现:对 `dev.Ports.SOAP` 逐方法发手写 SOAP envelope(WSSE 按 dev.Auth 生成),按 docs/04 的方法清单返回 `[{method, http_status, soap_fault, pass}]`;最后加一个不存在的方法,pass 条件 = 返回体是合法 Fault XML 且 subcode 为 ActionNotSupported;
- 静态 UI:Preact+TSX 源码在 `internal/web/ui/`,esbuild 打包为 `static/dist/{app.js,app.css}`(提交进仓库),`index.html` 薄壳,`go:embed all:static` 进二进制(含 `dist/`);`GET /dist/*` 由 `http.FileServerFS` 从嵌入 FS 服务;`PUT /api/devices/{uuid}` 编辑设备(见 docs/04);`GET /api/devices` DTO 为编辑预填补充了 `rtsp_port/auth_user` 与每流源参数;
- Basic 认证:cfg.Username 非空时对全部路由生效;若 backend 同时实现 `WebConfig() config.WebConfig`,每次请求动态读取凭证,用户名/密码变更无需重启 Web listener。

## cmd/onvif-proxy(main,集成层,最后实现)

职责:flag 解析(`-config`,默认 `./config.yaml`;`-version`)→ `config.Load` → 组装 Manager(实现 `web.Backend`;每设备起 onvif.Server + rtspproxy;discovery.Server;`alarm.enabled` 时起 alarm.Server,其 OnChange 把通道 N 映射到 `devices[N-1]` 的 `onvif.Server.SetMotion`;快照函数 = 透传 or mediautil.Grab + Cache)→ 信号处理与事务式热重载。重载先准备候选配置并同步预绑定全部 SOAP/RTSP/discovery/alarm listener;绑定或落盘失败会恢复旧 generation,保留可用的旧运行态与旧文件;成功时用同目录原子 rename 且权限为 0600。`web.username/password` 可热更新,`web.enabled` 或 `web.port` 变更则明确要求重启。
