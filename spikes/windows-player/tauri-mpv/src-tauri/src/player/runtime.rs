use std::env;
use std::ffi::OsString;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitStatus, Stdio};
use std::sync::mpsc::{self, RecvTimeoutError, Sender, SyncSender};
use std::thread;
use std::time::{Duration, Instant};

use serde::Serialize;
use serde_json::{json, Value};

use super::ipc::{EndFileReason, IpcConnection, IpcError, IpcReply, JsonPipe, MpvEvent};
use super::media::ValidatedMedia;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
// Keep every request bounded without turning a transient scheduler stall into a
// permanent player failure on a busy desktop.
const IPC_REQUEST_TIMEOUT: Duration = Duration::from_secs(5);
const GRACEFUL_EXIT_TIMEOUT: Duration = Duration::from_millis(1500);
const FORCED_EXIT_TIMEOUT: Duration = Duration::from_secs(2);
const PIPE_NAME_PREFIX: &str = r"\\.\pipe\doriya-w0-";
const SIDECAR_ENV_ALLOWLIST: &[&str] = &[
    "ALLUSERSPROFILE",
    "APPDATA",
    "COMMONPROGRAMFILES",
    "COMMONPROGRAMFILES(X86)",
    "COMMONPROGRAMW6432",
    "COMSPEC",
    "HOMEDRIVE",
    "HOMEPATH",
    "LOCALAPPDATA",
    "NUMBER_OF_PROCESSORS",
    "OS",
    "PATH",
    "PATHEXT",
    "PROCESSOR_ARCHITECTURE",
    "PROCESSOR_IDENTIFIER",
    "PROGRAMDATA",
    "PROGRAMFILES",
    "PROGRAMFILES(X86)",
    "PROGRAMW6432",
    "PUBLIC",
    "SESSIONNAME",
    "SYSTEMDRIVE",
    "SYSTEMROOT",
    "TEMP",
    "TMP",
    "USERDOMAIN",
    "USERNAME",
    "USERPROFILE",
    "WINDIR",
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PlayerError {
    MpvExecutableMissing,
    MpvStartFailed,
    IpcTimeout,
    IpcIo,
    IpcProtocol,
    IpcRejected,
    IpcPeerMismatch,
    MediaLoadFailed,
    Exited,
    InvalidCommand,
    Internal,
}

impl PlayerError {
    pub const fn code(self) -> &'static str {
        match self {
            Self::MpvExecutableMissing => "mpv_executable_missing",
            Self::MpvStartFailed => "mpv_start_failed",
            Self::IpcTimeout => "mpv_ipc_timeout",
            Self::IpcIo => "mpv_ipc_io",
            Self::IpcProtocol => "mpv_ipc_protocol",
            Self::IpcRejected => "mpv_ipc_rejected",
            Self::IpcPeerMismatch => "mpv_ipc_peer_mismatch",
            Self::MediaLoadFailed => "mpv_media_load_failed",
            Self::Exited => "mpv_exited",
            Self::InvalidCommand => "invalid_command",
            Self::Internal => "internal_error",
        }
    }

    pub const fn message(self) -> &'static str {
        match self {
            Self::MpvExecutableMissing => "The mpv executable could not be found.",
            Self::MpvStartFailed => "The mpv sidecar could not be started.",
            Self::IpcTimeout => "Timed out while communicating with the mpv IPC pipe.",
            Self::IpcIo => "The mpv IPC connection failed.",
            Self::IpcProtocol => "mpv returned an invalid IPC response.",
            Self::IpcRejected => "mpv rejected the command.",
            Self::IpcPeerMismatch => {
                "The mpv IPC pipe did not belong to the process launched by Doriya."
            }
            Self::MediaLoadFailed => "mpv could not load the requested media.",
            Self::Exited => "The mpv sidecar exited.",
            Self::InvalidCommand => "The playback command or value is invalid.",
            Self::Internal => "An internal player error occurred.",
        }
    }
}

