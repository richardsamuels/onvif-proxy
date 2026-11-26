# 08 · 报警服务器(TVT / ENS XML `SendAlarmStatus`)

[English](08-alarm-server.md) | **简体中文**

许多基于 TVT 方案的 NVR 与 IP 摄像机(以 ENS 等品牌销售)可以通过 HTTP 把报警状态推送给"报警服务器"。onvif-proxy 可以充当这个报警服务器,把收到的移动侦测状态转换为对应虚拟设备上的 ONVIF 移动事件。这样 Unifi Protect 等 ONVIF 客户端即使代理本身从不分析视频,也能看到摄像机端的移动侦测。

参考:*HTTP API Protocol User Guide For IP Media Device*,1.4 版(2015-12),§1.3.5 Response Message 与 §5.3 AlarmStatus。

## 1. 数据流

```
NVR / 摄像机 ──HTTP POST /SendAlarmStatus──▶ 报警接收器 (alarm.port)
                                               │  motionAlarm id=N
                                               ▼
                                 devices[N-1] onvif.Server.SetMotion()
                                               │
ONVIF 客户端 ◀──PullMessages (tns1:RuleEngine/CellMotionDetector/Motion …)──┘
```

## 2. 报文协议(接收端)

设备发送:

```
POST http://<alarm server>[:port]/SendAlarmStatus HTTP/1.1
Content-Type: application/xml

<?xml version="1.0" encoding="UTF-8"?>
<config version="1.0" xmlns="http://www.ipc.com/ver10">
  <alarmStatusInfo>
    <motionAlarm type="boolean" id="1">true</motionAlarm>
    <motionAlarm type="boolean" id="3">true</motionAlarm>
    <sensorAlarmIn type="list" count="4"> … </sensorAlarmIn>
  </alarmStatusInfo>
</config>
```

- 只服务 `POST /SendAlarmStatus`。其他路径返回 404,其他方法返回 405。
- 请求体上限 64 KiB,以 `encoding/xml` token 流解析。元素按 local name 匹配,因此 `http://www.ipc.com/ver10` 命名空间可有可无。
- 读取所有 `motionAlarm` 元素。`id` 必须是正整数,文本必须是 `true` 或 `false`(不区分大小写,去除首尾空白)。`id` 或取值非法的元素被跳过。其他元素(`sensorAlarmIn` 等)忽略。
- 成功响应:HTTP 200,`Content-Type: application/xml`,body 为 `<?xml version="1.0" encoding="UTF-8"?><config status="success"/>`(§1.3.5 的标准成功结果)。
- XML 格式错误或请求体超限:HTTP 400,body 为 `<config status="failed" errorCode="2"/>`(errorCode 2 = 实体 XML 格式无法识别)。
- 接收器没有认证。这类设备的报警服务器设置只有地址和端口,设备从不发送凭证。只应在可信网络上暴露该端口。

## 3. 状态语义

每条消息是发送方全部移动侦测通道的一份**快照**。规范说明发送的是 `GetAlarmStatus` 的完整 `alarmStatusInfo` 元素,实际设备往往只列出处于触发状态的通道。

- 通道 `N` 映射到 `devices[N-1]`,即设备在配置 `devices` 列表中的位置(id 从 1 开始)。超出列表长度的 id 被忽略。
- 以 `true` 出现的通道变为触发状态,并刷新其最后上报时间。
- 以 `false` 出现、**或在消息中缺失**的通道变为未触发。
- 安全超时:`alarm.hold_seconds > 0` 时,超过该秒数未再次报告 `true` 的通道会被自动清除。因此即使丢失一条"移动结束"消息,摄像机也不会永远处于录像状态。`0` 关闭超时。
- 只有状态变化才会转发到 ONVIF 层。重复的 `true` 只刷新计时,不产生新事件。
- 状态只保存在内存中。配置热重载会重建接收器和 ONVIF server,所有通道重新回到未触发状态。已有的 ONVIF pull point 订阅实际上不会丢失:客户端下一次 PullMessages 会按 id 复活其订阅(见 02 第 6 节)。

## 4. ONVIF 侧

每台虚拟设备始终暴露 Events 服务(PullPoint)。在收到报警之前移动状态一直为 `false`,因此从未收到报警的设备只会报告"无移动"。Unifi Protect 只在全新收编时发现 Events 服务,因此在 Events 服务出现之前已收编的摄像机需要移除并重新收编一次。方法清单、topic 与消息格式见 [docs/02-onvif-spec.zh-CN.md 第 6 节](02-onvif-spec.zh-CN.md#6-event-service-方法清单)。

## 5. 配置

见 [docs/03-config.zh-CN.md](03-config.zh-CN.md) 的 `alarm` 段。最小示例:

```yaml
alarm:
  enabled: true
  port: 8899          # 把 NVR 的"报警服务器"指向 <代理 IP>:8899
  hold_seconds: 60
```

在 NVR 上把报警服务器地址设为代理的 IP 和上述端口,例如通过 `SetAlarmServerConfig` 或设备的 Web 界面设置。
