import { invoke } from "@tauri-apps/api/core";
import "./styles.css";

type PlayerState =
  | "stopped"
  | "starting"
  | "ready"
  | "opening"
  | "playing"
  | "paused"
  | "exited"
  | "error";

interface PlayerSnapshot {
  state: PlayerState;
  pid: number | null;
  mediaLoaded: boolean;
  positionSeconds: number | null;
  durationSeconds: number | null;
  paused: boolean | null;
  volume: number | null;
  lastErrorCode: string | null;
}

interface EnvironmentStatus {
  mediaUrlConfigured: boolean;
  audioUrlConfigured: boolean;
  headerCount: number;
  allowLoopbackHttp: boolean;
  debugBuild: boolean;
  autoOpen: boolean;
}

interface PlaybackCommand {
  kind:
    | "play"
    | "pause"
    | "togglePause"
    | "seek"
    | "setVolume";
  seconds?: number;
  volume?: number;
}

interface WindowLayoutState {
  fullscreen: boolean;
}

interface SurfaceBounds {
  x: number;
  y: number;
  width: number;
  height: number;
}

interface WindowDiagnostics {
  debugBuild: boolean;
  fullscreen: boolean;
  tauriScaleFactor: number;
  tauriInnerWidth: number;
  tauriInnerHeight: number;
  monitor: {
    name: string | null;
    width: number;
    height: number;
    x: number;
    y: number;
    scaleFactor: number;
  } | null;
  surfaceCreated: boolean;
  surfaceVisible: boolean;
  surfaceBounds: {
    x: number;
    y: number;
    width: number;
    height: number;
  } | null;
  native: {
    clientWidth: number;
    clientHeight: number;
    dpi: number;
    dpiScale: number;
    monitorLeft: number;
    monitorTop: number;
    monitorRight: number;
    monitorBottom: number;
    workLeft: number;
    workTop: number;
    workRight: number;
    workBottom: number;
    mpvChild: {
      className: string;
      ownerPid: number;
      parentMatches: boolean;
      style: number;
      visible: boolean;
      width: number;
      height: number;
      clientWidth: number;
      clientHeight: number;
    } | null;
  } | null;
}

interface SafeCommandError {
  code?: unknown;
}

const errorLabels: Readonly<Record<string, string>> = {
  media_url_required: "未提供媒体 URL",
  media_url_too_long: "媒体 URL 超过长度限制",
  media_url_invalid: "媒体 URL 无法解析",
  media_url_userinfo: "媒体 URL 禁止包含用户名或密码",
  media_url_fragment: "媒体 URL 禁止包含 fragment",
  media_url_scheme: "媒体 URL 仅允许 HTTPS",
  media_url_insecure: "非 loopback HTTP 被拒绝",
  media_url_host_not_public: "媒体 URL 不能指向私网或本机主机",
  media_audio_url_duplicate: "音频 URL 必须与主媒体 URL 不同",
  media_audio_headers_mismatch: "双流必须使用完全相同的白名单 Header",
  header_not_allowed: "Header 名称不在白名单",
  header_duplicate: "Header 名称重复",
  header_value_invalid: "Header 值含非法字符或格式错误",
  header_too_large: "Header 超过长度限制",
  mpv_executable_missing: "找不到 mpv 可执行文件",
  mpv_identity_missing: "未配置 mpv SHA-256 身份",
  mpv_identity_invalid: "mpv SHA-256 身份格式无效",
  mpv_identity_mismatch: "mpv 文件与固定 SHA-256 不匹配",
  mpv_publisher_invalid: "mpv 发布者 SHA-256 身份格式无效",
  mpv_signature_missing: "mpv 文件缺少 Authenticode 签名",
  mpv_signature_invalid: "mpv 文件 Authenticode 签名无效或不受信任",
  mpv_publisher_mismatch: "mpv 文件签名发布者与固定身份不匹配",
  mpv_start_failed: "mpv 启动失败",
  mpv_media_open_timeout: "等待媒体开始播放超时",
  mpv_ipc_timeout: "mpv JSON IPC 超时",
  mpv_ipc_io: "mpv JSON IPC 连接中断",
  mpv_ipc_protocol: "mpv JSON IPC 响应不符合协议",
  mpv_ipc_rejected: "mpv 拒绝命令",
  mpv_ipc_peer_mismatch: "mpv IPC 端点身份校验失败",
  mpv_media_load_failed: "mpv 媒体加载失败",
  mpv_media_audio_load_failed: "mpv 外部音频流加载失败",
  mpv_media_dual_origin_unsupported: "当前出站模式不支持跨 origin 双流",
  mpv_egress_mode_unsupported: "当前出站模式不支持该媒体来源",
  mpv_exited: "mpv sidecar 已退出",
  mpv_busy: "播放器正忙",
  invalid_command: "控制命令超出白名单或参数范围",
  internal_error: "本地命令层内部错误",
  video_surface_unavailable: "原生视频表面不可用或拒绝当前边界",
  window_control_failed: "主窗口原生操作失败",
  debug_only: "窗口诊断仅在 debug 构建可用",
};