impl From<IpcError> for PlayerError {
    fn from(error: IpcError) -> Self {
        match error {
            IpcError::Io | IpcError::Closed => Self::IpcIo,
            IpcError::Timeout => Self::IpcTimeout,
            IpcError::Protocol => Self::IpcProtocol,
            IpcError::Rejected => Self::IpcRejected,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum PlayerState {
    Stopped,
    Starting,
    Ready,
    Opening,
    Playing,
    Paused,
    Exited,
    Error,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PlayerSnapshot {
    pub state: PlayerState,
    pub pid: Option<u32>,
    pub media_loaded: bool,
    pub position_seconds: Option<f64>,
    pub duration_seconds: Option<f64>,
    pub paused: Option<bool>,
    pub volume: Option<f64>,
    pub last_error_code: Option<&'static str>,
}

pub enum PlaybackCommand {
    Play,
    Pause,
    TogglePause,
    Seek(f64),
    SetVolume(f64),
    ToggleFullscreen,
}

pub struct PlayerController {
    runtime: Option<MpvRuntime>,
    state: PlayerState,
    media_loaded: bool,
    last_error_code: Option<&'static str>,
    status_metrics: PlaybackMetrics,
    pending_load: Option<PendingLoad>,
}

#[derive(Default)]
struct PlaybackMetrics {
    position_seconds: Option<f64>,
    duration_seconds: Option<f64>,
    paused: Option<bool>,
    volume: Option<f64>,
}

struct PendingLoad {
    saw_start: bool,
}

struct CommandReply {
    data: Option<Value>,
    events: Vec<MpvEvent>,
}

impl PlayerController {
    pub fn new() -> Self {
        Self {
            runtime: None,
            state: PlayerState::Stopped,
            media_loaded: false,
            last_error_code: None,
            status_metrics: PlaybackMetrics::default(),
            pending_load: None,
        }
    }

    pub fn start(&mut self) -> Result<PlayerSnapshot, PlayerError> {
        self.start_with(MpvRuntime::spawn)
    }

    fn start_with(
        &mut self,
        spawn: impl FnOnce() -> Result<MpvRuntime, PlayerError>,
    ) -> Result<PlayerSnapshot, PlayerError> {
        if let Some(runtime) = self.runtime.as_mut() {
            match runtime.try_reap() {
                Ok(false) => {}
                Ok(true) => {
                    self.runtime.take();
                }
                Err(error) => {
                    self.fail_and_cleanup(error);
                    return Err(error);
                }
            }
        }
        if self.runtime.is_none() {
            self.clear_media_state();
            self.state = PlayerState::Starting;
            self.last_error_code = None;
            match spawn() {
                Ok(runtime) => self.runtime = Some(runtime),
                Err(error) => {
                    self.state = PlayerState::Error;
                    self.last_error_code = Some(error.code());
                    log_code("mpv_start_error", error.code());
                    return Err(error);
                }
            }
        }
        self.last_error_code = None;
        self.state = PlayerState::Ready;
        Ok(self.snapshot())
    }

    pub fn open(&mut self, media: ValidatedMedia) -> Result<PlayerSnapshot, PlayerError> {
        self.ensure_runtime()?;
        // Flush events already queued for the previous load before starting a new generation.
        if let Err(error) =
            self.runtime_command_ignoring_events(&json!(["get_property", "mpv-version"]))
        {
            self.fail_and_cleanup(error);
            return Err(error);
        }

        self.begin_load();
        self.state = PlayerState::Opening;
        self.last_error_code = None;
        log_scheme("media_open_requested", media.url_scheme());

        let headers = media.header_fields();
        log_count("header_allowlist_count", headers.len());
        let result = (|| {
            // Any end-file from the previous item belongs to the old generation.
            self.runtime_command_ignoring_events(&json!(["stop"]))?;
            self.runtime_command_ignoring_events(&json!([
                "set_property",
                "http-header-fields",
                headers
            ]))?;
            self.runtime_command(&json!(["loadfile", media.url()]))?;
            self.runtime_command(&json!(["set_property", "pause", false]))?;
            Ok::<(), PlayerError>(())
        })();

        if let Err(error) = result {
            self.fail_and_cleanup(error);
            return Err(error);
        }

        if let Some(error) = self.terminal_event_error() {
            return Err(error);
        }

        self.status_metrics.paused = Some(false);
        self.state = if self.media_loaded
            && (self.status_metrics.position_seconds.is_some()
                || self.status_metrics.duration_seconds.is_some())
        {
            PlayerState::Playing
        } else {
            PlayerState::Opening
        };
        Ok(self.snapshot())
    }

    pub fn control(&mut self, command: PlaybackCommand) -> Result<PlayerSnapshot, PlayerError> {
        match command {
            PlaybackCommand::Seek(seconds) if !seconds.is_finite() || seconds.abs() > 86_400.0 => {
                return Err(PlayerError::InvalidCommand);
            }
            PlaybackCommand::SetVolume(volume)
                if !volume.is_finite() || !(0.0..=130.0).contains(&volume) =>
            {
                return Err(PlayerError::InvalidCommand);
            }
            _ => {}
        }

        self.ensure_runtime()?;

        let result = (|| {
            let (next_state, next_paused, next_volume) = match command {
                PlaybackCommand::Play => {
                    self.runtime_command(&json!(["set_property", "pause", false]))?;
                    (Some(PlayerState::Playing), Some(false), None)
                }
                PlaybackCommand::Pause => {
                    self.runtime_command(&json!(["set_property", "pause", true]))?;
                    (Some(PlayerState::Paused), Some(true), None)
                }
                PlaybackCommand::TogglePause => {
                    let paused = !self
                        .runtime_command(&json!(["get_property", "pause"]))?
                        .data
                        .and_then(|value| value.as_bool())
                        .ok_or(PlayerError::IpcProtocol)?;
                    self.runtime_command(&json!(["set_property", "pause", paused]))?;
                    (
                        Some(if paused {
                            PlayerState::Paused
                        } else {
                            PlayerState::Playing
                        }),
                        Some(paused),
                        None,
                    )
                }
                PlaybackCommand::Seek(seconds) => {
                    self.runtime_command(&json!(["seek", seconds, "relative"]))?;
                    (None, None, None)
                }
                PlaybackCommand::SetVolume(volume) => {
                    self.runtime_command(&json!(["set_property", "volume", volume]))?;
                    (None, None, Some(volume))
                }
                PlaybackCommand::ToggleFullscreen => {
                    self.runtime_command(&json!(["cycle", "fullscreen"]))?;
                    (None, None, None)
                }
            };
            Ok::<_, PlayerError>((next_state, next_paused, next_volume))
        })();

        let (next_state, next_paused, next_volume) = match result {
            Ok(effects) => effects,
            Err(error) => {
                self.fail_and_cleanup(error);
                return Err(error);
            }
        };

        if let Some(state) = next_state {
            self.state = state;
        }
        if let Some(paused) = next_paused {
            self.status_metrics.paused = Some(paused);
        }
        if let Some(volume) = next_volume {
            self.status_metrics.volume = Some(volume);
        }

        if let Some(error) = self.terminal_event_error() {
            return Err(error);
        }

        Ok(self.snapshot())
    }

    pub fn status(&mut self) -> PlayerSnapshot {
        if self.runtime.is_none() {
            self.clear_media_state();
            if matches!(
                self.state,
                PlayerState::Starting
                    | PlayerState::Ready
                    | PlayerState::Opening
                    | PlayerState::Playing
                    | PlayerState::Paused
            ) {
                self.state = PlayerState::Error;
                self.last_error_code = Some(PlayerError::Internal.code());
            }
            return self.snapshot();
        }
        if self.check_runtime_exit().is_err() {
            return self.snapshot();
        }
        if self.terminal_event_error().is_some() {
            return self.snapshot();
        }

        let result = self.read_status_metrics();
        if let Err(error) = result {
            self.fail_and_cleanup(error);
            return self.snapshot();
        }

        if self.terminal_event_error().is_some() {
            return self.snapshot();
        }

        if self.media_loaded {
            if self.status_metrics.position_seconds.is_some()
                || self.status_metrics.duration_seconds.is_some()
            {
                self.state = if self.status_metrics.paused == Some(true) {
                    PlayerState::Paused
                } else {
                    PlayerState::Playing
                };
            } else {
                self.state = PlayerState::Opening;
            }
        } else if self.pending_load.is_some() {
            self.state = PlayerState::Opening;
        } else if self.runtime.is_some() {
            self.state = PlayerState::Ready;
        }

        self.snapshot()
    }

    pub fn stop(&mut self) -> PlayerSnapshot {
        if let Some(mut runtime) = self.runtime.take() {
            runtime.shutdown();
        }
        self.state = PlayerState::Stopped;
        self.clear_media_state();
        self.last_error_code = None;
        self.snapshot()
    }

    pub fn shutdown(&mut self) {
        let _ = self.stop();
    }

    fn ensure_runtime(&mut self) -> Result<(), PlayerError> {
        if self.runtime.is_none() {
            self.start()?;
        } else {
            self.check_runtime_exit()?;
        }
        Ok(())
    }

    fn check_runtime_exit(&mut self) -> Result<(), PlayerError> {
        let reap_result = self
            .runtime
            .as_mut()
            .map(MpvRuntime::try_reap)
            .ok_or(PlayerError::Exited)?;

        match reap_result {
            Ok(false) => Ok(()),
            Ok(true) => {
                self.runtime.take();
                self.clear_media_state();
                self.state = PlayerState::Exited;
                self.last_error_code = Some(PlayerError::Exited.code());
                log_code("mpv_exited", PlayerError::Exited.code());
                Err(PlayerError::Exited)
            }
            Err(_) => {
                self.runtime.take();
                self.clear_media_state();
                self.state = PlayerState::Error;
                self.last_error_code = Some(PlayerError::Internal.code());
                log_code("mpv_reap_failed", PlayerError::Internal.code());
                Err(PlayerError::Internal)
            }
        }
    }

    fn read_status_metrics(&mut self) -> Result<(), PlayerError> {
        let paused = self
            .runtime_optional_command(&json!(["get_property", "pause"]))?
            .data
            .and_then(|value| value.as_bool());
        if self.terminal_event_error().is_some() {
            return Ok(());
        }
        let position = self
            .runtime_optional_command(&json!(["get_property", "time-pos"]))?
            .data
            .and_then(numeric_or_null);
        if self.terminal_event_error().is_some() {
            return Ok(());
        }
        let duration = self
            .runtime_optional_command(&json!(["get_property", "duration"]))?
            .data
            .and_then(numeric_or_null);
        if self.terminal_event_error().is_some() {
            return Ok(());
        }
        let volume = self
            .runtime_optional_command(&json!(["get_property", "volume"]))?
            .data
            .and_then(numeric_or_null);
        self.status_metrics.paused = paused;
        self.status_metrics.position_seconds = position;
        self.status_metrics.duration_seconds = duration;
        self.status_metrics.volume = volume;
        Ok(())
    }

    fn begin_load(&mut self) {
        self.pending_load = Some(PendingLoad { saw_start: false });
        self.media_loaded = false;
        self.status_metrics = PlaybackMetrics::default();
    }

    fn runtime_command(&mut self, command: &Value) -> Result<CommandReply, PlayerError> {
        let reply = self.runtime_mut()?.command(command)?;
        self.apply_events(reply.events);
        Ok(CommandReply {
            data: reply.data,
            events: Vec::new(),
        })
    }

    fn runtime_optional_command(&mut self, command: &Value) -> Result<CommandReply, PlayerError> {
        let reply = self.runtime_mut()?.optional_command(command)?;
        self.apply_events(reply.events);
        Ok(CommandReply {
            data: reply.data,
            events: Vec::new(),
        })
    }

    fn runtime_command_ignoring_events(
        &mut self,
        command: &Value,
    ) -> Result<CommandReply, PlayerError> {
        self.runtime_mut()?.command(command)
    }

    fn apply_events(&mut self, events: Vec<MpvEvent>) {
        for event in events {
            self.apply_event(event);
        }
    }

    fn apply_event(&mut self, event: MpvEvent) {
        match event {
            MpvEvent::StartFile => {
                if let Some(pending) = self.pending_load.as_mut() {
                    pending.saw_start = true;
                    self.media_loaded = false;
                    self.status_metrics = PlaybackMetrics::default();
                    self.state = PlayerState::Opening;
                    self.last_error_code = None;
                }
            }
            MpvEvent::FileLoaded => {
                if self
                    .pending_load
                    .as_ref()
                    .is_some_and(|pending| pending.saw_start)
                {
                    self.media_loaded = true;
                    self.last_error_code = None;
                    self.state = PlayerState::Opening;
                }
            }
            MpvEvent::EndFile(reason) => {
                if !self
                    .pending_load
                    .as_ref()
                    .is_some_and(|pending| pending.saw_start)
                {
                    return;
                }

                match reason {
                    EndFileReason::Redirect => {
                        if let Some(pending) = self.pending_load.as_mut() {
                            pending.saw_start = false;
                        }
                        self.media_loaded = false;
                        self.state = PlayerState::Opening;
                    }
                    EndFileReason::Error | EndFileReason::Unknown => {
                        self.pending_load = None;
                        self.media_loaded = false;
                        self.status_metrics = PlaybackMetrics::default();
                        self.state = PlayerState::Error;
                        self.last_error_code = Some(PlayerError::MediaLoadFailed.code());
                    }
                    EndFileReason::Eof | EndFileReason::Stop | EndFileReason::Quit => {
                        self.pending_load = None;
                        self.media_loaded = false;
                        self.status_metrics = PlaybackMetrics::default();
                        self.state = PlayerState::Exited;
                        self.last_error_code = Some(PlayerError::Exited.code());
                    }
                }
            }
        }
    }

    fn terminal_event_error(&self) -> Option<PlayerError> {
        match self.state {
            PlayerState::Error => Some(PlayerError::MediaLoadFailed),
            PlayerState::Exited => Some(PlayerError::Exited),
            _ => None,
        }
    }

    fn runtime_mut(&mut self) -> Result<&mut MpvRuntime, PlayerError> {
        self.runtime.as_mut().ok_or(PlayerError::Exited)
    }

    fn fail_and_cleanup(&mut self, error: PlayerError) {
        if let Some(mut runtime) = self.runtime.take() {
            runtime.abort();
        }
        self.clear_media_state();
        self.state = if error == PlayerError::Exited {
            PlayerState::Exited
        } else {
            PlayerState::Error
        };
        self.last_error_code = Some(error.code());
    }

    fn clear_media_state(&mut self) {
        self.media_loaded = false;
        self.status_metrics = PlaybackMetrics::default();
        self.pending_load = None;
    }

    fn snapshot(&self) -> PlayerSnapshot {
        PlayerSnapshot {
            state: self.state,
            pid: self.runtime.as_ref().and_then(MpvRuntime::pid),
            media_loaded: self.media_loaded,
            position_seconds: self.status_metrics.position_seconds,
            duration_seconds: self.status_metrics.duration_seconds,
            paused: self.status_metrics.paused,
            volume: self.status_metrics.volume,
            last_error_code: self.last_error_code,
        }
    }
}

impl Drop for PlayerController {
    fn drop(&mut self) {
        self.shutdown();
    }
}

struct MpvRuntime {
    child: Option<Child>,
    ipc: Option<IpcWorker>,
    next_request_id: u64,
}

impl MpvRuntime {
    fn spawn() -> Result<Self, PlayerError> {
        let executable = mpv_executable();
        let pipe_path = next_pipe_path()?;
        let args = mpv_arguments(&pipe_path);

        let mut command = Command::new(executable);
        command
            .args(args)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        configure_sidecar_environment(&mut command);
        configure_hidden_process(&mut command);

        let mut child = command.spawn().map_err(|error| {
            if error.kind() == std::io::ErrorKind::NotFound {
                PlayerError::MpvExecutableMissing
            } else {
                PlayerError::MpvStartFailed
            }
        })?;

        let deadline = Instant::now() + CONNECT_TIMEOUT;
        let ipc = loop {
            if child
                .try_wait()
                .map_err(|_| PlayerError::MpvStartFailed)?
                .is_some()
            {
                let _ = child.wait();
                return Err(PlayerError::MpvStartFailed);
            }
            match JsonPipe::connect(&pipe_path, Duration::from_millis(80)) {
                Ok(ipc) => break ipc,
                Err(_) if Instant::now() < deadline => {
                    thread::sleep(Duration::from_millis(40));
                }
                Err(_) => {
                    terminate_child(&mut child);
                    return Err(PlayerError::IpcTimeout);
                }
            }
        };

        if let Err(error) = verify_ipc_server_pid(&ipc, child.id()) {
            terminate_child(&mut child);
            return Err(error);
        }

        let mut runtime = Self {
            child: Some(child),
            ipc: Some(IpcWorker::new(Box::new(ipc), IPC_REQUEST_TIMEOUT)),
            next_request_id: 1,
        };

        if let Err(error) = runtime.command(&json!(["get_property", "mpv-version"])) {
            runtime.abort();
            return Err(error);
        }
        if let Some(pid) = runtime.pid() {
            log_pid("mpv_started", pid);
        }
        Ok(runtime)
    }

    fn command(&mut self, command: &Value) -> Result<CommandReply, PlayerError> {
        let reply = self.optional_command(command)?;
        if reply.data.is_some() {
            Ok(reply)
        } else {
            Err(PlayerError::IpcRejected)
        }
    }

    fn optional_command(&mut self, command: &Value) -> Result<CommandReply, PlayerError> {
        let command = command.as_array().ok_or(PlayerError::InvalidCommand)?;
        self.try_reap()?;
        let request_id = self.next_request_id;
        self.next_request_id = self.next_request_id.saturating_add(1);
        let reply = self
            .ipc
            .as_mut()
            .ok_or(PlayerError::Exited)?
            .request(request_id, command);
        match reply {
            Ok(reply) => Ok(CommandReply {
                data: reply.data,
                events: reply.events,
            }),
            Err(error) => Err(PlayerError::from(error)),
        }
    }

    fn try_reap(&mut self) -> Result<bool, PlayerError> {
        let Some(child) = self.child.as_mut() else {
            return Ok(true);
        };
        match child.try_wait().map_err(|_| PlayerError::Internal)? {
            Some(_) => {
                self.child = None;
                self.ipc = None;
                Ok(true)
            }
            None => Ok(false),
        }
    }

    fn pid(&self) -> Option<u32> {
        self.child.as_ref().map(Child::id)
    }

    fn shutdown(&mut self) {
        let quit = json!(["quit"]);
        if let (Some(ipc), Some(command)) = (self.ipc.as_mut(), quit.as_array()) {
            let _ = ipc.notify(command);
        }
        if let Some(child) = self.child.as_mut() {
            wait_for_exit(child, GRACEFUL_EXIT_TIMEOUT);
            terminate_child(child);
        }
        self.child = None;
        self.ipc = None;
    }

    fn abort(&mut self) {
        if let Some(child) = self.child.as_mut() {
            terminate_child(child);
        }
        self.child = None;
        self.ipc = None;
    }

    #[cfg(test)]
    fn from_test_parts(child: Child, ipc: Box<dyn IpcConnection>) -> Self {
        Self::from_test_parts_with_timeout(child, ipc, IPC_REQUEST_TIMEOUT)
    }

    #[cfg(test)]
    fn from_test_parts_with_timeout(
        child: Child,
        ipc: Box<dyn IpcConnection>,
        request_timeout: Duration,
    ) -> Self {
        Self {
            child: Some(child),
            ipc: Some(IpcWorker::new(ipc, request_timeout)),
            next_request_id: 1,
        }
    }

    #[cfg(test)]
    fn has_child(&self) -> bool {
        self.child.is_some()
    }
}

enum WorkerCommand {
    Request {
        request_id: u64,
        command: Vec<Value>,
        response: SyncSender<Result<IpcReply, IpcError>>,
    },
    Notify(Vec<Value>),
}

struct IpcWorker {
    commands: Sender<WorkerCommand>,
    request_timeout: Duration,
}

impl IpcWorker {
    fn new(mut ipc: Box<dyn IpcConnection>, request_timeout: Duration) -> Self {
        // Blocking pipe I/O stays off the controller thread; callers wait with a fixed deadline.
        let (commands, receiver) = mpsc::channel::<WorkerCommand>();
        thread::spawn(move || {
            while let Ok(command) = receiver.recv() {
                match command {
                    WorkerCommand::Request {
                        request_id,
                        command,
                        response,
                    } => {
                        let result = ipc.request(request_id, &command);
                        let _ = response.send(result);
                    }
                    WorkerCommand::Notify(command) => {
                        let _ = ipc.notify(&command);
                    }
                }
            }
        });

        Self {
            commands,
            request_timeout,
        }
    }

    fn request(&self, request_id: u64, command: &[Value]) -> Result<IpcReply, IpcError> {
        let (response, receiver) = mpsc::sync_channel(1);
        self.commands
            .send(WorkerCommand::Request {
                request_id,
                command: command.to_vec(),
                response,
            })
            .map_err(|_| IpcError::Closed)?;

        match receiver.recv_timeout(self.request_timeout) {
            Ok(result) => result,
            Err(RecvTimeoutError::Timeout) => Err(IpcError::Timeout),
            Err(RecvTimeoutError::Disconnected) => Err(IpcError::Closed),
        }
    }

    fn notify(&self, command: &[Value]) -> Result<(), IpcError> {
        self.commands
            .send(WorkerCommand::Notify(command.to_vec()))
            .map_err(|_| IpcError::Closed)
    }
}

impl Drop for MpvRuntime {
    fn drop(&mut self) {
        self.shutdown();
    }
}

fn mpv_executable() -> OsString {
    env::var_os("DORIYA_W0_MPV_PATH")
        .filter(|path| !path.is_empty())
        .unwrap_or_else(|| OsString::from("mpv"))
}

fn configure_sidecar_environment(command: &mut Command) {
    apply_sidecar_environment(command, env::vars_os());
}

fn apply_sidecar_environment(
    command: &mut Command,
    variables: impl IntoIterator<Item = (OsString, OsString)>,
) {
    command.env_clear();
    command.envs(
        variables
            .into_iter()
            .filter(|(name, _)| is_sidecar_environment_variable(name)),
    );
}

fn is_sidecar_environment_variable(name: &OsString) -> bool {
    let name = name.to_string_lossy();
    SIDECAR_ENV_ALLOWLIST
        .iter()
        .any(|allowed| name.eq_ignore_ascii_case(allowed))
}

fn mpv_arguments(pipe_path: &Path) -> Vec<OsString> {
    vec![
        OsString::from("--no-config"),
        OsString::from("--idle=yes"),
        OsString::from("--force-window=yes"),
        OsString::from("--keep-open=yes"),
        OsString::from("--no-terminal"),
        OsString::from("--really-quiet"),
        OsString::from("--osc=no"),
        OsString::from(format!(
            "--input-ipc-server={}",
            pipe_path.to_string_lossy()
        )),
        OsString::from("--title=Doriya W0 mpv sidecar"),
    ]
}

fn next_pipe_path() -> Result<PathBuf, PlayerError> {
    let mut random = [0u8; 16];
    getrandom::getrandom(&mut random).map_err(|_| PlayerError::Internal)?;

    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut name = String::with_capacity(PIPE_NAME_PREFIX.len() + random.len() * 2);
    name.push_str(PIPE_NAME_PREFIX);
    for byte in random {
        name.push(HEX[(byte >> 4) as usize] as char);
        name.push(HEX[(byte & 0x0f) as usize] as char);
    }
    Ok(PathBuf::from(name))
}

#[cfg(windows)]
fn verify_ipc_server_pid(ipc: &dyn IpcConnection, expected_pid: u32) -> Result<(), PlayerError> {
    match ipc.server_pid() {
        Ok(server_pid) if server_pid == expected_pid => Ok(()),
        _ => {
            log_code("mpv_ipc_peer_mismatch", PlayerError::IpcPeerMismatch.code());
            Err(PlayerError::IpcPeerMismatch)
        }
    }
}

#[cfg(not(windows))]
fn verify_ipc_server_pid(_ipc: &dyn IpcConnection, _expected_pid: u32) -> Result<(), PlayerError> {
    Ok(())
}

fn numeric_or_null(value: Value) -> Option<f64> {
    value.as_f64().filter(|number| number.is_finite())
}

fn wait_for_exit(child: &mut Child, timeout: Duration) -> Option<ExitStatus> {
    let deadline = Instant::now() + timeout;
    loop {
        match child.try_wait() {
            Ok(Some(status)) => return Some(status),
            Ok(None) if Instant::now() < deadline => thread::sleep(Duration::from_millis(20)),
            _ => return None,
        }
    }
}

fn terminate_child(child: &mut Child) {
    if matches!(child.try_wait(), Ok(None)) {
        let _ = child.kill();
    }
    let _ = wait_for_exit(child, FORCED_EXIT_TIMEOUT);
}

fn log_scheme(event: &'static str, scheme: &str) {
    let scheme = match scheme {
        "https" => "https",
        "http" => "http",
        _ => "other",
    };
    eprintln!("[doriya-w0-mpv] event={event} scheme={scheme}");
}

fn log_count(event: &'static str, value: usize) {
    eprintln!("[doriya-w0-mpv] event={event} count={value}");
}

fn log_pid(event: &'static str, value: u32) {
    eprintln!("[doriya-w0-mpv] event={event} pid={value}");
}

fn log_code(event: &'static str, value: &'static str) {
    eprintln!("[doriya-w0-mpv] event={event} code={value}");
}

#[cfg(windows)]
fn configure_hidden_process(command: &mut Command) {
    use std::os::windows::process::CommandExt;

    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    command.creation_flags(CREATE_NO_WINDOW);
}

#[cfg(not(windows))]
fn configure_hidden_process(_command: &mut Command) {}

#[cfg(test)]
mod tests {
    use std::cell::Cell;
    use std::collections::{HashSet, VecDeque};
    use std::process::Command;

    use super::*;
    use crate::player::media::{validate_media, MediaLoadRequest, MediaPolicy};

    fn reply(data: Value) -> IpcReply {
        IpcReply {
            data: Some(data),
            events: Vec::new(),
        }
    }

    fn reply_with_events(data: Value, events: Vec<MpvEvent>) -> IpcReply {
        IpcReply {
            data: Some(data),
            events,
        }
    }

    fn unavailable_reply_with_events(events: Vec<MpvEvent>) -> IpcReply {
        IpcReply { data: None, events }
    }

    fn test_media() -> ValidatedMedia {
        validate_media(
            MediaLoadRequest {
                url: "https://media.example.invalid/file.mkv".to_owned(),
                headers: Default::default(),
            },
            MediaPolicy::from_environment(),
        )
        .expect("valid test media")
    }

    struct FakeIpc;

    impl IpcConnection for FakeIpc {
        fn request(&mut self, _request_id: u64, _command: &[Value]) -> Result<IpcReply, IpcError> {
            Ok(reply(Value::Null))
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
        }

        fn server_pid(&self) -> Result<u32, IpcError> {
            Ok(std::process::id())
        }
    }

    struct RejectLoadIpc;

    impl IpcConnection for RejectLoadIpc {
        fn request(&mut self, _request_id: u64, command: &[Value]) -> Result<IpcReply, IpcError> {
            if command.first().and_then(Value::as_str) == Some("loadfile") {
                Err(IpcError::Io)
            } else {
                Ok(reply(Value::Null))
            }
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
        }

        fn server_pid(&self) -> Result<u32, IpcError> {
            Ok(std::process::id())
        }
    }

    struct PauseIpc {
        paused: bool,
    }

    impl IpcConnection for PauseIpc {
        fn request(&mut self, _request_id: u64, command: &[Value]) -> Result<IpcReply, IpcError> {
            match (
                command.first().and_then(Value::as_str),
                command.get(1).and_then(Value::as_str),
            ) {
                (Some("get_property"), Some("pause")) => Ok(reply(Value::Bool(self.paused))),
                (Some("set_property"), Some("pause")) => {
                    self.paused = command
                        .get(2)
                        .and_then(Value::as_bool)
                        .ok_or(IpcError::Protocol)?;
                    Ok(reply(Value::Null))
                }
                _ => Ok(reply(Value::Null)),
            }
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
        }

        fn server_pid(&self) -> Result<u32, IpcError> {
            Ok(std::process::id())
        }
    }

    struct RejectControlIpc;

    impl IpcConnection for RejectControlIpc {
        fn request(&mut self, _request_id: u64, command: &[Value]) -> Result<IpcReply, IpcError> {
            if command.first().and_then(Value::as_str) == Some("set_property") {
                Err(IpcError::Io)
            } else {
                Ok(reply(Value::Null))
            }
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
        }

        fn server_pid(&self) -> Result<u32, IpcError> {
            Ok(std::process::id())
        }
    }

    struct FixedPidIpc(u32);

    impl IpcConnection for FixedPidIpc {
        fn request(&mut self, _request_id: u64, _command: &[Value]) -> Result<IpcReply, IpcError> {
            Ok(reply(Value::Null))
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
        }

        fn server_pid(&self) -> Result<u32, IpcError> {
            Ok(self.0)
        }
    }

    struct ScriptedIpc {
        responses: VecDeque<Result<IpcReply, IpcError>>,
    }

    impl ScriptedIpc {
        fn new(responses: impl IntoIterator<Item = Result<IpcReply, IpcError>>) -> Self {
            Self {
                responses: responses.into_iter().collect(),
            }
        }
    }

    impl IpcConnection for ScriptedIpc {
        fn request(&mut self, _request_id: u64, _command: &[Value]) -> Result<IpcReply, IpcError> {
            self.responses
                .pop_front()
                .unwrap_or(Err(IpcError::Protocol))
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
        }

        fn server_pid(&self) -> Result<u32, IpcError> {
            Ok(std::process::id())
        }
    }

    struct NeverRespondingIpc;

    impl IpcConnection for NeverRespondingIpc {
        fn request(&mut self, _request_id: u64, _command: &[Value]) -> Result<IpcReply, IpcError> {
            thread::sleep(Duration::from_secs(30));
            Ok(reply(Value::Null))
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
        }

        fn server_pid(&self) -> Result<u32, IpcError> {
            Ok(std::process::id())
        }
    }

    #[cfg(windows)]
    fn spawn_long_lived_child() -> Child {
        Command::new("cmd.exe")
            .args(["/D", "/C", "ping", "127.0.0.1", "-n", "30", ">NUL"])
            .spawn()
            .expect("spawn long-lived test process")
    }

    #[cfg(windows)]
    fn controller_with_runtime(ipc: Box<dyn IpcConnection>) -> PlayerController {
        controller_with_runtime_timeout(ipc, IPC_REQUEST_TIMEOUT)
    }

    #[cfg(windows)]
    fn controller_with_runtime_timeout(
        ipc: Box<dyn IpcConnection>,
        request_timeout: Duration,
    ) -> PlayerController {
        PlayerController {
            runtime: Some(MpvRuntime::from_test_parts_with_timeout(
                spawn_long_lived_child(),
                ipc,
                request_timeout,
            )),
            state: PlayerState::Ready,
            media_loaded: false,
            last_error_code: None,
            status_metrics: PlaybackMetrics::default(),
            pending_load: None,
        }
    }

    #[cfg(windows)]
    #[test]
    fn reaps_exited_child_and_releases_ipc() {
        let child = Command::new("cmd.exe")
            .args(["/D", "/C", "exit", "7"])
            .spawn()
            .expect("spawn short-lived test process");
        let mut runtime = MpvRuntime::from_test_parts(child, Box::new(FakeIpc));

        let deadline = Instant::now() + Duration::from_secs(5);
        let mut exited = false;
        while Instant::now() < deadline {
            if runtime.try_reap().expect("reap child") {
                exited = true;
                break;
            }
            thread::sleep(Duration::from_millis(20));
        }

        assert!(exited, "test child must exit");
        assert!(!runtime.has_child(), "child handle must be released");
        assert!(runtime.ipc.is_none(), "IPC handle must be released");
    }

    #[cfg(windows)]
    #[test]
    fn shutdown_terminates_a_long_lived_child() {
        let mut runtime = MpvRuntime::from_test_parts(spawn_long_lived_child(), Box::new(FakeIpc));

        let started = Instant::now();
        runtime.shutdown();

        assert!(!runtime.has_child(), "child handle must be cleared");
        assert!(runtime.ipc.is_none(), "IPC handle must be cleared");
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "shutdown must be bounded"
        );
    }

    #[test]
    fn sidecar_arguments_never_receive_media_or_headers() {
        let pipe = PathBuf::from(r"\\.\pipe\doriya-w0-test");
        let args = mpv_arguments(&pipe)
            .into_iter()
            .map(|argument| argument.to_string_lossy().into_owned())
            .collect::<Vec<_>>();
        let joined = args.join(" ");

        assert!(joined.contains("--input-ipc-server"));
        assert!(!joined.contains("http://"));
        assert!(!joined.contains("https://"));
        assert!(!joined.to_ascii_lowercase().contains("referer"));
        assert!(!joined.to_ascii_lowercase().contains("cookie"));
        assert!(!joined.contains("secret"));
    }

    #[test]
    fn startup_failure_enters_error_state() {
        let mut player = PlayerController::new();
        let error = player
            .start_with(|| Err(PlayerError::MpvStartFailed))
            .err()
            .expect("startup must fail");
        let snapshot = player.snapshot();

        assert_eq!(error, PlayerError::MpvStartFailed);
        assert_eq!(snapshot.state, PlayerState::Error);
        assert_eq!(
            snapshot.last_error_code,
            Some(PlayerError::MpvStartFailed.code())
        );
        assert!(!snapshot.media_loaded);
        assert!(snapshot.pid.is_none());
    }

    #[test]
    fn runtime_without_child_does_not_return_stale_playback_state() {
        let mut player = PlayerController::new();
        player.state = PlayerState::Playing;
        player.media_loaded = true;
        player.status_metrics.position_seconds = Some(42.0);
        player.status_metrics.duration_seconds = Some(90.0);

        let snapshot = player.status();

        assert_eq!(snapshot.state, PlayerState::Error);
        assert_eq!(snapshot.last_error_code, Some(PlayerError::Internal.code()));
        assert!(!snapshot.media_loaded);
        assert!(snapshot.position_seconds.is_none());
        assert!(snapshot.duration_seconds.is_none());
    }

    #[cfg(windows)]
    #[test]
    fn start_replaces_a_reaped_runtime_before_spawning_again() {
        let child = Command::new("cmd.exe")
            .args(["/D", "/C", "exit", "7"])
            .spawn()
            .expect("spawn short-lived test process");
        let mut player = PlayerController {
            runtime: Some(MpvRuntime::from_test_parts(child, Box::new(FakeIpc))),
            state: PlayerState::Ready,
            media_loaded: false,
            last_error_code: None,
            status_metrics: PlaybackMetrics::default(),
            pending_load: None,
        };

        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline {
            if player
                .runtime
                .as_mut()
                .expect("runtime")
                .try_reap()
                .expect("reap child")
            {
                break;
            }
            thread::sleep(Duration::from_millis(20));
        }

        let spawned = Cell::new(false);
        let snapshot = player
            .start_with(|| {
                spawned.set(true);
                Ok(MpvRuntime::from_test_parts(
                    spawn_long_lived_child(),
                    Box::new(FakeIpc),
                ))
            })
            .expect("start replacement runtime");

        assert!(spawned.get(), "a reaped runtime must be replaced");
        assert_eq!(snapshot.state, PlayerState::Ready);
        assert!(snapshot.pid.is_some());
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn control_transport_failure_clears_runtime_and_playback_state() {
        let mut player = controller_with_runtime(Box::new(RejectControlIpc));
        player.media_loaded = true;
        player.status_metrics.position_seconds = Some(12.0);

        let error = player
            .control(PlaybackCommand::Pause)
            .err()
            .expect("control transport must fail");
        let snapshot = player.snapshot();

        assert_eq!(error, PlayerError::IpcIo);
        assert_eq!(snapshot.state, PlayerState::Error);
        assert_eq!(snapshot.last_error_code, Some(PlayerError::IpcIo.code()));
        assert!(!snapshot.media_loaded);
        assert!(snapshot.position_seconds.is_none());
        assert!(player.runtime.is_none());
    }

    #[cfg(windows)]
    #[test]
    fn toggle_pause_uses_the_current_mpv_property() {
        let mut player = controller_with_runtime(Box::new(PauseIpc { paused: false }));
        player.media_loaded = true;

        let snapshot = player
            .control(PlaybackCommand::TogglePause)
            .expect("toggle pause");

        assert_eq!(snapshot.state, PlayerState::Paused);
        assert_eq!(snapshot.paused, Some(true));
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn failed_open_does_not_report_loaded_media_or_retain_runtime() {
        let media = validate_media(
            MediaLoadRequest {
                url: "https://media.example.invalid/file.mkv".to_owned(),
                headers: Default::default(),
            },
            MediaPolicy::from_environment(),
        )
        .expect("valid test media");
        let mut player = controller_with_runtime(Box::new(RejectLoadIpc));

        let error = player.open(media).err().expect("load must fail");
        let snapshot = player.snapshot();

        assert_eq!(error, PlayerError::IpcIo);
        assert_eq!(snapshot.state, PlayerState::Error);
        assert_eq!(snapshot.last_error_code, Some(PlayerError::IpcIo.code()));
        assert!(!snapshot.media_loaded);
        assert!(snapshot.position_seconds.is_none());
        assert!(snapshot.duration_seconds.is_none());
        assert!(player.runtime.is_none());
    }

    #[cfg(windows)]
    #[test]
    fn loadfile_success_waits_for_file_loaded_and_status_transitions_to_playing() {
        let ipc = ScriptedIpc::new([
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply_with_events(
                Value::Null,
                vec![MpvEvent::StartFile, MpvEvent::FileLoaded],
            )),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Bool(false))),
            Ok(reply(json!(5.0))),
            Ok(reply(json!(100.0))),
            Ok(reply(json!(80.0))),
        ]);
        let mut player = controller_with_runtime(Box::new(ipc));

        let opening = player
            .open(test_media())
            .expect("load commands must succeed");
        assert!(opening.media_loaded);
        assert_eq!(opening.state, PlayerState::Opening);
        assert_eq!(opening.paused, Some(false));

        let playing = player.status();
        assert_eq!(playing.state, PlayerState::Playing);
        assert_eq!(playing.position_seconds, Some(5.0));
        assert_eq!(playing.duration_seconds, Some(100.0));
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn loadfile_success_without_file_loaded_stays_opening() {
        let ipc = ScriptedIpc::new([
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
        ]);
        let mut player = controller_with_runtime(Box::new(ipc));

        let snapshot = player
            .open(test_media())
            .expect("loadfile must be accepted");

        assert!(!snapshot.media_loaded);
        assert_eq!(snapshot.state, PlayerState::Opening);
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn load_error_event_enters_stable_error_state() {
        let ipc = ScriptedIpc::new([
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply_with_events(
                Value::Null,
                vec![MpvEvent::StartFile, MpvEvent::EndFile(EndFileReason::Error)],
            )),
            Ok(reply(Value::Null)),
        ]);
        let mut player = controller_with_runtime(Box::new(ipc));

        let error = player
            .open(test_media())
            .err()
            .expect("media failure must be reported");
        let snapshot = player.snapshot();

        assert_eq!(error, PlayerError::MediaLoadFailed);
        assert_eq!(snapshot.state, PlayerState::Error);
        assert_eq!(
            snapshot.last_error_code,
            Some(PlayerError::MediaLoadFailed.code())
        );
        assert!(!snapshot.media_loaded);
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn asynchronous_end_file_event_enters_stable_exited_state() {
        let ipc = ScriptedIpc::new([
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply_with_events(
                Value::Null,
                vec![MpvEvent::StartFile, MpvEvent::FileLoaded],
            )),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Bool(false))),
            Ok(reply(json!(5.0))),
            Ok(reply(json!(100.0))),
            Ok(reply(json!(80.0))),
            Ok(reply_with_events(
                Value::Null,
                vec![MpvEvent::EndFile(EndFileReason::Eof)],
            )),
        ]);
        let mut player = controller_with_runtime(Box::new(ipc));
        player
            .open(test_media())
            .expect("loadfile must be accepted");
        assert_eq!(player.status().state, PlayerState::Playing);

        let snapshot = player.status();

        assert_eq!(snapshot.state, PlayerState::Exited);
        assert_eq!(snapshot.last_error_code, Some(PlayerError::Exited.code()));
        assert!(!snapshot.media_loaded);
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn property_unavailable_response_still_applies_buffered_events() {
        let ipc = ScriptedIpc::new([
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply_with_events(
                Value::Null,
                vec![MpvEvent::StartFile, MpvEvent::FileLoaded],
            )),
            Ok(reply(Value::Null)),
            Ok(unavailable_reply_with_events(vec![MpvEvent::EndFile(
                EndFileReason::Eof,
            )])),
        ]);
        let mut player = controller_with_runtime(Box::new(ipc));
        player
            .open(test_media())
            .expect("loadfile must be accepted");

        let snapshot = player.status();

        assert_eq!(snapshot.state, PlayerState::Exited);
        assert_eq!(snapshot.last_error_code, Some(PlayerError::Exited.code()));
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn stale_events_before_the_new_start_file_do_not_poison_reopen() {
        let ipc = ScriptedIpc::new([
            // First open.
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply_with_events(
                Value::Null,
                vec![MpvEvent::StartFile, MpvEvent::FileLoaded],
            )),
            Ok(reply(Value::Null)),
            // Second open: events from the old generation arrive before start-file.
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply_with_events(
                Value::Null,
                vec![
                    MpvEvent::FileLoaded,
                    MpvEvent::EndFile(EndFileReason::Error),
                ],
            )),
            Ok(reply(Value::Null)),
            // New generation starts and loads.
            Ok(reply_with_events(
                Value::Null,
                vec![MpvEvent::StartFile, MpvEvent::FileLoaded],
            )),
            Ok(reply(json!(8.0))),
            Ok(reply(json!(120.0))),
            Ok(reply(json!(70.0))),
        ]);
        let mut player = controller_with_runtime(Box::new(ipc));
        player.open(test_media()).expect("first open");

        let reopened = player.open(test_media()).expect("second open");
        assert!(!reopened.media_loaded);
        assert_eq!(reopened.state, PlayerState::Opening);
        assert_ne!(
            reopened.last_error_code,
            Some(PlayerError::MediaLoadFailed.code())
        );

        let playing = player.status();
        assert_eq!(playing.state, PlayerState::Playing);
        assert!(playing.media_loaded);
        assert_eq!(playing.last_error_code, None);
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn unresponsive_ipc_times_out_and_releases_runtime_for_status_and_stop() {
        let mut player = controller_with_runtime_timeout(
            Box::new(NeverRespondingIpc),
            Duration::from_millis(80),
        );
        let started = Instant::now();

        let snapshot = player.status();

        assert_eq!(snapshot.state, PlayerState::Error);
        assert_eq!(
            snapshot.last_error_code,
            Some(PlayerError::IpcTimeout.code())
        );
        assert!(player.runtime.is_none());
        assert!(
            started.elapsed() < Duration::from_secs(3),
            "timed-out request must not hold the controller indefinitely"
        );

        let after_timeout = Instant::now();
        assert_eq!(player.status().state, PlayerState::Error);
        assert_eq!(player.stop().state, PlayerState::Stopped);
        assert!(
            after_timeout.elapsed() < Duration::from_millis(250),
            "later status and stop must not wait on the stuck worker"
        );
    }

    #[cfg(windows)]
    #[test]
    fn shutdown_is_bounded_when_the_ipc_worker_is_stuck() {
        let mut runtime = MpvRuntime::from_test_parts_with_timeout(
            spawn_long_lived_child(),
            Box::new(NeverRespondingIpc),
            Duration::from_millis(80),
        );
        let request = runtime.optional_command(&json!(["get_property", "mpv-version"]));
        assert!(matches!(request, Err(PlayerError::IpcTimeout)));

        let started = Instant::now();
        runtime.shutdown();

        assert!(!runtime.has_child());
        assert!(runtime.ipc.is_none());
        assert!(
            started.elapsed() < Duration::from_secs(4),
            "shutdown must not wait indefinitely for a blocked IPC worker"
        );
    }

    #[test]
    fn pipe_names_use_random_hex_suffixes_without_basic_collisions() {
        let prefix = PIPE_NAME_PREFIX;
        let paths = (0..64)
            .map(|_| {
                let path = next_pipe_path().expect("random pipe name");
                let rendered = path.to_string_lossy().into_owned();
                let suffix = rendered
                    .strip_prefix(prefix)
                    .expect("pipe name must use the fixed prefix");
                assert_eq!(suffix.len(), 32);
                assert!(suffix
                    .bytes()
                    .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte)));
                rendered
            })
            .collect::<HashSet<_>>();

