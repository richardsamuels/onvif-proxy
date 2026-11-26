# 08 · Alarm Server (TVT / ENS XML `SendAlarmStatus`)

**English** | [简体中文](08-alarm-server.zh-CN.md)

Many TVT-based NVRs and IP cameras (sold under brands such as ENS) can push alarm state to an "alarm server" over HTTP. onvif-proxy can act as that alarm server and turn the motion state it receives into ONVIF motion events on the matching virtual devices. ONVIF clients such as Unifi Protect then see camera-side motion even though the proxy never looks at the video.

Reference: *HTTP API Protocol User Guide For IP Media Device*, version 1.4 (2015-12), §1.3.5 Response Message and §5.3 AlarmStatus.

## 1. Data flow

```
NVR / camera ──HTTP POST /SendAlarmStatus──▶ alarm receiver (alarm.port)
                                               │  motionAlarm id=N
                                               ▼
                                 devices[N-1] onvif.Server.SetMotion()
                                               │
ONVIF client ◀──PullMessages (tns1:RuleEngine/CellMotionDetector/Motion …)──┘
```

## 2. Wire protocol (receiver side)

The device sends:

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

- Only `POST /SendAlarmStatus` is served. Any other path returns 404, and any other method returns 405.
- The body is limited to 64 KiB and parsed as an `encoding/xml` token stream. Elements match by local name, so the `http://www.ipc.com/ver10` namespace is optional.
- Every `motionAlarm` element is read. `id` must be a positive integer, and the text must be `true` or `false` (case-insensitive, whitespace trimmed). An element with a bad `id` or value is skipped. Other elements (`sensorAlarmIn`, …) are ignored.
- Response on success: HTTP 200, `Content-Type: application/xml`, body `<?xml version="1.0" encoding="UTF-8"?><config status="success"/>` (the standard successful result, §1.3.5).
- Response on malformed XML or an over-size body: HTTP 400 with `<config status="failed" errorCode="2"/>` (errorCode 2 = the entity's XML format is not recognized).
- The receiver has no authentication. The alarm-server settings on these devices hold only an address and a port, so the device never sends credentials. Expose the port only on a trusted network.

## 3. State semantics

Each message is a **snapshot** of every motion channel on the sender. The spec says the whole `alarmStatusInfo` element from `GetAlarmStatus` is sent, and in practice devices often list only the channels that are active.

- Channel `N` maps to `devices[N-1]`, the device's position in the configured `devices` list (ids start at 1). Ids past the end of the list are ignored.
- A channel that appears with `true` becomes active, and its last-seen time is refreshed.
- A channel that appears with `false`, **or is missing from the message**, becomes inactive.
- Safety timeout: when `alarm.hold_seconds > 0`, a channel that has not been reported `true` again for that many seconds is cleared automatically. A lost "motion ended" message therefore can't leave a camera recording forever. `0` disables the timeout.
- Only transitions are forwarded to the ONVIF layer. A repeated `true` refreshes the timer but does not emit a new event.
- State lives in memory only. A config hot reload rebuilds the receiver and the ONVIF servers, so every channel starts inactive again. Existing ONVIF pull-point subscriptions are not lost in practice: the client's next PullMessages revives its subscription by id (see 02 §6).

## 4. ONVIF side

Every virtual device always exposes the Events service (PullPoint). Motion stays `false` until an alarm arrives, so a device that never receives one just reports "no motion". Unifi Protect discovers the Events service only on a fresh adoption, so cameras adopted before the Events service existed must be removed and re-adopted once. The method list, topics and message format are in [docs/02-onvif-spec.md §6](02-onvif-spec.md#6-event-service-method-list).

## 5. Configuration

See [docs/03-config.md](03-config.md) `alarm`. Minimal example:

```yaml
alarm:
  enabled: true
  port: 8899          # point the NVR's "alarm server" at <proxy-ip>:8899
  hold_seconds: 60
```

On the NVR, set the alarm server address to the proxy's IP and the port above, for example with `SetAlarmServerConfig` or the device's web UI.
