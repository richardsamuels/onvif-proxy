import type { ComponentChild } from "preact";
import { useEffect, useState } from "preact/hooks";
import { apiJSON, errText, jsonBody } from "../api";
import { useT } from "../i18n";
import type { Dict } from "../i18n";
import type { DeviceSpec, DeviceView, RTSPResult, StreamInfo } from "../types";
import { AsyncButton } from "./AsyncButton";
import { Msg } from "./Msg";

const SUB_NAMES = ["sub", "mobile", "third", "fourth"];
const PROFILE_S_CODECS = ["h264", "jpeg", "mpeg4"];

function normalizeCodec(codec: string): string {
  switch (codec.trim().toLowerCase()) {
    case "h264":
    case "avc":
      return "h264";
    case "jpeg":
    case "mjpeg":
    case "jpg":
      return "jpeg";
    case "mpeg4":
      return "mpeg4";
    case "h265":
    case "hevc":
      return "h265";
    default:
      return codec.trim().toLowerCase();
  }
}

// rtspHint maps a backend err_kind to its localised explanation.
function rtspHint(t: Dict, kind: string): string {
  switch (kind) {
    case "dial_timeout":
      return t.hintDialTimeout;
    case "auth_failed":
      return t.hintAuthFailed;
    case "not_found":
      return t.hintNotFound;
    case "no_video_track":
      return t.hintNoVideoTrack;
    case "protocol_error":
      return t.hintProtocolError;
    default:
      return t.rtspProbeFailed;
  }
}

interface StreamRow {
  id: number;
  name: string;
  rtsp: string;
  codec: string;
  width: string;
  height: string;
  framerate: string;
  bitrate: string;
  proxyPort: string;
  out: ComponentChild;
}

let rowSeq = 0;
function newRow(name: string, rtsp = "", init: Partial<StreamRow> = {}): StreamRow {
  return {
    id: rowSeq++,
    name,
    rtsp,
    codec: "h264",
    width: "",
    height: "",
    framerate: "",
    bitrate: "4096",
    proxyPort: "",
    out: null,
    ...init,
  };
}

// suggestPorts finds a free soap/rtsp port pair while accounting for every
// device port and per-upstream proxy port already visible in the list DTO.
async function suggestPorts(): Promise<{ soap: number; rtsp: number }> {
  const used = new Set<number>([8080]);
  try {
    const list = await apiJSON<DeviceView[]>("/api/devices");
    for (const d of list || []) {
      used.add(d.soap_port);
      used.add(d.rtsp_port);
      for (const stream of d.endpoints.streams) {
        if (stream.proxy_port) used.add(stream.proxy_port);
      }
    }
  } catch {
    /* fall back to defaults */
  }
  const nextFree = (start: number) => {
    while (used.has(start)) start++;
    used.add(start);
    return start;
  };
  return { soap: nextFree(8081), rtsp: nextFree(8554) };
}

interface Props {
  mode: "add" | "edit";
  device?: DeviceView; // present in edit mode
  onClose: () => void;
  onSaved: () => void;
}

