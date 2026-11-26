# 08 · Alarm Server (TVT / ENS XML `SendAlarmStatus` / `GetAlarmStatus`)

**English** | [简体中文](08-alarm-server.zh-CN.md)

Many TVT-based NVRs and IP cameras (sold under brands such as ENS) report their alarm state over an XML HTTP API. onvif-proxy reads the motion part of that state and turns it into ONVIF motion events on the matching virtual devices. ONVIF clients such as Unifi Protect then see camera-side motion even though the proxy never looks at the video.

There are two ways to get the state, selected by whether `alarm.poll` is configured:

- **Push** (default): the proxy acts as the device's "alarm server" and receives `POST /SendAlarmStatus`. This needs the device to have a configurable alarm-server destination.
- **Poll** (`alarm.poll` set): the proxy calls the device's `GetAlarmStatus` on a fixed interval (default 1 s). Nothing has to be configured on the device. Use this when the device has no alarm-server setting, or rejects `SetAlarmServerConfig` (some NVR firmware answers `errorCode="3"` to every request).

Reference: *HTTP API Protocol User Guide For IP Media Device*, version 1.4 (2015-12), §1.3.3 Authorization, §1.3.5 Response Message and §5.3 AlarmStatus.

## 1. Data flow

```
push:  NVR / camera ──HTTP POST /SendAlarmStatus──▶ alarm receiver (alarm.port)
poll:  NVR / camera ◀──HTTP POST /GetAlarmStatus─── alarm poller (alarm.poll.url)
                                               │  motionAlarm id=N
                                               ▼
                                 devices[N-1] onvif.Server.SetMotion()
                                               │
ONVIF client ◀──PullMessages (tns1:RuleEngine/CellMotionDetector/Motion …)──┘
```

## 2. Status document

Both modes carry the same XML: the push body is "the whole `alarmStatusInfo` element in the response for GetAlarmStatus" (§5.3.4).

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

- Parsed as an `encoding/xml` token stream. Elements match by local name, so the `http://www.ipc.com/ver10` namespace is optional.
- Every `motionAlarm` element is read. `id` must be a positive integer, and the text must be `true` or `false` (case-insensitive, whitespace trimmed). An element with a bad `id` or value is skipped. Other elements (`sensorAlarmIn`, …) are ignored.
- A well-formed document **without an `alarmStatusInfo` element** carries no status (for example a heartbeat that has only `<deviceInfo>`, or a `<config status="failed" …/>` result). It is not applied, so it never clears motion.
- A document that is not well-formed XML is an error.

## 3. Push receiver (`POST /SendAlarmStatus`)

Used when `alarm.enabled` is true and `alarm.poll` is not set. The device sends `POST http://<alarm server>[:port]/SendAlarmStatus` with the status document as the body.

- Only `POST /SendAlarmStatus` is served. Any other path returns 404, and any other method returns 405.
- The body is limited to 64 KiB.
- Response on success, including a document with no `alarmStatusInfo` (acknowledged, not applied): HTTP 200, `Content-Type: application/xml`, body `<?xml version="1.0" encoding="UTF-8"?><config status="success"/>` (the standard successful result, §1.3.5).
- Response on malformed XML or an over-size body: HTTP 400 with `<config status="failed" errorCode="2"/>` (errorCode 2 = the entity's XML format is not recognized).
- The receiver has no authentication. The alarm-server settings on these devices hold only an address and a port, so the device never sends credentials. Expose the port only on a trusted network.

## 4. Poller (`GetAlarmStatus`)

Used when `alarm.enabled` is true and `alarm.poll` is set. The push listener is **not** started in this mode, so `alarm.port` is not bound.

- Every `alarm.poll.interval_ms` (default 1000) the proxy sends `POST <alarm.poll.url>/GetAlarmStatus` with an empty body. The first poll runs immediately at start. Polls never overlap: a slow response delays the next poll instead of stacking requests.
- Authentication is HTTP Basic (§1.3.3) with `alarm.poll.username` / `password`, sent on every request. With no username, no `Authorization` header is sent.
- Each request has a timeout of 5 s. The response body is limited to 64 KiB.
- An HTTP 200 response whose body is a status document is applied exactly like a push. Anything else is a failed poll: transport error, timeout, non-200 status (for example 401 for bad credentials, or 400 with a `<config status="failed" errorCode=…/>` body), malformed XML, or a document with no `alarmStatusInfo`.
- A failed poll changes no state. Channels that were active stay active until a later poll clears them or the hold timeout (§5) does, so a recorder that goes offline cannot leave a camera recording forever when `hold_seconds > 0`.
- Logging is edge-triggered: the first failure after a success is logged with its reason, and the first success after failures is logged as recovery. Repeated identical failures are not logged every interval.

## 5. State semantics

Each applied document is a **snapshot** of every motion channel on the sender. The spec says the whole `alarmStatusInfo` element is sent, and in practice devices often list only the channels that are active.

- Channel `N` maps to `devices[N-1]`, the device's position in the configured `devices` list (ids start at 1). Ids past the end of the list are ignored.
- A channel that appears with `true` becomes active, and its last-seen time is refreshed.
- A channel that appears with `false`, **or is missing from the document**, becomes inactive.
- Safety timeout: when `alarm.hold_seconds > 0`, a channel that has not been reported `true` again for that many seconds is cleared automatically. A lost "motion ended" message (push) or an unreachable device (poll) therefore can't leave a camera recording forever. `0` disables the timeout. The check runs every second in both modes.
- Only transitions are forwarded to the ONVIF layer. A repeated `true` refreshes the timer but does not emit a new event.
- State lives in memory only. A config hot reload rebuilds the receiver/poller and the ONVIF servers, so every channel starts inactive again (in poll mode the first poll restores it within one interval). Existing ONVIF pull-point subscriptions are not lost in practice: the client's next PullMessages revives its subscription by id (see 02 §6).

## 6. ONVIF side

Every virtual device always exposes the Events service (PullPoint). Motion stays `false` until an alarm arrives, so a device that never receives one just reports "no motion". Unifi Protect discovers the Events service only on a fresh adoption, so cameras adopted before the Events service existed must be removed and re-adopted once. The method list, topics and message format are in [docs/02-onvif-spec.md §6](02-onvif-spec.md#6-event-service-method-list).

## 7. Configuration

See [docs/03-config.md](03-config.md) `alarm`. Push example:

```yaml
alarm:
  enabled: true
  port: 8899          # point the NVR's "alarm server" at <proxy-ip>:8899
  hold_seconds: 60
```

On the NVR, set the alarm server address to the proxy's IP and the port above, for example with `SetAlarmServerConfig` or the device's web UI.

Poll example (nothing to configure on the NVR):

```yaml
alarm:
  enabled: true
  hold_seconds: 60
  poll:
    url: http://10.0.1.57:81   # the NVR's HTTP API base URL
    username: admin
    password: ""               # or set ONVIF_ALARM_POLL_PASSWORD
    interval_ms: 1000
```

Check that the device supports polling before enabling it: `curl -u admin --basic -X POST http://<nvr>/GetAlarmStatus` should return a document with `<alarmStatusInfo>`.