function element<T extends HTMLElement>(id: string): T {
  const found = document.getElementById(id);
  if (found === null) {
    throw new Error(`missing element: ${id}`);
  }
  return found as T;
}

const ui = {
  statusDot: element<HTMLSpanElement>("status-dot"),
  statusLabel: element<HTMLSpanElement>("status-label"),
  runtimeMode: element<HTMLElement>("runtime-mode"),
  metricState: element<HTMLElement>("metric-state"),
  metricSubstate: element<HTMLElement>("metric-substate"),
  metricPid: element<HTMLElement>("metric-pid"),
  metricPosition: element<HTMLElement>("metric-position"),
  metricDuration: element<HTMLElement>("metric-duration"),
  metricVolume: element<HTMLElement>("metric-volume"),
  metricPaused: element<HTMLElement>("metric-paused"),
  loadForm: element<HTMLFormElement>("load-form"),
  mediaUrl: element<HTMLInputElement>("media-url"),
  audioUrl: element<HTMLInputElement>("audio-url"),
  referer: element<HTMLInputElement>("header-referer"),
  userAgent: element<HTMLInputElement>("header-user-agent"),
  origin: element<HTMLInputElement>("header-origin"),
  loadButton: element<HTMLButtonElement>("load-button"),
  startButton: element<HTMLButtonElement>("start-button"),
  envButton: element<HTMLButtonElement>("env-button"),
  envNote: element<HTMLElement>("env-note"),
  seekBack: element<HTMLButtonElement>("seek-back"),
  seekForward: element<HTMLButtonElement>("seek-forward"),
  pauseButton: element<HTMLButtonElement>("pause-button"),
  volume: element<HTMLInputElement>("volume"),
  volumeOutput: element<HTMLOutputElement>("volume-output"),
  fullscreenButton: element<HTMLButtonElement>("fullscreen-button"),
  diagnosticsButton: element<HTMLButtonElement>("diagnostics-button"),
  diagnosticsNote: element<HTMLElement>("diagnostics-note"),
  stopButton: element<HTMLButtonElement>("stop-button"),
  eventList: element<HTMLOListElement>("event-list"),
  videoStage: element<HTMLElement>("video-stage"),
};

const isTauri = "__TAURI_INTERNALS__" in window;
let environmentReady = false;
let pollInFlight = false;
let actionInFlight = false;
let eventCount = 0;
let lastLoggedErrorCode: string | null = null;
let latestSnapshot: PlayerSnapshot | null = null;
let surfaceVisible = false;
let surfaceSyncFrame: number | null = null;
let surfaceSyncInFlight = false;
let surfaceSyncPending = false;
let lastSurfaceSyncKey: string | null = null;
let surfaceReady = false;
let fullscreen = false;