// DeviceModal is the single form used for both adding and editing a device. In
// edit mode it prefills from the device DTO (including per-stream source
// parameters and the ONVIF username); the password field stays blank and, left
// untouched, the backend keeps the stored password. Submit and every per-stream
// probe route through AsyncButton, so the form cannot be double-submitted and
// each button disables while its request runs.
export function DeviceModal({ mode, device, onClose, onSaved }: Props) {
  const t = useT();
  const [name, setName] = useState(device?.name ?? "");
  const [soapPort, setSoapPort] = useState(device ? String(device.soap_port) : "");
  const [rtspPort, setRtspPort] = useState(device ? String(device.rtsp_port) : "");
  const [authUser, setAuthUser] = useState(device?.auth_user ?? "");
  const [authPass, setAuthPass] = useState("");
  const [msg, setMsg] = useState<ComponentChild>(null);

  const [streams, setStreams] = useState<StreamRow[]>(() => {
    if (device) {
      return device.endpoints.streams.map((s) =>
        newRow(s.name, s.rtsp, {
          width: s.width ? String(s.width) : "",
          codec: s.codec || "h264",
          height: s.height ? String(s.height) : "",
          framerate: s.framerate ? String(s.framerate) : "",
          bitrate: s.bitrate ? String(s.bitrate) : "4096",
          proxyPort: s.proxy_port ? String(s.proxy_port) : "",
        }),
      );
    }
    return [newRow("main")];
  });

  // In add mode, suggest the next free port pair once on open.
  useEffect(() => {
    if (mode !== "add") return;
    let alive = true;
    void suggestPorts().then((p) => {
      if (!alive) return;
      setSoapPort(String(p.soap));
      setRtspPort(String(p.rtsp));
    });
    return () => {
      alive = false;
    };
  }, [mode]);

  const patchRow = (id: number, patch: Partial<StreamRow>) =>
    setStreams((prev) => prev.map((s) => (s.id === id ? { ...s, ...patch } : s)));

  const addStream = () => {
    setStreams((prev) => {
      const idx = prev.length;
      const nm = idx === 0 ? "main" : SUB_NAMES[idx] || `stream${idx}`;
      return [...prev, newRow(nm)];
    });
  };

  const removeStream = (id: number) => setStreams((prev) => prev.filter((s) => s.id !== id));

  const probe = (row: StreamRow) => async () => {
    const url = row.rtsp.trim();
    if (!url) {
      patchRow(row.id, { out: <Msg kind="bad">{t.fillRtspFirst}</Msg> });
      return;
    }
    patchRow(row.id, { out: <span class="muted">{t.probingConn}</span> });
    try {
      const j = await apiJSON<RTSPResult>("/api/test/rtsp", jsonBody({ url }));
      if (!j.ok) {
        patchRow(row.id, {
          out: (
            <Msg kind="bad">
              {rtspHint(t, j.err_kind)}
              <br />
              <span class="muted">
                {j.err_kind || ""}: {j.err_detail || ""}
              </span>
            </Msg>
          ),
        });
        return;
      }
      const tracks = (j.tracks || []).map((x) => `${x.type}:${x.codec}`).join(", ");
      const videoCodec = (j.tracks || []).find((x) => x.type.toLowerCase() === "video")?.codec;
      patchRow(row.id, {
        ...(videoCodec ? { codec: normalizeCodec(videoCodec) } : {}),
        out: <Msg kind="ok">{t.rtspOkShort(j.auth, j.latency_ms, tracks || "-")}</Msg>,
      });
      await fillStreamInfo(row.id, url);
    } catch (e) {
      patchRow(row.id, { out: <Msg kind="bad">{t.requestFailed(errText(e))}</Msg> });
    }
  };

  // fillStreamInfo probes resolution/framerate and back-fills the row inputs.
  const fillStreamInfo = async (id: number, url: string) => {
    try {
      const j = await apiJSON<StreamInfo>("/api/test/streaminfo", jsonBody({ url }));
      const patch: Partial<StreamRow> = {};
      if (j.width) patch.width = String(j.width);
      if (j.codec) patch.codec = normalizeCodec(j.codec);
      if (j.height) patch.height = String(j.height);
      if (j.fps) patch.framerate = String(j.fps);
      if (j.bitrate) patch.bitrate = String(j.bitrate);
      patch.out = (
        <Msg kind="ok">
          {t.filledBack(j.codec || "", j.width, j.height, j.fps)}
          {j.bitrate ? t.bitrateMeasured(j.bitrate) : t.bitrateUnknown}
        </Msg>
      );
      patchRow(id, patch);
    } catch (e) {
      const noFfmpeg =
        e && typeof e === "object" && "status" in e && (e as { status: number }).status === 501;
      patchRow(id, {
        out: noFfmpeg ? (
          <Msg kind="info">{t.ffmpegManual}</Msg>
        ) : (
          <Msg kind="bad">{t.fillFailed(errText(e))}</Msg>
        ),
      });
    }
  };

  const submit = async () => {
    const num = (v: string) => parseInt(v, 10) || 0;
    const spec: DeviceSpec = {
      name: name.trim(),
      soap_port: num(soapPort),
      rtsp_port: num(rtspPort),
      streams: streams.map((s) => ({
        name: s.name.trim(),
        rtsp: s.rtsp.trim(),
        codec: s.codec,
        width: num(s.width),
        height: num(s.height),
        framerate: num(s.framerate),
        bitrate: num(s.bitrate),
        ...(num(s.proxyPort) ? { proxy_port: num(s.proxyPort) } : {}),
      })),
    };
    const user = authUser.trim();
    const pass = authPass;
    if (user !== "" || pass !== "") {
      spec.auth = { username: user, password: pass };
    }

    // Client-side validation: surface every problem at once, in Chinese,
    // before the request is made (the backend still re-validates).
    const problems: string[] = [];
    if (!spec.name) problems.push(t.vNameEmpty);
    if (spec.soap_port <= 0 || spec.soap_port > 65535) problems.push(t.vSoapPort);
    if (spec.rtsp_port <= 0 || spec.rtsp_port > 65535) problems.push(t.vRtspPort);
    spec.streams.forEach((s, i) => {
      const label = t.streamRowLabel(i + 1, s.name || t.unnamedStream);
      if (!/^[a-z0-9_]+$/.test(s.name)) problems.push(t.vStreamName(label));
      if (!s.rtsp.startsWith("rtsp://")) problems.push(t.vStreamRtsp(label));
      if (!PROFILE_S_CODECS.includes(s.codec)) {
        problems.push(s.codec === "h265" ? t.vCodecH265(label) : t.vCodec(label));
      }
      if (s.width <= 0 || s.height <= 0) problems.push(t.vStreamSize(label));
      if (s.framerate <= 0) problems.push(t.vStreamFps(label));
      if (s.bitrate <= 0) problems.push(t.vStreamBitrate(label));
      if (s.proxy_port !== undefined && (s.proxy_port <= 0 || s.proxy_port > 65535)) {
        problems.push(t.vProxyPort(label));
      }
    });
    if (problems.length > 0) {
      setMsg(
        <Msg kind="bad">
          {problems.map((p) => (
            <div>{p}</div>
          ))}
        </Msg>,
      );
      return;
    }

    setMsg(<span class="muted">{t.busySubmit}</span>);
    try {
      if (mode === "edit" && device) {
        await apiJSON("/api/devices/" + encodeURIComponent(device.uuid), jsonBody(spec, "PUT"));
      } else {
        await apiJSON("/api/devices", jsonBody(spec));
      }
      onSaved();
      onClose();
    } catch (e) {
      const detail =
        e && typeof e === "object" && "detail" in e ? (e as { detail: string }).detail : "";
      setMsg(
        <>
          <Msg kind="bad">
            {mode === "edit" ? t.saveFailed : t.addFailed}: {errText(e)}
          </Msg>
          {detail && <pre>{detail}</pre>}
        </>,
      );
    }
  };

  return (
    <div class="modal" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div class="box">
        <h3>{mode === "edit" ? t.titleEdit : t.titleAdd}</h3>
        <div class="field">
          <label>{t.fieldDeviceName}</label>
          <input value={name} placeholder={t.phDeviceName} onInput={(e) => setName(e.currentTarget.value)} />
        </div>

        <div>
          {streams.map((row, idx) => (
            <div class="stream-row" key={row.id}>
              <div class="top">
                <div class="field">
                  <label>{t.fieldStreamName}</label>
                  <input value={row.name} onInput={(e) => patchRow(row.id, { name: e.currentTarget.value })} />
                </div>
                <div class="field" style="flex:3">
                  <label>{t.fieldRtspUrl}</label>
                  <input
                    value={row.rtsp}
                    placeholder={t.phRtspUrl}
                    onInput={(e) => patchRow(row.id, { rtsp: e.currentTarget.value })}
                  />
                </div>
                <AsyncButton onClick={probe(row)} busyText={t.busyProbing}>
                  {t.btnProbe}
                </AsyncButton>
                {idx !== 0 && (
                  <button type="button" class="danger" onClick={() => removeStream(row.id)}>
                    {t.btnRemove}
                  </button>
                )}
              </div>
              <div class="grid6">
                <div class="field">
                  <label>{t.fieldCodec}</label>
                  <select value={row.codec} onInput={(e) => patchRow(row.id, { codec: e.currentTarget.value })}>
                    {!PROFILE_S_CODECS.includes(row.codec) && (
                      <option value={row.codec} disabled>
                        {t.codecUnsupported(row.codec)}
                      </option>
                    )}
                    <option value="h264">H.264</option>
                    <option value="jpeg">JPEG / MJPEG</option>
                    <option value="mpeg4">MPEG-4</option>
                  </select>
                </div>
                <div class="field">
                  <label>{t.fieldWidth}</label>
                  <input type="number" value={row.width} onInput={(e) => patchRow(row.id, { width: e.currentTarget.value })} />
                </div>
                <div class="field">
                  <label>{t.fieldHeight}</label>
                  <input type="number" value={row.height} onInput={(e) => patchRow(row.id, { height: e.currentTarget.value })} />
                </div>
                <div class="field">
                  <label>{t.fieldFramerate}</label>
                  <input type="number" value={row.framerate} onInput={(e) => patchRow(row.id, { framerate: e.currentTarget.value })} />
                </div>
                <div class="field">
                  <label>{t.fieldBitrate}</label>
                  <input type="number" value={row.bitrate} onInput={(e) => patchRow(row.id, { bitrate: e.currentTarget.value })} />
                </div>
                <div class="field">
                  <label>{t.fieldProxyPort}</label>
                  <input
                    type="number"
                    value={row.proxyPort}
                    disabled={idx === 0}
                    placeholder={idx === 0 ? t.phPrimaryProxyPort : t.phProxyPort}
                    onInput={(e) => patchRow(row.id, { proxyPort: e.currentTarget.value })}
                  />
                </div>
              </div>
              {row.out && <div class="s-out">{row.out}</div>}
            </div>
          ))}
        </div>

        <p class="muted">{t.rtspAuthNote}</p>
        <button type="button" onClick={addStream}>
          {t.addSubStream}
        </button>

        <div class="inline" style="margin-top:12px">
          <div class="field">
            <label>{t.fieldSoapPort}</label>
            <input type="number" value={soapPort} onInput={(e) => setSoapPort(e.currentTarget.value)} />
          </div>
          <div class="field">
            <label>{t.fieldRtspPort}</label>
            <input type="number" value={rtspPort} onInput={(e) => setRtspPort(e.currentTarget.value)} />
          </div>
        </div>

        <div class="inline">
          <div class="field">
            <label>{t.fieldOnvifUser}</label>
            <input value={authUser} onInput={(e) => setAuthUser(e.currentTarget.value)} />
          </div>
          <div class="field">
            <label>{t.fieldOnvifPass}</label>
            <input
              type="password"
              value={authPass}
              placeholder={mode === "edit" && device?.auth_user ? t.phKeepPass : ""}
              onInput={(e) => setAuthPass(e.currentTarget.value)}
            />
          </div>
        </div>
        <p class="muted">{t.onvifAuthNote}</p>

        {msg}
        <div class="actions">
          <button type="button" onClick={onClose}>
            {t.btnCancel}
          </button>
          <AsyncButton className="primary" busyText={t.busySubmit} onClick={submit}>
            {mode === "edit" ? t.btnSave : t.btnAdd}
          </AsyncButton>
        </div>
      </div>
    </div>
  );
}
