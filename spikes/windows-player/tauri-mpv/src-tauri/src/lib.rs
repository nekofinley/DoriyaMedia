mod player;

use std::sync::{Mutex, MutexGuard};

use player::media::{
    environment_header_count, request_from_environment, validate_media, MediaError,
    MediaLoadRequest, MediaPolicy,
};
use player::runtime::{PlaybackCommand, PlayerController, PlayerError, PlayerSnapshot};
use serde::{Deserialize, Serialize};
use tauri::{Manager, RunEvent, State};

struct AppState {
    player: Mutex<PlayerController>,
}

impl AppState {
    fn new() -> Self {
        Self {
            player: Mutex::new(PlayerController::new()),
        }
    }

    fn player(&self) -> Result<MutexGuard<'_, PlayerController>, CommandFault> {
        self.player.lock().map_err(|_| CommandFault::internal())
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

#[derive(Deserialize)]
#[serde(tag = "kind", rename_all = "camelCase")]
enum CommandRequest {
    Play,
    Pause,
    TogglePause,
    Seek { seconds: f64 },
    SetVolume { volume: f64 },
    ToggleFullscreen,
}

impl From<CommandRequest> for PlaybackCommand {
    fn from(command: CommandRequest) -> Self {
        match command {
            CommandRequest::Play => Self::Play,
            CommandRequest::Pause => Self::Pause,
            CommandRequest::TogglePause => Self::TogglePause,
            CommandRequest::Seek { seconds } => Self::Seek(seconds),
            CommandRequest::SetVolume { volume } => Self::SetVolume(volume),
            CommandRequest::ToggleFullscreen => Self::ToggleFullscreen,
        }
    }
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct EnvironmentStatus {
    media_url_configured: bool,
    header_count: usize,
    allow_loopback_http: bool,
}

#[tauri::command]
fn environment_status() -> EnvironmentStatus {
    let policy = MediaPolicy::from_environment();
    EnvironmentStatus {
        media_url_configured: std::env::var("DORIYA_W0_MEDIA_URL")
            .is_ok_and(|value| !value.is_empty()),
        header_count: environment_header_count(),
        allow_loopback_http: policy.allows_loopback_http(),
    }
}

#[tauri::command]
fn player_start(state: State<'_, AppState>) -> Result<PlayerSnapshot, CommandFault> {
    state.player()?.start().map_err(CommandFault::from)
}

#[tauri::command]
fn player_open(
    state: State<'_, AppState>,
    request: MediaLoadRequest,
) -> Result<PlayerSnapshot, CommandFault> {
    let media = validate_media(request, MediaPolicy::from_environment())?;
    state.player()?.open(media).map_err(CommandFault::from)
}

#[tauri::command]
fn player_open_from_environment(
    state: State<'_, AppState>,
) -> Result<PlayerSnapshot, CommandFault> {
    let request = request_from_environment()?;
    let media = validate_media(request, MediaPolicy::from_environment())?;
    state.player()?.open(media).map_err(CommandFault::from)
}

#[tauri::command]
fn player_command(
    state: State<'_, AppState>,
    command: CommandRequest,
) -> Result<PlayerSnapshot, CommandFault> {
    state
        .player()?
        .control(command.into())
        .map_err(CommandFault::from)
}

#[tauri::command]
fn player_status(state: State<'_, AppState>) -> Result<PlayerSnapshot, CommandFault> {
    Ok(state.player()?.status())
}

#[tauri::command]
fn player_stop(state: State<'_, AppState>) -> Result<PlayerSnapshot, CommandFault> {
    Ok(state.player()?.stop())
}

pub fn run() {
    let app = tauri::Builder::default()
        .manage(AppState::new())
        .invoke_handler(tauri::generate_handler![
            environment_status,
            player_start,
            player_open,
            player_open_from_environment,
            player_command,
            player_status,
            player_stop
        ])
        .build(tauri::generate_context!())
        .expect("failed to build the Tauri application");

    app.run(|app_handle, event| {
        if matches!(event, RunEvent::Exit | RunEvent::ExitRequested { .. }) {
            if let Some(state) = app_handle.try_state::<AppState>() {
                if let Ok(mut player) = state.player.lock() {
                    player.shutdown();
                }
            }
        }
    });
}
