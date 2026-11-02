mod player;

use std::io::Write;
use std::sync::{mpsc, Mutex, MutexGuard};
use std::time::Duration;

use player::media::{
    environment_header_count, environment_media_url, request_from_environment, validate_media,
    MediaError, MediaLoadRequest, MediaPolicy,
};
use player::runtime::{PlaybackCommand, PlayerController, PlayerError, PlayerSnapshot};
use player::video_surface::{self, NativeWindowMetrics, SurfaceBounds, VideoSurface};
use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Manager, RunEvent, WebviewWindow};

struct AppState {
    player: Mutex<PlayerController>,
    surface: Mutex<Option<VideoSurface>>,
}

impl AppState {
    fn new() -> Self {
        Self {
            player: Mutex::new(PlayerController::new()),
            surface: Mutex::new(None),
        }
    }

    fn player(&self) -> Result<MutexGuard<'_, PlayerController>, CommandFault> {
        self.player.lock().map_err(|_| CommandFault::internal())
    }

    fn video_surface(&self) -> Result<Option<VideoSurface>, CommandFault> {
        self.surface
            .lock()
            .map(|surface| *surface)
            .map_err(|_| CommandFault::internal())
    }
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct CommandFault {
    code: &'static str,
    message: &'static str,
}

impl CommandFault {
    const fn new(code: &'static str, message: &'static str) -> Self {
        Self { code, message }
    }

    const fn internal() -> Self {
        Self::new(
            "internal_error",
            "The local command layer could not acquire player state.",
        )
    }

    const fn surface() -> Self {
        Self::new(
            "video_surface_unavailable",
            "The native video surface is unavailable or rejected the requested bounds.",
        )
    }

    const fn window() -> Self {
        Self::new(
            "window_control_failed",
            "The native window operation could not be completed.",
        )
    }
}

impl From<MediaError> for CommandFault {
    fn from(error: MediaError) -> Self {
        Self::new(error.code(), error.message())
    }
}

impl From<PlayerError> for CommandFault {
    fn from(error: PlayerError) -> Self {
        Self::new(error.code(), error.message())
    }
}

fn trace_redacted(event: &str) {
    if !cfg!(debug_assertions) {
        return;
    }
    let Ok(path) = std::env::var("DORIYA_W0_DEBUG_TRACE_PATH") else {
        return;
    };
    let Ok(mut file) = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
    else {
        return;
    };
    let _ = writeln!(file, "{event}");
}

// Tauri commands can run on worker threads, so Win32 window operations must
// be marshalled back to the event-loop thread and joined with a bounded wait.
fn run_on_main_thread_result<T, F>(window: &WebviewWindow, operation: F) -> Result<T, CommandFault>
where
    T: Send + 'static,
    F: FnOnce(&tauri::AppHandle) -> Result<T, CommandFault> + Send + 'static,
{
    let app = window.app_handle().clone();
    let (sender, receiver) = mpsc::sync_channel(1);
    window
        .run_on_main_thread(move || {
            let _ = sender.send(operation(&app));
        })
        .map_err(|_| CommandFault::window())?;
    receiver
        .recv_timeout(Duration::from_secs(3))
        .map_err(|_| CommandFault::window())?
}

async fn run_player_on_blocking_thread<T, F>(
    app: AppHandle,
    operation: F,
) -> Result<T, CommandFault>
where
    T: Send + 'static,
    F: FnOnce(&AppState) -> Result<T, CommandFault> + Send + 'static,
{
    tauri::async_runtime::spawn_blocking(move || operation(&app.state::<AppState>()))
        .await
        .map_err(|_| CommandFault::internal())?
}

#[derive(Deserialize)]
#[serde(tag = "kind", rename_all = "camelCase")]
enum CommandRequest {
    Play,
    Pause,
    TogglePause,
    Seek { seconds: f64 },
    SetVolume { volume: f64 },
}