        assert_eq!(paths.len(), 64, "random pipe names must not collide");
    }

    #[cfg(windows)]
    #[test]
    fn ipc_peer_pid_must_match_the_launched_child() {
        assert_eq!(verify_ipc_server_pid(&FixedPidIpc(42), 42), Ok(()));
        assert_eq!(
            verify_ipc_server_pid(&FixedPidIpc(41), 42),
            Err(PlayerError::IpcPeerMismatch)
        );
    }

    #[cfg(windows)]
    #[test]
    fn sidecar_environment_drops_media_credentials_and_loopback_override() {
        const SECRET: &str = "doriya-env-secret-do-not-inherit";
        let mut variables = env::vars_os().collect::<Vec<_>>();
        variables.extend([
            (
                OsString::from("DORIYA_W0_MEDIA_URL"),
                OsString::from(format!("https://example.invalid/?token={SECRET}")),
            ),
            (
                OsString::from("DORIYA_W0_MEDIA_REFERER"),
                OsString::from(format!("https://example.invalid/{SECRET}")),
            ),
            (
                OsString::from("DORIYA_W0_ALLOW_LOOPBACK_HTTP"),
                OsString::from("1"),
            ),
            (
                OsString::from("HTTP_PROXY"),
                OsString::from("http://127.0.0.1:7890"),
            ),
        ]);

        let mut command =
            Command::new(env::var_os("COMSPEC").unwrap_or_else(|| OsString::from("cmd.exe")));
        command.args(["/D", "/C", "set"]).stdout(Stdio::piped());
        apply_sidecar_environment(&mut command, variables);
        let output = command.output().expect("read filtered child environment");
        let environment = String::from_utf8_lossy(&output.stdout);

        assert!(output.status.success());
        assert!(!environment.contains(SECRET));
        assert!(!environment.to_ascii_lowercase().contains("doriya_w0_"));
        assert!(!environment.to_ascii_lowercase().contains("http_proxy="));
    }

    #[cfg(windows)]
    #[test]
    #[ignore = "requires an installed mpv; no media is loaded"]
    fn installed_mpv_accepts_json_ipc_and_shuts_down() {
        let mut runtime = MpvRuntime::spawn().expect("start installed mpv");
        let version = runtime
            .command(&json!(["get_property", "mpv-version"]))
            .expect("read mpv version through JSON IPC");
        assert!(
            version
                .data
                .as_ref()
                .and_then(Value::as_str)
                .is_some_and(|value| value.starts_with("mpv")),
            "mpv must answer the version property"
        );

        runtime.shutdown();
        assert!(!runtime.has_child(), "mpv child must be released");
        assert!(runtime.ipc.is_none(), "IPC pipe must be released");
    }
}
