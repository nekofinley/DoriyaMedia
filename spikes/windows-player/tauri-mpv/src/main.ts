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
  headerCount: number;
  allowLoopbackHttp: boolean;
}

interface PlaybackCommand {
  kind:
    | "play"
    | "pause"
    | "togglePause"
    | "seek"
    | "setVolume"
    | "toggleFullscreen";
  seconds?: number;
  volume?: number;
  enabled?: boolean;
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
  header_not_allowed: "Header 名称不在白名单",
  header_duplicate: "Header 名称重复",
  header_value_invalid: "Header 值含非法字符或格式错误",
  header_too_large: "Header 超过长度限制",
  mpv_executable_missing: "找不到 mpv 可执行文件",
  mpv_start_failed: "mpv 启动失败",
  mpv_ipc_timeout: "mpv JSON IPC 超时",
  mpv_ipc_io: "mpv JSON IPC 连接中断",
  mpv_ipc_protocol: "mpv JSON IPC 响应不符合协议",
  mpv_ipc_rejected: "mpv 拒绝命令",
  mpv_ipc_peer_mismatch: "mpv IPC 端点身份校验失败",
  mpv_media_load_failed: "mpv 媒体加载失败",
  mpv_exited: "mpv sidecar 已退出",
  mpv_busy: "播放器正忙",
  invalid_command: "控制命令超出白名单或参数范围",
  internal_error: "本地命令层内部错误",
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
  stopButton: element<HTMLButtonElement>("stop-button"),
  eventList: element<HTMLOListElement>("event-list"),
};

const isTauri = "__TAURI_INTERNALS__" in window;
let environmentReady = false;
let pollInFlight = false;
let actionInFlight = false;
let eventCount = 0;
let lastLoggedErrorCode: string | null = null;

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
    ui.stopButton,
  ]) {
    button.disabled = disabled;
  }
  ui.envButton.disabled = disabled || !environmentReady;
}

function clearSensitiveInputs(): void {
  ui.mediaUrl.value = "";
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
  if (url === "") {
    recordEvent("装载被拒绝 / URL 为空");
    return;
  }
  const headers = buildHeaders();
  clearSensitiveInputs();
  void runAction("装载", async () => {
    const snapshot = await callPlayer<PlayerSnapshot>("player_open", {
      request: { url, headers },
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

ui.fullscreenButton.addEventListener("click", () => {
  void runAction("全屏切换", () => sendCommand({ kind: "toggleFullscreen" }));
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
    const environment = await callPlayer<EnvironmentStatus>("environment_status");
    environmentReady = environment.mediaUrlConfigured;
    ui.envNote.textContent = environment.mediaUrlConfigured
      ? `环境输入可用：URL 已配置，${environment.headerCount} 个白名单 Header${environment.allowLoopbackHttp ? "，允许 loopback HTTP" : ""}。`
      : "未检测到 DORIYA_W0_MEDIA_URL；可使用上方受控输入。输入值不会持久化。";
    setControlsDisabled(false);
    const snapshot = await callPlayer<PlayerSnapshot>("player_status");
    renderSnapshot(snapshot);
    recordEvent("Rust 命令层可用");
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