function recordEvent(message: string): void {
  const item = document.createElement("li");
  const timestamp = document.createElement("time");
  const text = document.createElement("span");
  const now = new Date();
  timestamp.textContent = now.toLocaleTimeString("zh-CN", {
    hour12: false,
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
  text.textContent = message;
  item.append(timestamp, text);
  ui.eventList.prepend(item);
  eventCount += 1;
  while (eventCount > 80) {
    ui.eventList.lastElementChild?.remove();
    eventCount -= 1;
  }
}

function safeErrorLabel(error: unknown): string {
  if (typeof error !== "object" || error === null) {
    return errorLabels.internal_error ?? "本地命令失败";
  }
  const code = (error as SafeCommandError).code;
  if (typeof code === "string" && Object.hasOwn(errorLabels, code)) {
    return errorLabels[code] ?? errorLabels.internal_error ?? "本地命令失败";
  }
  return errorLabels.internal_error ?? "本地命令失败";
}

function formatTime(value: number | null): string {
  if (value === null || !Number.isFinite(value) || value < 0) {
    return "—";
  }
  const seconds = Math.floor(value);
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const remainder = seconds % 60;
  if (hours > 0) {
    return `${hours}:${String(minutes).padStart(2, "0")}:${String(remainder).padStart(2, "0")}`;
  }
  return `${minutes}:${String(remainder).padStart(2, "0")}`;
}

function stateLabel(state: PlayerState): string {
  const labels: Record<PlayerState, string> = {
    stopped: "已停止",
    starting: "启动中",
    ready: "Sidecar 就绪",
    opening: "正在装载",
    playing: "播放中",
    paused: "已暂停",
    exited: "进程已退出",
    error: "命令失败",
  };
  return labels[state];
}

function renderSnapshot(snapshot: PlayerSnapshot): void {
  latestSnapshot = snapshot;
  ui.metricState.textContent = snapshot.state.toUpperCase();
  ui.metricSubstate.textContent = stateLabel(snapshot.state);
  ui.metricPid.textContent = snapshot.pid === null ? "—" : String(snapshot.pid);
  ui.metricPosition.textContent = formatTime(snapshot.positionSeconds);
  ui.metricDuration.textContent = `duration ${formatTime(snapshot.durationSeconds)}`;
  ui.metricVolume.textContent = snapshot.volume === null ? "—" : `${Math.round(snapshot.volume)}%`;
  ui.metricPaused.textContent =
    snapshot.paused === null ? "pause unknown" : snapshot.paused ? "paused" : "running";
  ui.statusLabel.textContent = snapshot.lastErrorCode
    ? (errorLabels[snapshot.lastErrorCode] ?? "播放器状态异常")
    : stateLabel(snapshot.state);
  ui.statusDot.dataset.live = String(
    snapshot.state !== "stopped" && snapshot.state !== "exited" && snapshot.state !== "error",
  );
  if (
    snapshot.lastErrorCode !== null &&
    snapshot.lastErrorCode !== lastLoggedErrorCode
  ) {
    recordEvent(`状态错误 / ${snapshot.lastErrorCode}`);
    lastLoggedErrorCode = snapshot.lastErrorCode;
  } else if (snapshot.lastErrorCode === null) {
    lastLoggedErrorCode = null;
  }
  const shouldShowSurface =
    snapshot.state !== "stopped" &&
    snapshot.state !== "exited" &&
    snapshot.state !== "error";
  if (shouldShowSurface !== surfaceVisible) {
    surfaceVisible = shouldShowSurface;
    scheduleSurfaceSync();
    window.setTimeout(scheduleSurfaceSync, 80);
    window.setTimeout(scheduleSurfaceSync, 260);
    window.setTimeout(scheduleSurfaceSync, 600);
  }
}

async function callPlayer<T>(command: string, args?: Record<string, unknown>): Promise<T> {
  if (!isTauri) {
    throw new Error("Tauri runtime unavailable");
  }
  return invoke<T>(command, args);
}

async function runAction(
  label: string,
  action: () => Promise<void>,
): Promise<void> {
  if (actionInFlight) {
    return;
  }
  actionInFlight = true;
  setControlsDisabled(true);
  try {
    await action();
  } catch (error: unknown) {
    const message = safeErrorLabel(error);
    recordEvent(`${label}失败 / ${message}`);
    ui.statusLabel.textContent = message;
    ui.statusDot.dataset.live = "false";
  } finally {
    actionInFlight = false;
    setControlsDisabled(false);
  }
}

function setControlsDisabled(disabled: boolean): void {
  for (const button of [
    ui.loadButton,
    ui.startButton,
    ui.envButton,
    ui.seekBack,
    ui.seekForward,
    ui.pauseButton,
    ui.fullscreenButton,
    ui.diagnosticsButton,
    ui.stopButton,
  ]) {
    button.disabled = disabled;
  }
  ui.envButton.disabled = disabled || !environmentReady;
}

function scheduleSurfaceSync(): void {
  if (!isTauri || surfaceSyncFrame !== null) {
    return;
  }
  surfaceSyncFrame = window.requestAnimationFrame(() => {
    surfaceSyncFrame = null;
    void syncSurface();
  });
}

async function syncSurface(): Promise<boolean> {
  if (!isTauri) {
    return false;
  }
  if (surfaceSyncInFlight) {
    surfaceSyncPending = true;
    return false;
  }
  const rect = ui.videoStage.getBoundingClientRect();
  const left = Math.max(0, rect.left);
  const top = Math.max(0, rect.top);
  const right = Math.min(window.innerWidth, rect.right);
  const bottom = Math.min(window.innerHeight, rect.bottom);
  const width = Math.max(0, right - left);
  const height = Math.max(0, bottom - top);
  const dpr = window.devicePixelRatio || 1;
  const request = {
    x: Math.round(left * dpr),
    y: Math.round(top * dpr),
    width: Math.round(width * dpr),
    height: Math.round(height * dpr),
    visible:
      surfaceVisible &&
      latestSnapshot !== null &&
      width >= 2 &&
      height >= 2,
  };
  const key = [
    request.x,
    request.y,
    request.width,
    request.height,
    request.visible ? 1 : 0,
  ].join(":");
  if (key === lastSurfaceSyncKey) {
    return true;
  }
  surfaceSyncInFlight = true;
  try {
    const applied = await callPlayer<SurfaceBounds>("sync_video_surface", { request });
    lastSurfaceSyncKey = [
      applied.x,
      applied.y,
      applied.width,
      applied.height,
      request.visible ? 1 : 0,
    ].join(":");
    surfaceReady = true;
    return true;
  } catch {
    // A transient resize during teardown should not flood the event log.
    lastSurfaceSyncKey = null;
    surfaceReady = false;
    return false;
  } finally {
    surfaceSyncInFlight = false;
    if (surfaceSyncPending) {
      surfaceSyncPending = false;
      scheduleSurfaceSync();
    }
  }
}

async function ensureSurfaceSynced(): Promise<boolean> {
  for (let attempt = 0; attempt < 4; attempt += 1) {
    if (await syncSurface()) {
      return true;
    }
    await new Promise((resolve) => window.setTimeout(resolve, 50));
  }
  return false;
}

function applyFullscreenState(next: boolean): void {
  fullscreen = next;
  lastSurfaceSyncKey = null;
  document.documentElement.classList.toggle("shell--fullscreen", next);
  ui.fullscreenButton.textContent = next ? "退出主窗口全屏" : "进入主窗口全屏";
  scheduleSurfaceSync();
  window.setTimeout(scheduleSurfaceSync, 80);
  window.setTimeout(scheduleSurfaceSync, 260);
}

function clearSensitiveInputs(): void {
  ui.mediaUrl.value = "";
  ui.audioUrl.value = "";
  ui.referer.value = "";
  ui.userAgent.value = "";
  ui.origin.value = "";
}

function buildHeaders(): Record<string, string> {
  const headers: Record<string, string> = {};
  const candidates: ReadonlyArray<readonly [string, HTMLInputElement]> = [
    ["Referer", ui.referer],
    ["User-Agent", ui.userAgent],
    ["Origin", ui.origin],
  ];
  for (const [name, input] of candidates) {
    const value = input.value.trim();
    if (value !== "") {
      headers[name] = value;
    }
  }
  return headers;
}

async function sendCommand(command: PlaybackCommand): Promise<void> {
  const snapshot = await callPlayer<PlayerSnapshot>("player_command", { command });
  renderSnapshot(snapshot);
}

async function refreshStatus(): Promise<void> {
  if (!isTauri || pollInFlight || actionInFlight) {
    return;
  }
  pollInFlight = true;
  try {
    const snapshot = await callPlayer<PlayerSnapshot>("player_status");
    renderSnapshot(snapshot);
  } catch (error: unknown) {
    ui.statusLabel.textContent = safeErrorLabel(error);
    ui.statusDot.dataset.live = "false";
  } finally {
    pollInFlight = false;
  }
}

ui.loadForm.addEventListener("submit", (event) => {
  event.preventDefault();
  const url = ui.mediaUrl.value.trim();
  const audioUrl = ui.audioUrl.value.trim();
  if (url === "") {
    recordEvent("装载被拒绝 / URL 为空");
    return;
  }
  const headers = buildHeaders();
  const request: {
    url: string;
    headers: Record<string, string>;
    audio?: { url: string; headers: Record<string, string> };
  } = { url, headers };
  if (audioUrl !== "") {
    request.audio = { url: audioUrl, headers };
  }
  clearSensitiveInputs();
  void runAction("装载", async () => {
    const snapshot = await callPlayer<PlayerSnapshot>("player_open", {
      request,
    });
    renderSnapshot(snapshot);
    recordEvent("装载命令已受理 / 输入已清空");
  });
});

ui.startButton.addEventListener("click", () => {
  void runAction("启动", async () => {
    const snapshot = await callPlayer<PlayerSnapshot>("player_start");
    renderSnapshot(snapshot);
    recordEvent("mpv sidecar 已启动");
  });
});

ui.envButton.addEventListener("click", () => {
  void runAction("环境装载", async () => {
    const snapshot = await callPlayer<PlayerSnapshot>("player_open_from_environment");
    renderSnapshot(snapshot);
    recordEvent("环境媒体已提交到受控命令层");
  });
});

ui.pauseButton.addEventListener("click", () => {
  void runAction("暂停切换", () => sendCommand({ kind: "togglePause" }));
});

ui.seekBack.addEventListener("click", () => {
  void runAction("后退", () => sendCommand({ kind: "seek", seconds: -10 }));
});

ui.seekForward.addEventListener("click", () => {
  void runAction("前进", () => sendCommand({ kind: "seek", seconds: 10 }));
});

async function switchFullscreen(next: boolean): Promise<void> {
  const layout = await callPlayer<WindowLayoutState>("set_main_fullscreen", {
    fullscreen: next,
  });
  applyFullscreenState(layout.fullscreen);
  recordEvent(layout.fullscreen ? "主窗口进入全屏 / 视频表面随视口同步" : "主窗口退出全屏");
}

ui.fullscreenButton.addEventListener("click", () => {
  void runAction("全屏切换", () => switchFullscreen(!fullscreen));
});

ui.diagnosticsButton.addEventListener("click", () => {
  void runAction("窗口诊断", async () => {
    const diagnostics = await callPlayer<WindowDiagnostics>("window_diagnostics");
    const monitor = diagnostics.monitor;
    const native = diagnostics.native;
    ui.diagnosticsNote.textContent = [
      `window=${diagnostics.tauriInnerWidth}x${diagnostics.tauriInnerHeight}`,
      `scale=${diagnostics.tauriScaleFactor.toFixed(2)}`,
      `nativeDpi=${native ? `${native.dpi} (${native.dpiScale.toFixed(2)}x)` : "unavailable"}`,
      `surface=${diagnostics.surfaceVisible ? "visible" : "hidden"} ${diagnostics.surfaceBounds?.width ?? 0}x${diagnostics.surfaceBounds?.height ?? 0}`,
      `mpv=${native?.mpvChild ? `${native.mpvChild.className || "unknown"} pid=${native.mpvChild.ownerPid} ${native.mpvChild.visible ? "visible" : "hidden"} ${native.mpvChild.clientWidth}x${native.mpvChild.clientHeight}` : "missing"}`,
      monitor
        ? `monitor=${monitor.width}x${monitor.height}@${monitor.scaleFactor.toFixed(2)}`
        : "monitor=unknown",
    ].join(" · ");
    recordEvent(`窗口诊断 / ${ui.diagnosticsNote.textContent}`);
  });
});

ui.stopButton.addEventListener("click", () => {
  void runAction("停止", async () => {
    const snapshot = await callPlayer<PlayerSnapshot>("player_stop");
    renderSnapshot(snapshot);
    recordEvent("停止完成 / 子进程句柄已清理");
  });
});

ui.volume.addEventListener("input", () => {
  ui.volumeOutput.textContent = ui.volume.value;
});

ui.volume.addEventListener("change", () => {
  const volume = Number(ui.volume.value);
  void runAction("音量", () => sendCommand({ kind: "setVolume", volume }));
});

window.addEventListener("resize", scheduleSurfaceSync);
window.addEventListener("scroll", scheduleSurfaceSync, {
  capture: true,
  passive: true,
});
window.addEventListener("animationend", scheduleSurfaceSync, true);
window.addEventListener("transitionend", scheduleSurfaceSync, true);
window.visualViewport?.addEventListener("resize", scheduleSurfaceSync);
window.visualViewport?.addEventListener("scroll", scheduleSurfaceSync);
document.addEventListener("visibilitychange", scheduleSurfaceSync);

document.addEventListener("keydown", (event) => {
  if (event.key === "F11" && !actionInFlight) {
    event.preventDefault();
    void runAction("全屏切换", () => switchFullscreen(!fullscreen));
    return;
  }
  if (event.key === "Escape" && fullscreen && !actionInFlight) {
    event.preventDefault();
    void runAction("退出全屏", () => switchFullscreen(false));
  }
});

async function bootstrap(): Promise<void> {
  recordEvent("界面已加载 / 无媒体自动请求");
  if (!isTauri) {
    ui.runtimeMode.textContent = "BROWSER PREVIEW · COMMANDS OFFLINE";
    ui.statusLabel.textContent = "请通过 Tauri 启动";
    ui.metricSubstate.textContent = "浏览器仅用于检查布局";
    setControlsDisabled(true);
    return;
  }

  try {
    setControlsDisabled(true);
    const environment = await callPlayer<EnvironmentStatus>("environment_status");
    environmentReady = environment.mediaUrlConfigured;
    ui.diagnosticsButton.hidden = !environment.debugBuild;
    ui.envNote.textContent = environment.mediaUrlConfigured
      ? `环境输入可用：主媒体 URL 已配置${environment.audioUrlConfigured ? "，外部音频 URL 已配置" : ""}，${environment.headerCount} 个白名单 Header${environment.allowLoopbackHttp ? "，允许 loopback HTTP" : ""}。`
      : "未检测到 DORIYA_W0_MEDIA_URL；可使用上方受控输入。输入值不会持久化。";
    const snapshot = await callPlayer<PlayerSnapshot>("player_status");
    renderSnapshot(snapshot);
    if (!(await ensureSurfaceSynced()) || !surfaceReady) {
      throw { code: "video_surface_unavailable" };
    }
    setControlsDisabled(false);
    recordEvent("Rust 命令层与原生视频表面可用");
    new ResizeObserver(scheduleSurfaceSync).observe(ui.videoStage);
    if (environment.autoOpen) {
      recordEvent("debug 自动装载已启用");
      await runAction("自动装载", async () => {
        const opened = await callPlayer<PlayerSnapshot>("player_open_from_environment");
        renderSnapshot(opened);
        recordEvent("环境媒体已提交到受控命令层");
      });
    }
  } catch (error: unknown) {
    ui.statusLabel.textContent = safeErrorLabel(error);
    recordEvent(`初始化失败 / ${safeErrorLabel(error)}`);
    setControlsDisabled(true);
  }

  window.setInterval(() => {
    void refreshStatus();
  }, 1000);
}

void bootstrap();