impl From<CommandRequest> for PlaybackCommand {
    fn from(command: CommandRequest) -> Self {
        match command {
            CommandRequest::Play => Self::Play,
            CommandRequest::Pause => Self::Pause,
            CommandRequest::TogglePause => Self::TogglePause,
            CommandRequest::Seek { seconds } => Self::Seek(seconds),
            CommandRequest::SetVolume { volume } => Self::SetVolume(volume),
        }
    }
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct EnvironmentStatus {
    media_url_configured: bool,
    header_count: usize,
    allow_loopback_http: bool,
    debug_build: bool,
    auto_open: bool,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct SurfaceSyncRequest {
    x: f64,
    y: f64,
    width: f64,
    height: f64,
    visible: bool,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct WindowLayoutState {
    fullscreen: bool,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct MonitorDiagnostics {
    name: Option<String>,
    width: u32,
    height: u32,
    x: i32,
    y: i32,
    scale_factor: f64,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct WindowDiagnostics {
    debug_build: bool,
    fullscreen: bool,
    tauri_scale_factor: f64,
    tauri_inner_width: u32,
    tauri_inner_height: u32,
    tauri_outer_x: i32,
    tauri_outer_y: i32,
    monitor: Option<MonitorDiagnostics>,
    surface_created: bool,
    surface_visible: bool,
    surface_bounds: Option<SurfaceBounds>,
    native: Option<NativeWindowMetrics>,
}

fn surface_target(state: &AppState) -> Result<isize, CommandFault> {
    state
        .video_surface()?
        .filter(|surface| surface.is_valid())
        .map(VideoSurface::hwnd)
        .ok_or_else(CommandFault::surface)
}

fn physical_coordinate(value: f64) -> Result<i32, CommandFault> {
    if value.is_finite() && (0.0..=100_000.0).contains(&value) {
        Ok(value.round() as i32)
    } else {
        Err(CommandFault::new(
            "invalid_command",
            "The native surface bounds are outside the accepted range.",
        ))
    }
}

#[tauri::command]
fn environment_status() -> EnvironmentStatus {
    trace_redacted("environment_status");
    let policy = MediaPolicy::from_environment();
    let media_url_configured = environment_media_url().is_some();
    EnvironmentStatus {
        media_url_configured,
        header_count: environment_header_count(),
        allow_loopback_http: policy.allows_loopback_http(),
        debug_build: cfg!(debug_assertions),
        auto_open: cfg!(debug_assertions)
            && media_url_configured
            && std::env::var("DORIYA_W0_AUTO_OPEN").is_ok_and(|value| value == "1"),
    }
}

#[tauri::command]
async fn player_start(app: AppHandle) -> Result<PlayerSnapshot, CommandFault> {
    run_player_on_blocking_thread(app, |state| {
        let target = surface_target(state)?;
        trace_redacted("player_start_surface_target");
        let mut player = state.player()?;
        player.set_video_target(target)?;
        player.start().map_err(CommandFault::from)
    })
    .await
}

#[tauri::command]
async fn player_open(
    app: AppHandle,
    request: MediaLoadRequest,
) -> Result<PlayerSnapshot, CommandFault> {
    run_player_on_blocking_thread(app, move |state| {
        let media = validate_media(request, MediaPolicy::from_environment())?;
        let target = surface_target(state)?;
        let mut player = state.player()?;
        player.set_video_target(target)?;
        player.open(media).map_err(CommandFault::from)
    })
    .await
}

#[tauri::command]
async fn player_open_from_environment(app: AppHandle) -> Result<PlayerSnapshot, CommandFault> {
    run_player_on_blocking_thread(app, |state| {
        let request = request_from_environment()?;
        let media = validate_media(request, MediaPolicy::from_environment())?;
        let target = surface_target(state)?;
        let mut player = state.player()?;
        player.set_video_target(target)?;
        player.open(media).map_err(CommandFault::from)
    })
    .await
}

#[tauri::command]
async fn player_command(
    app: AppHandle,
    command: CommandRequest,
) -> Result<PlayerSnapshot, CommandFault> {
    run_player_on_blocking_thread(app, move |state| {
        let target = surface_target(state)?;
        let mut player = state.player()?;
        player.set_video_target(target)?;
        player.control(command.into()).map_err(CommandFault::from)
    })
    .await
}

#[tauri::command]
async fn player_status(app: AppHandle) -> Result<PlayerSnapshot, CommandFault> {
    run_player_on_blocking_thread(app, |state| Ok(state.player()?.status())).await
}

#[tauri::command]
async fn player_stop(app: AppHandle) -> Result<PlayerSnapshot, CommandFault> {
    run_player_on_blocking_thread(app, |state| Ok(state.player()?.stop())).await
}

#[tauri::command]
async fn sync_video_surface(
    window: WebviewWindow,
    request: SurfaceSyncRequest,
) -> Result<SurfaceBounds, CommandFault> {
    trace_redacted("sync_video_surface");
    let bounds = SurfaceBounds {
        x: physical_coordinate(request.x)?,
        y: physical_coordinate(request.y)?,
        width: physical_coordinate(request.width)?,
        height: physical_coordinate(request.height)?,
    };
    let main_window = window.clone();
    run_on_main_thread_result(&window, move |app| {
        let parent_hwnd = main_window.hwnd().map_err(|_| CommandFault::window())?.0 as isize;
        let state = app.state::<AppState>();
        let mut surface = state.surface.lock().map_err(|_| CommandFault::internal())?;
        if surface.as_ref().is_some_and(|surface| !surface.is_valid()) {
            *surface = None;
            trace_redacted("video_surface_invalidated");
        }
        if surface.is_none() {
            *surface =
                Some(video_surface::create(parent_hwnd).map_err(|_| CommandFault::surface())?);
            trace_redacted("video_surface_created");
        }
        let surface = surface.as_mut().ok_or_else(CommandFault::surface)?;
        if surface.parent_hwnd() != parent_hwnd {
            return Err(CommandFault::surface());
        }
        let changed = surface
            .set_bounds(bounds, request.visible)
            .map_err(|_| CommandFault::surface())?;
        if changed {
            trace_redacted(if surface.visible() {
                "video_surface_visible"
            } else {
                "video_surface_hidden"
            });
        }
        Ok(surface.bounds())
    })
}

#[tauri::command]
async fn set_main_fullscreen(
    window: WebviewWindow,
    fullscreen: bool,
) -> Result<WindowLayoutState, CommandFault> {
    let main_window = window.clone();
    run_on_main_thread_result(&window, move |_app| {
        if main_window
            .is_fullscreen()
            .map_err(|_| CommandFault::window())?
            != fullscreen
        {
            main_window
                .set_fullscreen(fullscreen)
                .map_err(|_| CommandFault::window())?;
        }
        let fullscreen = main_window
            .is_fullscreen()
            .map_err(|_| CommandFault::window())?;
        Ok(WindowLayoutState { fullscreen })
    })
}

#[tauri::command]
async fn window_diagnostics(window: WebviewWindow) -> Result<WindowDiagnostics, CommandFault> {
    trace_redacted("window_diagnostics");
    if !cfg!(debug_assertions) {
        return Err(CommandFault::new(
            "debug_only",
            "Window diagnostics are available only in debug builds.",
        ));
    }
    let main_window = window.clone();
    run_on_main_thread_result(&window, move |app| {
        let parent_hwnd = main_window.hwnd().map_err(|_| CommandFault::window())?.0 as isize;
        let inner_size = main_window
            .inner_size()
            .map_err(|_| CommandFault::window())?;
        let outer_position = main_window
            .outer_position()
            .map_err(|_| CommandFault::window())?;
        let scale_factor = main_window
            .scale_factor()
            .map_err(|_| CommandFault::window())?;
        let monitor = main_window
            .current_monitor()
            .map_err(|_| CommandFault::window())?
            .map(|monitor| MonitorDiagnostics {
                name: monitor.name().cloned(),
                width: monitor.size().width,
                height: monitor.size().height,
                x: monitor.position().x,
                y: monitor.position().y,
                scale_factor: monitor.scale_factor(),
            });
        let fullscreen = main_window
            .is_fullscreen()
            .map_err(|_| CommandFault::window())?;
        let state = app.state::<AppState>();
        let surface = state.video_surface()?;
        let native = video_surface::diagnostics(parent_hwnd, surface).ok();
        Ok(WindowDiagnostics {
            debug_build: cfg!(debug_assertions),
            fullscreen,
            tauri_scale_factor: scale_factor,
            tauri_inner_width: inner_size.width,
            tauri_inner_height: inner_size.height,
            tauri_outer_x: outer_position.x,
            tauri_outer_y: outer_position.y,
            monitor,
            surface_created: surface.is_some(),
            surface_visible: surface.is_some_and(VideoSurface::visible),
            surface_bounds: surface.map(VideoSurface::bounds),
            native,
        })
    })
}

pub fn run() {
    trace_redacted("run_start");
    let app = tauri::Builder::default()
        .manage(AppState::new())
        .on_page_load(|webview, _payload| {
            trace_redacted(if webview.label() == "main" {
                "main_page_loaded"
            } else {
                "secondary_page_loaded"
            });
        })
        .invoke_handler(tauri::generate_handler![
            environment_status,
            player_start,
            player_open,
            player_open_from_environment,
            player_command,
            player_status,
            player_stop,
            sync_video_surface,
            set_main_fullscreen,
            window_diagnostics
        ])
        .build(tauri::generate_context!())
        .expect("failed to build the Tauri application");

    app.run(|app_handle, event| {
        if matches!(event, RunEvent::Exit | RunEvent::ExitRequested { .. }) {
            if let Some(state) = app_handle.try_state::<AppState>() {
                if let Ok(mut player) = state.player.try_lock() {
                    player.shutdown();
                }
            }
        }
    });
}
