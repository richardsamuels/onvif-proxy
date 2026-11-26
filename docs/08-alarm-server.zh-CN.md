# 08 · 报警服务器(TVT / ENS XML `SendAlarmStatus` / `GetAlarmStatus`)

[English](08-alarm-server.md) | **简体中文**

许多基于 TVT 方案的 NVR 与 IP 摄像机(以 ENS 等品牌销售)通过 XML HTTP API 报告报警状态。onvif-proxy 读取其中的移动侦测部分,转换为对应虚拟设备上的 ONVIF 移动事件。这样 Unifi Protect 等 ONVIF 客户端即使代理本身从不分析视频,也能看到摄像机端的移动侦测。

获取状态有两种方式,由是否配置 `alarm.poll` 决定:

- **推送**(默认):代理充当设备的"报警服务器",接收 `POST /SendAlarmStatus`。要求设备可以配置报警服务器地址。
- **轮询**(配置了 `alarm.poll`):代理按固定间隔(默认 1 秒)调用设备的 `GetAlarmStatus`。设备端无需任何配置。适用于设备没有报警服务器设置,或拒绝 `SetAlarmServerConfig` 的情况(部分 NVR 固件对所有请求都返回 `errorCode="3"`)。

参考:*HTTP API Protocol User Guide For IP Media Device*,1.4 版(2015-12),§1.3.3 Authorization、§1.3.5 Response Message 与 §5.3 AlarmStatus。

## 1. 数据流

```
推送:NVR / 摄像机 ──HTTP POST /SendAlarmStatus──▶ 报警接收器 (alarm.port)
轮询:NVR / 摄像机 ◀──HTTP POST /GetAlarmStatus─── 报警轮询器 (alarm.poll.url)
                                               │  motionAlarm id=N
                                               ▼
                                 devices[N-1] onvif.Server.SetMotion()
                                               │
ONVIF 客户端 ◀──PullMessages (tns1:RuleEngine/CellMotionDetector/Motion …)──┘
```

## 2. 状态文档

两种方式携带相同的 XML:推送的报文体就是"GetAlarmStatus 响应中的完整 `alarmStatusInfo` 元素"(§5.3.4)。

```xml
<?xml version="1.0" encoding="UTF-8"?>
<config version="1.0" xmlns="http://www.ipc.com/ver10">
  <alarmStatusInfo>
    <motionAlarm type="boolean" id="1">true</motionAlarm>
    <motionAlarm type="boolean" id="3">true</motionAlarm>
    <sensorAlarmIn type="list" count="4"> … </sensorAlarmIn>
  </alarmStatusInfo>
</config>
```

- 以 `encoding/xml` token 流解析。元素按 local name 匹配,因此 `http://www.ipc.com/ver10` 命名空间可有可无。
- 读取所有 `motionAlarm` 元素。`id` 必须是正整数,文本必须是 `true` 或 `false`(不区分大小写,去除首尾空白)。`id` 或取值非法的元素被跳过。其他元素(`sensorAlarmIn` 等)忽略。
- 格式合法但**不含 `alarmStatusInfo` 元素**的文档不携带状态(例如只有 `<deviceInfo>` 的心跳,或 `<config status="failed" …/>` 结果)。它不会被应用,因此永远不会清除移动状态。
- XML 格式不合法的文档视为错误。

## 3. 推送接收器(`POST /SendAlarmStatus`)

在 `alarm.enabled` 为 true 且未配置 `alarm.poll` 时使用。设备向 `POST http://<alarm server>[:port]/SendAlarmStatus` 发送状态文档。

- 只服务 `POST /SendAlarmStatus`。其他路径返回 404,其他方法返回 405。
- 请求体上限 64 KiB。
- 成功响应(包括不含 `alarmStatusInfo` 的文档:确认但不应用):HTTP 200,`Content-Type: application/xml`,body 为 `<?xml version="1.0" encoding="UTF-8"?><config status="success"/>`(§1.3.5 的标准成功结果)。
- XML 格式错误或请求体超限:HTTP 400,body 为 `<config status="failed" errorCode="2"/>`(errorCode 2 = 实体 XML 格式无法识别)。
- 接收器没有认证。这类设备的报警服务器设置只有地址和端口,设备从不发送凭证。只应在可信网络上暴露该端口。

## 4. 轮询器(`GetAlarmStatus`)

在 `alarm.enabled` 为 true 且配置了 `alarm.poll` 时使用。此模式**不**启动推送监听,`alarm.port` 不会被绑定。

- 每隔 `alarm.poll.interval_ms`(默认 1000)发送一次 `POST <alarm.poll.url>/GetAlarmStatus`,请求体为空。启动时立即执行第一次轮询。轮询不会重叠:响应慢时推迟下一次轮询,而不是堆积请求。
- 认证为 HTTP Basic(§1.3.3),使用 `alarm.poll.username` / `password`,每个请求都携带。未配置用户名时不发送 `Authorization` 头。
- 每个请求超时 5 秒。响应体上限 64 KiB。
- HTTP 200 且响应体为状态文档时,与推送完全相同地应用。其他情况均为轮询失败:传输错误、超时、非 200 状态(例如凭证错误的 401,或 body 为 `<config status="failed" errorCode=…/>` 的 400)、XML 格式错误,或不含 `alarmStatusInfo` 的文档。
- 轮询失败不改变任何状态。已触发的通道保持触发,直到之后的轮询或安全超时(§5)将其清除。因此 `hold_seconds > 0` 时,录像机离线也不会让摄像机永远处于录像状态。
- 日志按边沿触发:成功后的第一次失败记录其原因,失败后的第一次成功记录为恢复。相同的失败不会每个间隔都记录。

## 5. 状态语义

每个被应用的文档是发送方全部移动侦测通道的一份**快照**。规范说明发送的是完整的 `alarmStatusInfo` 元素,实际设备往往只列出处于触发状态的通道。

- 通道 `N` 映射到 `devices[N-1]`,即设备在配置 `devices` 列表中的位置(id 从 1 开始)。超出列表长度的 id 被忽略。
- 以 `true` 出现的通道变为触发状态,并刷新其最后上报时间。
- 以 `false` 出现、**或在文档中缺失**的通道变为未触发。
- 安全超时:`alarm.hold_seconds > 0` 时,超过该秒数未再次报告 `true` 的通道会被自动清除。因此即使丢失一条"移动结束"消息(推送)或设备不可达(轮询),摄像机也不会永远处于录像状态。`0` 关闭超时。两种模式下都每秒检查一次。
- 只有状态变化才会转发到 ONVIF 层。重复的 `true` 只刷新计时,不产生新事件。
- 状态只保存在内存中。配置热重载会重建接收器/轮询器和 ONVIF server,所有通道重新回到未触发状态(轮询模式下第一次轮询会在一个间隔内恢复状态)。已有的 ONVIF pull point 订阅实际上不会丢失:客户端下一次 PullMessages 会按 id 复活其订阅(见 02 第 6 节)。

## 6. ONVIF 侧

每台虚拟设备始终暴露 Events 服务(PullPoint)。在收到报警之前移动状态一直为 `false`,因此从未收到报警的设备只会报告"无移动"。Unifi Protect 只在全新收编时发现 Events 服务,因此在 Events 服务出现之前已收编的摄像机需要移除并重新收编一次。方法清单、topic 与消息格式见 [docs/02-onvif-spec.zh-CN.md 第 6 节](02-onvif-spec.zh-CN.md#6-event-service-方法清单)。

## 7. 配置

见 [docs/03-config.zh-CN.md](03-config.zh-CN.md) 的 `alarm` 段。推送示例:

```yaml
alarm:
  enabled: true
  port: 8899          # 把 NVR 的"报警服务器"指向 <代理 IP>:8899
  hold_seconds: 60
```

在 NVR 上把报警服务器地址设为代理的 IP 和上述端口,例如通过 `SetAlarmServerConfig` 或设备的 Web 界面设置。

轮询示例(NVR 端无需配置):

```yaml
alarm:
  enabled: true
  hold_seconds: 60
  poll:
    url: http://10.0.1.57:81   # NVR HTTP API 的基础 URL
    username: admin
    password: ""               # 或设置 ONVIF_ALARM_POLL_PASSWORD
    interval_ms: 1000
```

启用前先确认设备支持轮询:`curl -u admin --basic -X POST http://<nvr>/GetAlarmStatus` 应返回含 `<alarmStatusInfo>` 的文档。
