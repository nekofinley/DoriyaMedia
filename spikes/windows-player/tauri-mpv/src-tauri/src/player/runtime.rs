use std::env;
use std::ffi::OsString;
#[cfg(windows)]
use std::fs::{self, File, OpenOptions};
#[cfg(windows)]
use std::io::Read;
#[cfg(windows)]
use std::os::windows::fs::OpenOptionsExt;
#[cfg(windows)]
use std::path::{Path, PathBuf};
#[cfg(not(windows))]
use std::process::Child as PlatformChild;
use std::process::ExitStatus;
#[cfg(test)]
use std::process::{Child, Command};
use std::sync::mpsc::{self, RecvTimeoutError, Sender, SyncSender};
use std::thread;
use std::time::{Duration, Instant};

use serde::Serialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
#[cfg(windows)]
use windows_sys::Win32::Storage::FileSystem::FILE_SHARE_READ;

#[cfg(windows)]
use super::authenticode::{verify_embedded_signature, SignatureError};
use super::ipc::{EndFileReason, IpcConnection, IpcError, IpcReply, JsonPipe, MpvEvent};
use super::media::ValidatedMedia;
use super::relay::RelayProxy;
#[cfg(windows)]
use super::windows_security::{
    current_user_session_id, current_user_sid, verify_process_identity, JobObject, NamedPipeServer,
    PipeError, SidecarChild as PlatformChild,
};

// Keep every request bounded without turning a transient scheduler stall into a
// permanent player failure on a busy desktop.
const IPC_REQUEST_TIMEOUT: Duration = Duration::from_secs(5);
const IPC_OPERATION_TIMEOUT: Duration = Duration::from_secs(5);
// Process launch and the first IPC acknowledgement are slower than ordinary
// control requests on cold disks. Keep them bounded but separate.
const MPV_STARTUP_TIMEOUT: Duration = Duration::from_secs(15);
const MEDIA_OPEN_TIMEOUT: Duration = Duration::from_secs(30);
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

fn capped_startup_deadline(request_deadline: Instant) -> Instant {
    request_deadline.min(Instant::now() + MPV_STARTUP_TIMEOUT)
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PlayerError {
    MpvExecutableMissing,
    MpvIdentityMissing,
    MpvIdentityInvalid,
    MpvIdentityMismatch,
    MpvPublisherInvalid,
    MpvSignatureMissing,
    MpvSignatureInvalid,
    MpvPublisherMismatch,
    MpvStartFailed,
    MediaOpenTimeout,
    IpcTimeout,
    IpcIo,
    IpcProtocol,
    IpcRejected,
    MediaLoadFailed,
    MediaAudioLoadFailed,
    MediaDualOriginUnsupported,
    EgressModeUnsupported,
    Exited,
    InvalidCommand,
    Internal,
}

impl PlayerError {
    pub const fn code(self) -> &'static str {
        match self {
            Self::MpvExecutableMissing => "mpv_executable_missing",
            Self::MpvIdentityMissing => "mpv_identity_missing",
            Self::MpvIdentityInvalid => "mpv_identity_invalid",
            Self::MpvIdentityMismatch => "mpv_identity_mismatch",
            Self::MpvPublisherInvalid => "mpv_publisher_invalid",
            Self::MpvSignatureMissing => "mpv_signature_missing",
            Self::MpvSignatureInvalid => "mpv_signature_invalid",
            Self::MpvPublisherMismatch => "mpv_publisher_mismatch",
            Self::MpvStartFailed => "mpv_start_failed",
            Self::MediaOpenTimeout => "mpv_media_open_timeout",
            Self::IpcTimeout => "mpv_ipc_timeout",
            Self::IpcIo => "mpv_ipc_io",
            Self::IpcProtocol => "mpv_ipc_protocol",
            Self::IpcRejected => "mpv_ipc_rejected",
            Self::MediaLoadFailed => "mpv_media_load_failed",
            Self::MediaAudioLoadFailed => "mpv_media_audio_load_failed",
            Self::MediaDualOriginUnsupported => "mpv_media_dual_origin_unsupported",
            Self::EgressModeUnsupported => "mpv_egress_mode_unsupported",
            Self::Exited => "mpv_exited",
            Self::InvalidCommand => "invalid_command",
            Self::Internal => "internal_error",
        }
    }

    pub const fn message(self) -> &'static str {
        match self {
            Self::MpvExecutableMissing => "The mpv executable could not be found.",
            Self::MpvIdentityMissing => "The mpv executable SHA-256 identity is not configured.",
            Self::MpvIdentityInvalid => {
                "The configured mpv executable SHA-256 identity is invalid."
            }
            Self::MpvIdentityMismatch => {
                "The mpv executable does not match the configured SHA-256 identity."
            }
            Self::MpvPublisherInvalid => {
                "The configured mpv publisher SHA-256 identity is invalid."
            }
            Self::MpvSignatureMissing => "The mpv executable is not Authenticode-signed.",
            Self::MpvSignatureInvalid => {
                "The mpv executable's Authenticode signature is invalid or untrusted."
            }
            Self::MpvPublisherMismatch => {
                "The mpv executable signer does not match the configured publisher identity."
            }
            Self::MpvStartFailed => "The mpv sidecar could not be started.",
            Self::MediaOpenTimeout => "Timed out while waiting for media to load.",
            Self::IpcTimeout => "Timed out while communicating with the mpv IPC pipe.",
            Self::IpcIo => "The mpv IPC connection failed.",
            Self::IpcProtocol => "mpv returned an invalid IPC response.",
            Self::IpcRejected => "mpv rejected the command.",
            Self::MediaLoadFailed => "mpv could not load the requested media.",
            Self::MediaAudioLoadFailed => "mpv could not attach the requested audio stream.",
            Self::MediaDualOriginUnsupported => {
                "The selected egress mode cannot attach audio from a different origin."
            }
            Self::EgressModeUnsupported => {
                "The selected egress mode cannot open this media source type."
            }
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
}

pub struct PlayerController {
    runtime: Option<MpvRuntime>,
    state: PlayerState,
    media_loaded: bool,
    last_error_code: Option<&'static str>,
    terminal_error: Option<PlayerError>,
    status_metrics: PlaybackMetrics,
    pending_load: Option<PendingLoad>,
    video_target: Option<isize>,
    #[cfg(test)]
    media_open_test_delay: Option<Duration>,
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
    deadline: Instant,
    audio_state: AudioAttachState,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
enum AudioAttachState {
    #[default]
    NotRequested,
    Pending,
    Attached,
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
            terminal_error: None,
            status_metrics: PlaybackMetrics::default(),
            pending_load: None,
            video_target: None,
            #[cfg(test)]
            media_open_test_delay: None,
        }
    }

    pub fn start(&mut self) -> Result<PlayerSnapshot, PlayerError> {
        let video_target = self.video_target.ok_or(PlayerError::InvalidCommand)?;
        self.start_with(|| MpvRuntime::spawn_with_video_target(Some(video_target)))
    }

    #[cfg(test)]
    fn start_standalone(&mut self) -> Result<PlayerSnapshot, PlayerError> {
        self.start_with(|| MpvRuntime::spawn_with_video_target(None))
    }

    pub fn set_video_target(&mut self, video_target: isize) -> Result<(), PlayerError> {
        if !valid_video_target(video_target) {
            return Err(PlayerError::InvalidCommand);
        }
        if self.video_target == Some(video_target) {
            return Ok(());
        }
        if self.runtime.is_some() {
            self.stop();
        }
        self.video_target = Some(video_target);
        Ok(())
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
        if let Err(error) =
            self.ensure_runtime_until(Instant::now() + MPV_STARTUP_TIMEOUT + IPC_OPERATION_TIMEOUT)
        {
            self.fail_and_cleanup(error);
            return Err(error);
        }

        self.open_runtime_ready_until(media, Instant::now() + MEDIA_OPEN_TIMEOUT)
    }

    #[cfg(test)]
    fn open_until(
        &mut self,
        media: ValidatedMedia,
        deadline: Instant,
    ) -> Result<PlayerSnapshot, PlayerError> {
        if let Err(error) = self.ensure_runtime_until(deadline) {
            self.fail_and_cleanup(error);
            return Err(error);
        }

        self.open_runtime_ready_until(media, deadline)
    }

    fn open_runtime_ready_until(
        &mut self,
        media: ValidatedMedia,
        deadline: Instant,
    ) -> Result<PlayerSnapshot, PlayerError> {
        if let Err(error) = self.configure_relay_for_media(&media, deadline) {
            self.fail_and_cleanup(error);
            return Err(error);
        }
        // Flush events already queued for the previous load before starting a new generation.
        if let Err(error) = self.media_open_command_ignoring_events_until(
            &json!(["get_property", "mpv-version"]),
            deadline,
        ) {
            self.fail_and_cleanup(error);
            return Err(error);
        }

        self.begin_load(deadline);
        self.state = PlayerState::Opening;
        self.last_error_code = None;
        log_scheme("media_open_requested", media.url_scheme());

        let headers = media.header_fields();
        log_count("header_allowlist_count", headers.len());
        let result = (|| {
            // Any end-file from the previous item belongs to the old generation.
            self.media_open_command_ignoring_events_until(&json!(["stop"]), deadline)?;
            if media.audio().is_some() {
                // Never let the primary clock advance while the external track is
                // being attached. Unpause only after both streams are selected.
                self.media_open_command_ignoring_events_until(
                    &json!(["set_property", "pause", true]),
                    deadline,
                )?;
            }
            self.media_open_command_ignoring_events_until(
                &json!(["set_property", "http-header-fields", headers]),
                deadline,
            )?;
            self.media_open_command_until(&json!(["loadfile", media.url()]), deadline)?;
            if let Some(audio) = media.audio() {
                self.wait_for_primary_track_until(deadline)?;
                #[cfg(test)]
                if let Some(delay) = self.media_open_test_delay {
                    thread::sleep(delay);
                }
                self.begin_audio_attach();
                log_scheme("audio_open_requested", audio.url_scheme());
                self.media_open_command_until(
                    &json!(["audio-add", audio.url(), "select"]),
                    deadline,
                )
                .map_err(|error| match error {
                    PlayerError::IpcRejected => PlayerError::MediaAudioLoadFailed,
                    error => error,
                })?;
                self.wait_for_external_audio_track_until(deadline)?;
            }
            self.media_open_command_until(&json!(["set_property", "pause", false]), deadline)?;
            Ok::<(), PlayerError>(())
        })();

        if let Err(error) = result {
            self.fail_and_cleanup(error);
            return Err(error);
        }

        if let Some(error) = self.terminal_event_error() {
            self.fail_and_cleanup(error);
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

        let deadline = Instant::now() + self.command_deadline_budget();
        self.ensure_runtime_until(deadline)?;

        let result = (|| {
            let (next_state, next_paused, next_volume) = match command {
                PlaybackCommand::Play => {
                    self.runtime_command_until(&json!(["set_property", "pause", false]), deadline)?;
                    (Some(PlayerState::Playing), Some(false), None)
                }
                PlaybackCommand::Pause => {
                    self.runtime_command_until(&json!(["set_property", "pause", true]), deadline)?;
                    (Some(PlayerState::Paused), Some(true), None)
                }
                PlaybackCommand::TogglePause => {
                    let paused = !self
                        .runtime_command_until(&json!(["get_property", "pause"]), deadline)?
                        .data
                        .and_then(|value| value.as_bool())
                        .ok_or(PlayerError::IpcProtocol)?;
                    self.runtime_command_until(
                        &json!(["set_property", "pause", paused]),
                        deadline,
                    )?;
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
                    self.runtime_command_until(&json!(["seek", seconds, "relative"]), deadline)?;
                    (None, None, None)
                }
                PlaybackCommand::SetVolume(volume) => {
                    self.runtime_command_until(
                        &json!(["set_property", "volume", volume]),
                        deadline,
                    )?;
                    (None, None, Some(volume))
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
            self.fail_and_cleanup(error);
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
        if let Some(error) = self.terminal_event_error() {
            self.fail_and_cleanup(error);
            return self.snapshot();
        }

        let result = self.read_status_metrics(Instant::now() + self.operation_timeout());
        if let Err(error) = result {
            self.fail_and_cleanup(error);
            return self.snapshot();
        }

        if let Some(error) = self.terminal_event_error() {
            self.fail_and_cleanup(error);
            return self.snapshot();
        }
        if self.opening_timed_out() {
            self.fail_and_cleanup(PlayerError::MediaOpenTimeout);
            return self.snapshot();
        }

        if self.media_loaded {
            self.state = if self.status_metrics.paused == Some(true) {
                PlayerState::Paused
            } else {
                PlayerState::Playing
            };
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

    fn command_deadline_budget(&self) -> Duration {
        if self.runtime.is_none() {
            MPV_STARTUP_TIMEOUT + IPC_OPERATION_TIMEOUT
        } else {
            self.operation_timeout()
        }
    }

    fn ensure_runtime_until(&mut self, deadline: Instant) -> Result<(), PlayerError> {
        if self.runtime.is_none() {
            let startup_deadline = capped_startup_deadline(deadline);
            let video_target = self.video_target.ok_or(PlayerError::InvalidCommand)?;
            self.start_with(|| MpvRuntime::spawn_until(startup_deadline, Some(video_target)))?;
        } else {
            self.check_runtime_exit()?;
        }
        Ok(())
    }

    fn configure_relay_for_media(
        &mut self,
        media: &ValidatedMedia,
        deadline: Instant,
    ) -> Result<(), PlayerError> {
        let proxy_url = {
            let runtime = self.runtime.as_ref().ok_or(PlayerError::Internal)?;
            match runtime.egress_mode {
                EgressMode::RelayRequired => {
                    let relay = runtime.relay.as_ref().ok_or(PlayerError::Internal)?;
                    let origin = relay_origin_for_playback(media)?;
                    Some(
                        relay
                            .begin_playback(origin)
                            .map_err(|_| PlayerError::Internal)?,
                    )
                }
                EgressMode::DirectLoopbackFixture
                    if media.url_scheme() == "http"
                        && media
                            .audio()
                            .is_some_and(|audio| audio.url_scheme() != "http") =>
                {
                    return Err(PlayerError::MediaDualOriginUnsupported);
                }
                EgressMode::DirectLoopbackFixture if media.url_scheme() == "http" => None,
                EgressMode::DirectLoopbackFixture => {
                    return Err(PlayerError::EgressModeUnsupported)
                }
                #[cfg(test)]
                EgressMode::TestFake => None,
            }
        };
        if let Some(proxy_url) = proxy_url {
            self.media_open_command_ignoring_events_until(
                &json!(["set_property", "http-proxy", proxy_url]),
                deadline,
            )?;
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

    fn read_status_metrics(&mut self, deadline: Instant) -> Result<(), PlayerError> {
        let paused = self
            .runtime_optional_command_until(&json!(["get_property", "pause"]), deadline)?
            .data
            .and_then(|value| value.as_bool());
        if self.terminal_event_error().is_some() {
            return Ok(());
        }
        let position = self
            .runtime_optional_command_until(&json!(["get_property", "time-pos"]), deadline)?
            .data
            .and_then(numeric_or_null);
        if self.terminal_event_error().is_some() {
            return Ok(());
        }
        let duration = self
            .runtime_optional_command_until(&json!(["get_property", "duration"]), deadline)?
            .data
            .and_then(numeric_or_null);
        if self.terminal_event_error().is_some() {
            return Ok(());
        }
        let volume = self
            .runtime_optional_command_until(&json!(["get_property", "volume"]), deadline)?
            .data
            .and_then(numeric_or_null);
        self.status_metrics.paused = paused;
        self.status_metrics.position_seconds = position;
        self.status_metrics.duration_seconds = duration;
        self.status_metrics.volume = volume;
        Ok(())
    }

    fn wait_for_primary_track_until(&mut self, deadline: Instant) -> Result<(), PlayerError> {
        loop {
            if let Some(error) = self.terminal_event_error() {
                return Err(error);
            }
            if Instant::now() >= deadline {
                return Err(PlayerError::MediaOpenTimeout);
            }
            let track_count = self
                .media_open_optional_command_until(
                    &json!(["get_property", "track-list/count"]),
                    deadline,
                )?
                .data
                .and_then(|value| value.as_u64())
                .unwrap_or(0);
            if let Some(error) = self.terminal_event_error() {
                return Err(error);
            }
            if track_count > 0 {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(PlayerError::MediaOpenTimeout);
            }
            thread::sleep(
                Duration::from_millis(20).min(deadline.saturating_duration_since(Instant::now())),
            );
        }
    }

    fn wait_for_external_audio_track_until(
        &mut self,
        deadline: Instant,
    ) -> Result<(), PlayerError> {
        loop {
            if let Some(error) = self.terminal_event_error() {
                return Err(error);
            }
            if Instant::now() >= deadline {
                return Err(PlayerError::MediaOpenTimeout);
            }
            let tracks = self
                .media_open_optional_command_until(
                    &json!(["get_property", "track-list"]),
                    deadline,
                )?
                .data;
            if let Some(error) = self.terminal_event_error() {
                return Err(error);
            }
            if tracks
                .as_ref()
                .and_then(Value::as_array)
                .is_some_and(|tracks| {
                    tracks.iter().any(|track| {
                        track.get("type").and_then(Value::as_str) == Some("audio")
                            && track.get("selected").and_then(Value::as_bool) == Some(true)
                            && track.get("external").and_then(Value::as_bool) == Some(true)
                    })
                })
            {
                self.mark_audio_attached();
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(PlayerError::MediaOpenTimeout);
            }
            thread::sleep(
                Duration::from_millis(20).min(deadline.saturating_duration_since(Instant::now())),
            );
        }
    }

    fn begin_load(&mut self, deadline: Instant) {
        self.pending_load = Some(PendingLoad {
            saw_start: false,
            deadline,
            audio_state: AudioAttachState::NotRequested,
        });
        self.terminal_error = None;
        self.media_loaded = false;
        self.status_metrics = PlaybackMetrics::default();
    }

    fn begin_audio_attach(&mut self) {
        if let Some(pending) = self.pending_load.as_mut() {
            pending.audio_state = AudioAttachState::Pending;
        }
    }

    fn mark_audio_attached(&mut self) {
        if let Some(pending) = self.pending_load.as_mut() {
            pending.audio_state = AudioAttachState::Attached;
        }
    }

    fn opening_timed_out(&self) -> bool {
        self.pending_load
            .as_ref()
            .is_some_and(|pending| !self.media_loaded && Instant::now() >= pending.deadline)
    }

    fn operation_timeout(&self) -> Duration {
        self.runtime
            .as_ref()
            .map(|runtime| runtime.operation_timeout)
            .unwrap_or(IPC_OPERATION_TIMEOUT)
    }

    fn runtime_command_until(
        &mut self,
        command: &Value,
        deadline: Instant,
    ) -> Result<CommandReply, PlayerError> {
        let reply = self.runtime_mut()?.command_until(command, deadline)?;
        self.apply_events(reply.events);
        Ok(CommandReply {
            data: reply.data,
            events: Vec::new(),
        })
    }

    fn media_open_command_until(
        &mut self,
        command: &Value,
        deadline: Instant,
    ) -> Result<CommandReply, PlayerError> {
        if Instant::now() >= deadline {
            return Err(PlayerError::MediaOpenTimeout);
        }
        match self.runtime_command_until(command, deadline) {
            Err(PlayerError::IpcTimeout) if Instant::now() >= deadline => {
                Err(PlayerError::MediaOpenTimeout)
            }
            result => result,
        }
    }

    fn media_open_optional_command_until(
        &mut self,
        command: &Value,
        deadline: Instant,
    ) -> Result<CommandReply, PlayerError> {
        if Instant::now() >= deadline {
            return Err(PlayerError::MediaOpenTimeout);
        }
        match self.runtime_optional_command_until(command, deadline) {
            Err(PlayerError::IpcTimeout) if Instant::now() >= deadline => {
                Err(PlayerError::MediaOpenTimeout)
            }
            result => result,
        }
    }

    fn runtime_optional_command_until(
        &mut self,
        command: &Value,
        deadline: Instant,
    ) -> Result<CommandReply, PlayerError> {
        let reply = self
            .runtime_mut()?
            .optional_command_until(command, deadline)?;
        self.apply_events(reply.events);
        Ok(CommandReply {
            data: reply.data,
            events: Vec::new(),
        })
    }

    fn media_open_command_ignoring_events_until(
        &mut self,
        command: &Value,
        deadline: Instant,
    ) -> Result<CommandReply, PlayerError> {
        if Instant::now() >= deadline {
            return Err(PlayerError::MediaOpenTimeout);
        }
        match self.runtime_mut()?.command_until(command, deadline) {
            Err(PlayerError::IpcTimeout) if Instant::now() >= deadline => {
                Err(PlayerError::MediaOpenTimeout)
            }
            result => result,
        }
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
                    self.terminal_error = None;
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
                        let error = self
                            .pending_load
                            .take()
                            .filter(|pending| pending.audio_state == AudioAttachState::Pending)
                            .map_or(PlayerError::MediaLoadFailed, |_| {
                                PlayerError::MediaAudioLoadFailed
                            });
                        self.media_loaded = false;
                        self.status_metrics = PlaybackMetrics::default();
                        self.state = PlayerState::Error;
                        self.last_error_code = Some(error.code());
                        self.terminal_error = Some(error);
                    }
                    EndFileReason::Eof | EndFileReason::Stop | EndFileReason::Quit => {
                        self.pending_load = None;
                        self.media_loaded = false;
                        self.status_metrics = PlaybackMetrics::default();
                        self.state = PlayerState::Exited;
                        self.last_error_code = Some(PlayerError::Exited.code());
                        self.terminal_error = Some(PlayerError::Exited);
                    }
                }
            }
        }
    }

    fn terminal_event_error(&self) -> Option<PlayerError> {
        self.terminal_error.or(match self.state {
            PlayerState::Error => Some(PlayerError::MediaLoadFailed),
            PlayerState::Exited => Some(PlayerError::Exited),
            _ => None,
        })
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
        self.terminal_error = Some(error);
    }

    fn clear_media_state(&mut self) {
        self.media_loaded = false;
        self.status_metrics = PlaybackMetrics::default();
        self.pending_load = None;
        self.terminal_error = None;
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

fn relay_origin_for_playback(
    media: &ValidatedMedia,
) -> Result<super::relay::RelayOrigin, PlayerError> {
    let origin = media.relay_origin().ok_or(PlayerError::Internal)?;
    if media
        .audio()
        .and_then(super::media::ValidatedStream::relay_origin)
        .is_some_and(|audio_origin| audio_origin != origin)
    {
        return Err(PlayerError::MediaDualOriginUnsupported);
    }
    Ok(origin)
}

impl Drop for PlayerController {
    fn drop(&mut self) {
        self.shutdown();
    }
}

struct MpvRuntime {
    child: Option<PlatformChild>,
    ipc: Option<IpcWorker>,
    relay: Option<RelayProxy>,
    egress_mode: EgressMode,
    next_request_id: u64,
    operation_timeout: Duration,
    #[cfg(windows)]
    job: Option<JobObject>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum EgressMode {
    RelayRequired,
    DirectLoopbackFixture,
    #[cfg(test)]
    TestFake,
}

impl MpvRuntime {
    #[cfg(all(windows, test))]
    fn spawn() -> Result<Self, PlayerError> {
        Self::spawn_with_video_target(None)
    }

    #[cfg(windows)]
    fn spawn_with_video_target(video_target: Option<isize>) -> Result<Self, PlayerError> {
        Self::spawn_until(Instant::now() + MPV_STARTUP_TIMEOUT, video_target)
    }

    #[cfg(windows)]
    fn spawn_until(
        startup_deadline: Instant,
        video_target: Option<isize>,
    ) -> Result<Self, PlayerError> {
        let executable = verified_mpv_executable()?;
        let direct_loopback_fixture = loopback_http_enabled();
        let egress_mode = if direct_loopback_fixture {
            EgressMode::DirectLoopbackFixture
        } else {
            EgressMode::RelayRequired
        };
        let relay = if direct_loopback_fixture {
            None
        } else {
            Some(RelayProxy::start().map_err(|_| PlayerError::MpvStartFailed)?)
        };
        let pipe_path = next_pipe_path()?;
        let user_sid = current_user_sid().map_err(|error| {
            log_pipe_error("mpv_user_sid_error", error);
            PlayerError::MpvStartFailed
        })?;
        let user_session_id = current_user_session_id().map_err(|error| {
            log_pipe_error("mpv_user_session_error", error);
            PlayerError::MpvStartFailed
        })?;
        let pipe_server = NamedPipeServer::create(&pipe_path, &user_sid).map_err(|error| {
            log_pipe_error("mpv_pipe_create_error", error);
            PlayerError::MpvStartFailed
        })?;
        let client = pipe_server
            .connect_client_until(startup_deadline)
            .map_err(|error| {
                log_pipe_error("mpv_pipe_client_error", error);
                PlayerError::MpvStartFailed
            })?;
        let args = mpv_arguments(video_target);
        let job = JobObject::new_kill_on_close().map_err(|error| {
            log_pipe_error("mpv_job_create_error", error);
            PlayerError::MpvStartFailed
        })?;

        let environment = sidecar_environment();
        let mut child = PlatformChild::spawn_restricted(
            executable.path.as_os_str(),
            &args,
            &environment,
            client,
            &job,
        )
        .map_err(|error| {
            if error.kind() == std::io::ErrorKind::NotFound {
                PlayerError::MpvExecutableMissing
            } else {
                PlayerError::MpvStartFailed
            }
        })?;
        // The verification handle denies write/delete sharing until the image
        // has been handed to CreateProcessW, closing the hash/spawn swap gap.
        drop(executable);

        if let Err(error) = job.ensure_assigned(&child) {
            log_pipe_error("mpv_job_assign_error", error);
            terminate_child(&mut child);
            return Err(PlayerError::MpvStartFailed);
        }
        if let Err(error) = verify_process_identity(&child, &user_sid, user_session_id) {
            log_pipe_error("mpv_process_identity_error", error);
            terminate_child(&mut child);
            return Err(PlayerError::MpvStartFailed);
        }

        if child
            .try_wait()
            .map_err(|_| PlayerError::MpvStartFailed)?
            .is_some()
        {
            return Err(PlayerError::MpvStartFailed);
        }
        if let Some(relay) = relay.as_ref() {
            let creation_time = child.creation_time().map_err(|error| {
                log_io_error("mpv_process_time_error", error);
                PlayerError::MpvStartFailed
            })?;
            if relay.bind_process(child.id(), creation_time).is_err() {
                terminate_child(&mut child);
                return Err(PlayerError::MpvStartFailed);
            }
        }

        let transport = pipe_server.into_transport();
        let ipc = JsonPipe::from_parts(Box::new(transport.clone()), Box::new(transport));

        let mut runtime = Self {
            child: Some(child),
            ipc: Some(IpcWorker::new(Box::new(ipc), IPC_REQUEST_TIMEOUT)),
            relay,
            egress_mode,
            next_request_id: 1,
            operation_timeout: IPC_OPERATION_TIMEOUT,
            job: Some(job),
        };

        if let Err(error) =
            runtime.command_until(&json!(["get_property", "mpv-version"]), startup_deadline)
        {
            runtime.abort();
            return Err(error);
        }
        if let Some(pid) = runtime.pid() {
            log_pid("mpv_started", pid);
        }
        Ok(runtime)
    }

    #[cfg(not(windows))]
    fn spawn_with_video_target(_video_target: Option<isize>) -> Result<Self, PlayerError> {
        Self::spawn_until(Instant::now() + MPV_STARTUP_TIMEOUT, None)
    }

    #[cfg(not(windows))]
    fn spawn_until(
        _startup_deadline: Instant,
        _video_target: Option<isize>,
    ) -> Result<Self, PlayerError> {
        Err(PlayerError::MpvStartFailed)
    }

    #[cfg(test)]
    fn command(&mut self, command: &Value) -> Result<CommandReply, PlayerError> {
        self.command_until(command, Instant::now() + self.operation_timeout)
    }

    fn command_until(
        &mut self,
        command: &Value,
        deadline: Instant,
    ) -> Result<CommandReply, PlayerError> {
        let reply = self.optional_command_until(command, deadline)?;
        if reply.data.is_some() {
            Ok(reply)
        } else {
            Err(PlayerError::IpcRejected)
        }
    }

    #[cfg(test)]
    fn optional_command(&mut self, command: &Value) -> Result<CommandReply, PlayerError> {
        self.optional_command_until(command, Instant::now() + self.operation_timeout)
    }

    fn optional_command_until(
        &mut self,
        command: &Value,
        deadline: Instant,
    ) -> Result<CommandReply, PlayerError> {
        let command = command.as_array().ok_or(PlayerError::InvalidCommand)?;
        self.try_reap()?;
        let request_id = self.next_request_id;
        self.next_request_id = self.next_request_id.saturating_add(1);
        let reply = self
            .ipc
            .as_mut()
            .ok_or(PlayerError::Exited)?
            .request_until(request_id, command, deadline);
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
                self.relay = None;
                #[cfg(windows)]
                {
                    self.job = None;
                }
                Ok(true)
            }
            None => Ok(false),
        }
    }

    fn pid(&self) -> Option<u32> {
        self.child.as_ref().map(|child| child.id())
    }

    fn shutdown(&mut self) {
        if let Some(relay) = self.relay.as_ref() {
            relay.disarm();
        }
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
        self.relay = None;
        #[cfg(windows)]
        {
            self.job = None;
        }
    }

    fn abort(&mut self) {
        if let Some(relay) = self.relay.as_ref() {
            relay.disarm();
        }
        if let Some(child) = self.child.as_mut() {
            terminate_child(child);
        }
        self.child = None;
        self.ipc = None;
        self.relay = None;
        #[cfg(windows)]
        {
            self.job = None;
        }
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
            child: Some(child.into()),
            ipc: Some(IpcWorker::new(ipc, request_timeout)),
            relay: None,
            egress_mode: EgressMode::TestFake,
            next_request_id: 1,
            operation_timeout: request_timeout,
            #[cfg(windows)]
            job: None,
        }
    }

    #[cfg(test)]
    fn has_child(&self) -> bool {
        self.child.is_some()
    }

    #[cfg(all(test, windows))]
    fn job_is_configured(&self) -> bool {
        self.job.is_some()
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

    fn request_until(
        &self,
        request_id: u64,
        command: &[Value],
        deadline: Instant,
    ) -> Result<IpcReply, IpcError> {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Err(IpcError::Timeout);
        }
        let (response, receiver) = mpsc::sync_channel(1);
        self.commands
            .send(WorkerCommand::Request {
                request_id,
                command: command.to_vec(),
                response,
            })
            .map_err(|_| IpcError::Closed)?;

        match receiver.recv_timeout(remaining.min(self.request_timeout)) {
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

#[cfg(windows)]
struct VerifiedMpvExecutable {
    path: PathBuf,
    _verification_handle: File,
}

#[cfg(windows)]
fn verified_mpv_executable() -> Result<VerifiedMpvExecutable, PlayerError> {
    let configured_path = env::var_os("DORIYA_W0_MPV_PATH")
        .filter(|path| !path.is_empty())
        .ok_or(PlayerError::MpvExecutableMissing)?;
    let configured_path = PathBuf::from(configured_path);
    if !configured_path.is_absolute() {
        return Err(PlayerError::MpvExecutableMissing);
    }

    let expected = env::var("DORIYA_W0_MPV_SHA256").map_err(|_| PlayerError::MpvIdentityMissing)?;
    let expected = parse_sha256(&expected).ok_or(PlayerError::MpvIdentityInvalid)?;
    let expected_signer = env::var_os("DORIYA_W0_MPV_SIGNER_SHA256")
        .map(|value| {
            value
                .to_str()
                .and_then(parse_sha256)
                .ok_or(PlayerError::MpvPublisherInvalid)
        })
        .transpose()?;
    verify_mpv_executable(&configured_path, expected, expected_signer)
}

#[cfg(windows)]
fn verify_mpv_executable(
    path: &Path,
    expected: [u8; 32],
    expected_signer: Option<[u8; 32]>,
) -> Result<VerifiedMpvExecutable, PlayerError> {
    let path = fs::canonicalize(path).map_err(|_| PlayerError::MpvExecutableMissing)?;
    if !path.is_file() {
        return Err(PlayerError::MpvExecutableMissing);
    }
    let mut verification_handle = OpenOptions::new()
        .read(true)
        .share_mode(FILE_SHARE_READ)
        .open(&path)
        .map_err(|_| PlayerError::MpvExecutableMissing)?;
    let actual = sha256_file(&mut verification_handle)?;
    if actual != expected {
        return Err(PlayerError::MpvIdentityMismatch);
    }
    if let Some(expected_signer) = expected_signer {
        let signer =
            verify_embedded_signature(&path, &verification_handle).map_err(map_signature_error)?;
        if signer != expected_signer {
            return Err(PlayerError::MpvPublisherMismatch);
        }
    }

    Ok(VerifiedMpvExecutable {
        path,
        _verification_handle: verification_handle,
    })
}

#[cfg(windows)]
fn map_signature_error(error: SignatureError) -> PlayerError {
    match error {
        SignatureError::Missing | SignatureError::NoSigner => PlayerError::MpvSignatureMissing,
        SignatureError::Invalid | SignatureError::Untrusted | SignatureError::Thumbprint => {
            PlayerError::MpvSignatureInvalid
        }
    }
}

#[cfg(windows)]
fn sha256_file(file: &mut File) -> Result<[u8; 32], PlayerError> {
    let mut digest = Sha256::new();
    let mut buffer = [0u8; 64 * 1024];
    loop {
        let read = file
            .read(&mut buffer)
            .map_err(|_| PlayerError::MpvStartFailed)?;
        if read == 0 {
            break;
        }
        digest.update(&buffer[..read]);
    }
    Ok(digest.finalize().into())
}

#[cfg(windows)]
fn parse_sha256(value: &str) -> Option<[u8; 32]> {
    if value.len() != 64 {
        return None;
    }
    let mut bytes = [0u8; 32];
    for (index, byte) in bytes.iter_mut().enumerate() {
        let start = index * 2;
        *byte = u8::from_str_radix(&value[start..start + 2], 16).ok()?;
    }
    Some(bytes)
}

fn sidecar_environment() -> Vec<(OsString, OsString)> {
    env::vars_os()
        .filter(|(name, _)| is_sidecar_environment_variable(name))
        .collect()
}

#[cfg(test)]
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

fn loopback_http_enabled() -> bool {
    cfg!(debug_assertions)
        && env::var("DORIYA_W0_ALLOW_LOOPBACK_HTTP").is_ok_and(|value| value == "1")
}

fn mpv_arguments(video_target: Option<isize>) -> Vec<OsString> {
    let protocols = if loopback_http_enabled() {
        "http,https,tcp,tls,crypto,httpproxy"
    } else {
        "https,tcp,tls,crypto,httpproxy"
    };
    let mut arguments = vec![
        OsString::from("--no-config"),
        OsString::from("--no-load-scripts"),
        OsString::from("--no-autoload-files"),
        OsString::from("--idle=yes"),
        OsString::from("--force-window=yes"),
        OsString::from("--keep-open=yes"),
        OsString::from("--no-terminal"),
        OsString::from("--really-quiet"),
        OsString::from("--osc=no"),
        OsString::from("--sub-auto=no"),
        OsString::from("--audio-file-auto=no"),
        OsString::from("--cover-art-auto=no"),
        OsString::from("--load-unsafe-playlists=no"),
        OsString::from("--tls-verify=yes"),
        OsString::from("--ytdl=no"),
        OsString::from("--network-timeout=15"),
        OsString::from(format!(
            "--stream-lavf-o-append=protocol_whitelist={protocols}"
        )),
        OsString::from(format!(
            "--demuxer-lavf-o-append=protocol_whitelist={protocols}"
        )),
        OsString::from("--stream-lavf-o-append=max_redirects=3"),
        OsString::from("--demuxer-lavf-o-append=max_redirects=3"),
        OsString::from("--input-ipc-client=fd://0"),
        OsString::from("--title=Doriya W0 mpv sidecar"),
    ];
    if let Some(video_target) = video_target.filter(|target| valid_video_target(*target)) {
        arguments.push(OsString::from(format!("--wid={video_target}")));
    }
    arguments
}

fn valid_video_target(video_target: isize) -> bool {
    video_target > 0 && video_target as u64 <= u64::from(u32::MAX)
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
fn log_pipe_error(event: &'static str, error: PipeError) {
    eprintln!("[doriya-w0-mpv] event={event} reason={}", error.code());
}

#[cfg(windows)]
fn log_io_error(event: &'static str, error: std::io::Error) {
    eprintln!(
        "[doriya-w0-mpv] event={event} reason=os_{}",
        error.raw_os_error().unwrap_or_default()
    );
}

fn numeric_or_null(value: Value) -> Option<f64> {
    value.as_f64().filter(|number| number.is_finite())
}

fn wait_for_exit(child: &mut PlatformChild, timeout: Duration) -> Option<ExitStatus> {
    let deadline = Instant::now() + timeout;
    loop {
        match child.try_wait() {
            Ok(Some(status)) => return Some(status),
            Ok(None) if Instant::now() < deadline => thread::sleep(Duration::from_millis(20)),
            _ => return None,
        }
    }
}

fn terminate_child(child: &mut PlatformChild) {
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

#[cfg(test)]
mod tests {
    use std::cell::Cell;
    use std::collections::{BTreeMap, HashSet, VecDeque};
    #[cfg(windows)]
    use std::io::{BufRead, BufReader};
    #[cfg(windows)]
    use std::os::windows::process::CommandExt;
    use std::process::{Command, Stdio};
    #[cfg(windows)]
    use std::sync::{Arc, Mutex, MutexGuard};

    use super::*;
    use crate::player::media::{validate_media, MediaLoadRequest, MediaPolicy, MediaStreamRequest};

    #[cfg(windows)]
    #[derive(serde::Deserialize)]
    struct LoopbackFixtureInfo {
        url: String,
        referer: String,
        user_agent: String,
    }

    #[cfg(windows)]
    struct LoopbackFixtureServer {
        child: Child,
        info: LoopbackFixtureInfo,
    }

    #[cfg(windows)]
    static OPT_IN_ENV_LOCK: Mutex<()> = Mutex::new(());

    #[cfg(windows)]
    struct OptInEnvironmentGuard {
        _lock: MutexGuard<'static, ()>,
        previous: Option<OsString>,
    }

    #[cfg(windows)]
    fn set_opt_in_environment(loopback_fixture: bool) -> OptInEnvironmentGuard {
        let lock = OPT_IN_ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let previous = env::var_os("DORIYA_W0_ALLOW_LOOPBACK_HTTP");
        if loopback_fixture {
            env::set_var("DORIYA_W0_ALLOW_LOOPBACK_HTTP", "1");
        } else {
            env::remove_var("DORIYA_W0_ALLOW_LOOPBACK_HTTP");
        }
        OptInEnvironmentGuard {
            _lock: lock,
            previous,
        }
    }

    #[cfg(windows)]
    impl Drop for OptInEnvironmentGuard {
        fn drop(&mut self) {
            match self.previous.take() {
                Some(value) => env::set_var("DORIYA_W0_ALLOW_LOOPBACK_HTTP", value),
                None => env::remove_var("DORIYA_W0_ALLOW_LOOPBACK_HTTP"),
            }
        }
    }

    #[cfg(windows)]
    impl Drop for LoopbackFixtureServer {
        fn drop(&mut self) {
            let _ = self.child.kill();
            let _ = self.child.wait();
        }
    }

    #[cfg(windows)]
    fn start_loopback_fixture(media_name: &str) -> LoopbackFixtureServer {
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;

        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
        let script = root.join("fixtures/header-range-server.py");
        let media = root.join("fixtures/synthetic").join(media_name);
        assert!(
            script.is_file(),
            "missing fixture server at {}",
            script.display()
        );
        assert!(
            media.is_file(),
            "missing synthetic media at {}",
            media.display()
        );

        let mut child = Command::new("python")
            .args(["-u"])
            .arg(&script)
            .arg("--file")
            .arg(&media)
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .creation_flags(CREATE_NO_WINDOW)
            .spawn()
            .expect("start loopback fixture server");
        let stdout = child.stdout.take().expect("fixture server stdout");
        let (sender, receiver) = mpsc::sync_channel(1);
        thread::spawn(move || {
            let mut line = String::new();
            let _ = BufReader::new(stdout).read_line(&mut line);
            let _ = sender.send(line);
        });
        let line = match receiver.recv_timeout(Duration::from_secs(10)) {
            Ok(line) if !line.trim().is_empty() => line,
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                panic!("fixture server did not publish its URL within 10 seconds");
            }
        };
        let info = match serde_json::from_str::<LoopbackFixtureInfo>(line.trim()) {
            Ok(info) => info,
            Err(error) => {
                let _ = child.kill();
                let _ = child.wait();
                panic!("invalid fixture server metadata: {error}");
            }
        };
        LoopbackFixtureServer { child, info }
    }

    #[cfg(windows)]
    fn wait_for_playing(player: &mut PlayerController) -> PlayerSnapshot {
        let deadline = Instant::now() + Duration::from_secs(20);
        loop {
            let snapshot = player.status();
            if snapshot.state == PlayerState::Playing && snapshot.media_loaded {
                return snapshot;
            }
            if matches!(
                snapshot.state,
                PlayerState::Error | PlayerState::Exited | PlayerState::Stopped
            ) {
                panic!(
                    "player entered {:?} with {:?}",
                    snapshot.state, snapshot.last_error_code
                );
            }
            assert!(
                Instant::now() < deadline,
                "player did not reach Playing within 20 seconds; last state={:?}",
                snapshot.state
            );
            thread::sleep(Duration::from_millis(50));
        }
    }

    #[cfg(windows)]
    fn player_property(player: &mut PlayerController, name: &str) -> Value {
        player
            .runtime_optional_command_until(
                &json!(["get_property", name]),
                Instant::now() + Duration::from_secs(5),
            )
            .unwrap_or_else(|error| panic!("read mpv property {name}: {error:?}"))
            .data
            .unwrap_or_else(|| panic!("mpv property {name} returned no data"))
    }

    #[cfg(windows)]
    fn set_player_property(player: &mut PlayerController, name: &str, value: Value) {
        player
            .runtime_command_until(
                &json!(["set_property", name, value]),
                Instant::now() + Duration::from_secs(5),
            )
            .unwrap_or_else(|error| panic!("set mpv property {name}: {error:?}"));
    }

    #[cfg(windows)]
    fn player_time_position(player: &mut PlayerController) -> f64 {
        player_property(player, "time-pos")
            .as_f64()
            .expect("mpv time-pos")
    }

    #[cfg(windows)]
    fn wait_for_seek_position(player: &mut PlayerController, target: f64) -> f64 {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            let seeking = player_property(player, "seeking")
                .as_bool()
                .unwrap_or(false);
            let position = player_time_position(player);
            if !seeking && (position - target).abs() <= 0.35 {
                return position;
            }
            assert!(
                Instant::now() < deadline,
                "seek did not settle near {target:.3}s; seeking={seeking}, position={position:.3}s"
            );
            thread::sleep(Duration::from_millis(20));
        }
    }

    #[cfg(windows)]
    fn verify_twenty_player_seeks(player: &mut PlayerController, label: &str) {
        set_player_property(player, "hr-seek", Value::String("yes".to_owned()));
        let mut observed = HashSet::new();
        for index in 0..20 {
            let target = 0.2 + ((index * 7) % 25) as f64 * 0.1;
            let current = player_time_position(player);
            let snapshot = player
                .control(PlaybackCommand::Seek(target - current))
                .unwrap_or_else(|error| panic!("{label}: seek {index} failed: {error:?}"));
            assert_eq!(snapshot.state, PlayerState::Paused);
            assert!(snapshot.media_loaded);
            assert_eq!(snapshot.last_error_code, None);
            let actual = wait_for_seek_position(player, target);
            observed.insert((actual * 10.0).round() as i64);
        }
        assert!(
            observed.len() >= 12,
            "{label}: seeks collapsed onto too few positions: {observed:?}"
        );
    }

    #[cfg(windows)]
    fn verify_playback_advances(player: &mut PlayerController, label: &str) {
        let current = player_time_position(player);
        let target = 0.2_f64.max(current.min(1.5));
        if (current - target).abs() > 0.05 {
            player
                .control(PlaybackCommand::Seek(target - current))
                .unwrap_or_else(|error| panic!("{label}: reset seek failed: {error:?}"));
            wait_for_seek_position(player, target);
        }
        player
            .control(PlaybackCommand::Play)
            .unwrap_or_else(|error| panic!("{label}: resume failed: {error:?}"));
        let before = player_time_position(player);
        let audio_before = player_property(player, "audio-pts")
            .as_f64()
            .expect("audio pts before playback");
        thread::sleep(Duration::from_millis(350));
        let after = player_time_position(player);
        let audio_after = player_property(player, "audio-pts")
            .as_f64()
            .expect("audio pts after playback");
        assert!(
            after >= before + 0.1,
            "{label}: playback position did not advance: {before:.3} -> {after:.3}"
        );
        assert!(
            audio_after >= audio_before + 0.1,
            "{label}: audio PTS did not advance: {audio_before:.3} -> {audio_after:.3}"
        );
    }

    #[cfg(windows)]
    fn verify_dual_track_selection(player: &mut PlayerController, label: &str) {
        let tracks = player_property(player, "track-list");
        let tracks = tracks.as_array().expect("mpv track list");
        let video_tracks = tracks
            .iter()
            .filter(|track| {
                track.get("type").and_then(Value::as_str) == Some("video")
                    && track.get("selected").and_then(Value::as_bool) == Some(true)
            })
            .count();
        let audio_tracks = tracks
            .iter()
            .filter(|track| {
                track.get("type").and_then(Value::as_str) == Some("audio")
                    && track.get("selected").and_then(Value::as_bool) == Some(true)
            })
            .count();
        let external_audio = tracks.iter().any(|track| {
            track.get("type").and_then(Value::as_str) == Some("audio")
                && track.get("selected").and_then(Value::as_bool) == Some(true)
                && track.get("external").and_then(Value::as_bool) == Some(true)
        });

        assert_eq!(
            video_tracks, 1,
            "{label}: expected one selected video track"
        );
        assert_eq!(
            audio_tracks, 1,
            "{label}: expected one selected audio track"
        );
        assert!(external_audio, "{label}: audio track must be external");
    }

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
                audio: None,
            },
            MediaPolicy::from_environment(),
        )
        .expect("valid test media")
    }

    fn test_dual_media() -> ValidatedMedia {
        validate_media(
            MediaLoadRequest {
                url: "https://media.example.invalid/video.mp4".to_owned(),
                headers: Default::default(),
                audio: Some(MediaStreamRequest {
                    url: "https://media.example.invalid/audio.mp4".to_owned(),
                    headers: Default::default(),
                }),
            },
            MediaPolicy::from_environment(),
        )
        .expect("valid dual-stream test media")
    }

    fn test_cross_origin_dual_media() -> ValidatedMedia {
        validate_media(
            MediaLoadRequest {
                url: "https://video.example.invalid/video.mp4".to_owned(),
                headers: Default::default(),
                audio: Some(MediaStreamRequest {
                    url: "https://audio.example.invalid/audio.mp4".to_owned(),
                    headers: Default::default(),
                }),
            },
            MediaPolicy::from_environment(),
        )
        .expect("valid cross-origin dual-stream test media")
    }

    fn selected_dual_track_list() -> Value {
        json!([
            {"type": "video", "selected": true, "external": false},
            {"type": "audio", "selected": true, "external": true}
        ])
    }

    struct FakeIpc;

    impl IpcConnection for FakeIpc {
        fn request(&mut self, _request_id: u64, _command: &[Value]) -> Result<IpcReply, IpcError> {
            Ok(reply(Value::Null))
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
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
    }

    struct RecordingIpc {
        commands: Arc<Mutex<Vec<Vec<Value>>>>,
    }

    impl IpcConnection for RecordingIpc {
        fn request(&mut self, _request_id: u64, command: &[Value]) -> Result<IpcReply, IpcError> {
            self.commands
                .lock()
                .expect("record commands")
                .push(command.to_vec());
            if command.first().and_then(Value::as_str) == Some("get_property")
                && command.get(1).and_then(Value::as_str) == Some("track-list/count")
            {
                Ok(reply(json!(1)))
            } else if command.first().and_then(Value::as_str) == Some("get_property")
                && command.get(1).and_then(Value::as_str) == Some("track-list")
            {
                Ok(reply(selected_dual_track_list()))
            } else {
                Ok(reply(Value::Null))
            }
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
        }
    }

    struct DelayedPrimaryTrackIpc {
        started: Instant,
        delay: Duration,
    }

    impl IpcConnection for DelayedPrimaryTrackIpc {
        fn request(&mut self, _request_id: u64, command: &[Value]) -> Result<IpcReply, IpcError> {
            match (
                command.first().and_then(Value::as_str),
                command.get(1).and_then(Value::as_str),
            ) {
                (Some("get_property"), Some("track-list/count")) => {
                    if self.started.elapsed() >= self.delay {
                        Ok(reply(json!(1)))
                    } else {
                        Ok(reply(json!(0)))
                    }
                }
                (Some("get_property"), Some("track-list")) => Ok(reply(selected_dual_track_list())),
                _ => Ok(reply(Value::Null)),
            }
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
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
    }

    struct SlowRespondingIpc(Duration);

    impl IpcConnection for SlowRespondingIpc {
        fn request(&mut self, _request_id: u64, _command: &[Value]) -> Result<IpcReply, IpcError> {
            thread::sleep(self.0);
            Ok(reply(Value::Null))
        }

        fn notify(&mut self, _command: &[Value]) -> Result<(), IpcError> {
            Ok(())
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
            terminal_error: None,
            status_metrics: PlaybackMetrics::default(),
            pending_load: None,
            video_target: None,
            #[cfg(test)]
            media_open_test_delay: None,
        }
    }

    #[cfg(windows)]
    fn controller_with_test_egress(
        ipc: Box<dyn IpcConnection>,
        egress_mode: EgressMode,
        relay: Option<RelayProxy>,
    ) -> PlayerController {
        PlayerController {
            runtime: Some(MpvRuntime {
                child: Some(spawn_long_lived_child().into()),
                ipc: Some(IpcWorker::new(ipc, IPC_REQUEST_TIMEOUT)),
                relay,
                egress_mode,
                next_request_id: 1,
                operation_timeout: IPC_OPERATION_TIMEOUT,
                job: None,
            }),
            state: PlayerState::Ready,
            media_loaded: false,
            last_error_code: None,
            terminal_error: None,
            status_metrics: PlaybackMetrics::default(),
            pending_load: None,
            video_target: None,
            media_open_test_delay: None,
        }
    }

    #[cfg(windows)]
    fn controller_with_relay_runtime(ipc: Box<dyn IpcConnection>) -> PlayerController {
        let relay = RelayProxy::start().expect("start test relay");
        relay
            .bind_process(
                std::process::id(),
                super::super::windows_security::current_process_creation_time()
                    .expect("current process creation time"),
            )
            .expect("bind relay to the test process");
        controller_with_test_egress(ipc, EgressMode::RelayRequired, Some(relay))
    }

    #[cfg(windows)]
    fn controller_with_loopback_runtime(ipc: Box<dyn IpcConnection>) -> PlayerController {
        controller_with_test_egress(ipc, EgressMode::DirectLoopbackFixture, None)
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
        let args = mpv_arguments(None)
            .into_iter()
            .map(|argument| argument.to_string_lossy().into_owned())
            .collect::<Vec<_>>();
        let joined = args.join(" ");

        assert!(args.contains(&"--input-ipc-client=fd://0".to_owned()));
        assert!(args.contains(&"--tls-verify=yes".to_owned()));
        assert!(args.contains(&"--ytdl=no".to_owned()));
        assert!(args.contains(&"--network-timeout=15".to_owned()));
        assert!(args.contains(&"--no-load-scripts".to_owned()));
        assert!(args.contains(&"--no-autoload-files".to_owned()));
        assert!(args.contains(&"--sub-auto=no".to_owned()));
        assert!(args.contains(&"--audio-file-auto=no".to_owned()));
        assert!(args.contains(&"--cover-art-auto=no".to_owned()));
        assert!(args.contains(&"--load-unsafe-playlists=no".to_owned()));
        assert!(args.iter().any(|argument| argument.starts_with(
            "--stream-lavf-o-append=protocol_whitelist=https,tcp,tls,crypto,httpproxy"
        ) || argument.starts_with(
            "--stream-lavf-o-append=protocol_whitelist=http,https,tcp,tls,crypto,httpproxy"
        )));
        assert!(args.iter().any(|argument| argument.starts_with(
            "--demuxer-lavf-o-append=protocol_whitelist=https,tcp,tls,crypto,httpproxy"
        ) || argument.starts_with(
            "--demuxer-lavf-o-append=protocol_whitelist=http,https,tcp,tls,crypto,httpproxy"
        )));
        assert!(args.contains(&"--stream-lavf-o-append=max_redirects=3".to_owned()));
        assert!(args.contains(&"--demuxer-lavf-o-append=max_redirects=3".to_owned()));
        assert!(!joined.contains("--input-ipc-server"));
        assert!(!joined.contains(r"\\.\pipe"));
        assert!(!joined.contains("--http-proxy"));
        assert!(!joined.contains("--wid="));
        assert!(!joined.contains("http://"));
        assert!(!joined.contains("https://"));
        assert!(!joined.to_ascii_lowercase().contains("referer"));
        assert!(!joined.to_ascii_lowercase().contains("cookie"));
        assert!(!joined.contains("secret"));
    }

    #[test]
    fn embedded_arguments_target_only_the_owned_surface_hwnd() {
        let args = mpv_arguments(Some(0x1234))
            .into_iter()
            .map(|argument| argument.to_string_lossy().into_owned())
            .collect::<Vec<_>>();
        let joined = args.join(" ");

        assert!(args.contains(&"--wid=4660".to_owned()));
        assert!(args.contains(&"--force-window=yes".to_owned()));
        assert_eq!(
            args.iter()
                .filter(|argument| argument.starts_with("--wid="))
                .count(),
            1
        );
        assert!(!joined.contains("http://"));
        assert!(!joined.contains("https://"));
    }

    #[test]
    fn invalid_video_targets_are_never_forwarded_to_mpv() {
        for target in [Some(0), Some(-1)] {
            let args = mpv_arguments(target)
                .into_iter()
                .map(|argument| argument.to_string_lossy().into_owned())
                .collect::<Vec<_>>();
            assert!(!args.iter().any(|argument| argument.starts_with("--wid=")));
        }
    }

    #[test]
    fn controller_rejects_invalid_video_targets() {
        let mut player = PlayerController::new();
        for target in [0, -1] {
            assert_eq!(
                player.set_video_target(target),
                Err(PlayerError::InvalidCommand)
            );
        }
    }

    #[test]
    fn controller_requires_a_video_target_before_spawning() {
        let mut player = PlayerController::new();

        assert!(matches!(player.start(), Err(PlayerError::InvalidCommand)));
        assert_eq!(
            player
                .control(PlaybackCommand::TogglePause)
                .err()
                .expect("targetless control must fail"),
            PlayerError::InvalidCommand
        );
        assert!(player.runtime.is_none());
        assert_eq!(player.snapshot().state, PlayerState::Stopped);
    }

    #[cfg(windows)]
    #[test]
    fn sha256_identity_parser_rejects_malformed_values() {
        assert!(
            parse_sha256("2CF24DBA5FB0A30E26E83B2AC5B9E29E1B161E5C1FA7425E73043362938B9824")
                .is_some()
        );
        assert!(parse_sha256("not-a-sha256").is_none());
        assert!(
            parse_sha256("2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b982z")
                .is_none()
        );
    }

    #[cfg(windows)]
    #[test]
    fn sha256_file_matches_a_known_digest() {
        let path =
            PathBuf::from("target").join(format!("doriya-w0-sha256-{}.tmp", std::process::id()));
        fs::create_dir_all("target").expect("create hash fixture directory");
        fs::write(&path, b"hello").expect("write hash fixture");
        let mut file = File::open(&path).expect("open hash fixture");
        let actual = sha256_file(&mut file).expect("hash fixture");
        let _ = fs::remove_file(path);
        assert_eq!(
            actual,
            parse_sha256("2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
                .expect("known digest")
        );
    }

    #[cfg(windows)]
    #[test]
    fn executable_identity_mismatch_fails_before_spawn() {
        let path =
            PathBuf::from("target").join(format!("doriya-w0-identity-{}.tmp", std::process::id()));
        fs::create_dir_all("target").expect("create identity fixture directory");
        fs::write(&path, b"not mpv").expect("write identity fixture");
        assert!(matches!(
            verify_mpv_executable(&path, [0u8; 32], None),
            Err(PlayerError::MpvIdentityMismatch)
        ));
        let _ = fs::remove_file(path);
    }

    #[cfg(windows)]
    #[test]
    fn configured_publisher_pin_rejects_an_unsigned_hash_matching_file() {
        let path = PathBuf::from("target")
            .join(format!("doriya-w0-unsigned-pin-{}.tmp", std::process::id()));
        fs::create_dir_all("target").expect("create unsigned pin fixture directory");
        fs::write(&path, b"unsigned but hash matching").expect("write unsigned pin fixture");
        let expected = {
            let mut file = File::open(&path).expect("open unsigned pin fixture");
            sha256_file(&mut file).expect("hash unsigned pin fixture")
        };

        assert!(matches!(
            verify_mpv_executable(&path, expected, Some([0u8; 32])),
            Err(PlayerError::MpvSignatureMissing)
        ));
        let _ = fs::remove_file(path);
    }

    #[cfg(windows)]
    #[test]
    fn configured_publisher_pin_accepts_the_matching_signer() {
        let path = Path::new(r"C:\Windows\explorer.exe");
        if !path.is_file() {
            return;
        }
        let expected = {
            let mut file = File::open(path).expect("open signed system binary for hashing");
            sha256_file(&mut file).expect("hash signed system binary")
        };
        let signer = {
            let file = File::open(path).expect("open signed system binary for signature");
            verify_embedded_signature(path, &file).expect("read signed system binary signer")
        };

        assert!(verify_mpv_executable(path, expected, Some(signer)).is_ok());
    }

    #[cfg(windows)]
    #[test]
    fn configured_publisher_pin_rejects_a_different_signer() {
        let path = Path::new(r"C:\Windows\explorer.exe");
        if !path.is_file() {
            return;
        }
        let expected = {
            let mut file = File::open(path).expect("open signed system binary for hashing");
            sha256_file(&mut file).expect("hash signed system binary")
        };

        assert!(matches!(
            verify_mpv_executable(path, expected, Some([0u8; 32])),
            Err(PlayerError::MpvPublisherMismatch)
        ));
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
    fn cold_command_budget_covers_startup_and_one_operation() {
        let player = PlayerController::new();
        assert_eq!(
            player.command_deadline_budget(),
            MPV_STARTUP_TIMEOUT + IPC_OPERATION_TIMEOUT
        );
    }

    #[test]
    fn cold_operation_caps_startup_before_the_remaining_operation_budget() {
        let request_deadline = Instant::now() + MPV_STARTUP_TIMEOUT + IPC_OPERATION_TIMEOUT;
        let startup_deadline = capped_startup_deadline(request_deadline);

        assert!(
            request_deadline.saturating_duration_since(startup_deadline)
                >= IPC_OPERATION_TIMEOUT - Duration::from_millis(10)
        );
    }

    #[cfg(windows)]
    #[test]
    fn warm_command_budget_uses_only_the_operation_timeout() {
        let player = controller_with_runtime(Box::new(FakeIpc));
        assert_eq!(player.command_deadline_budget(), IPC_OPERATION_TIMEOUT);
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
            terminal_error: None,
            status_metrics: PlaybackMetrics::default(),
            pending_load: None,
            video_target: None,
            #[cfg(test)]
            media_open_test_delay: None,
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
                audio: None,
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
    fn dual_stream_open_pauses_before_load_and_unpauses_after_both_tracks() {
        let commands = Arc::new(Mutex::new(Vec::new()));
        let mut player = controller_with_runtime(Box::new(RecordingIpc {
            commands: Arc::clone(&commands),
        }));

        player.open(test_dual_media()).expect("dual-stream open");
        let commands = commands.lock().expect("read recorded commands");
        let position = |name: &str, argument: &str| {
            commands
                .iter()
                .position(|command| {
                    command.first().and_then(Value::as_str) == Some(name)
                        && command.get(1).and_then(Value::as_str) == Some(argument)
                })
                .unwrap_or_else(|| panic!("missing command {name} {argument}"))
        };
        let pause_before_load = commands
            .iter()
            .position(|command| {
                command.first().and_then(Value::as_str) == Some("set_property")
                    && command.get(1).and_then(Value::as_str) == Some("pause")
                    && command.get(2).and_then(Value::as_bool) == Some(true)
            })
            .expect("missing pause-before-load command");
        let loadfile = position("loadfile", "https://media.example.invalid/video.mp4");
        let audio_add = position("audio-add", "https://media.example.invalid/audio.mp4");
        let track_selection = position("get_property", "track-list");
        let unpause = commands
            .iter()
            .position(|command| {
                command.first().and_then(Value::as_str) == Some("set_property")
                    && command.get(1).and_then(Value::as_str) == Some("pause")
                    && command.get(2).and_then(Value::as_bool) == Some(false)
            })
            .expect("missing unpause command");

        assert!(
            pause_before_load < loadfile,
            "the primary clock must be paused before loadfile"
        );
        assert!(loadfile < audio_add, "audio must be added after the video");
        assert!(
            audio_add < track_selection && track_selection < unpause,
            "the selected external audio track must be observed before playback"
        );
        assert_eq!(
            commands[audio_add].get(2).and_then(Value::as_str),
            Some("select")
        );
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn primary_failure_before_audio_attach_remains_a_media_load_error() {
        let ipc = ScriptedIpc::new([
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply_with_events(
                Value::Null,
                vec![MpvEvent::StartFile, MpvEvent::EndFile(EndFileReason::Error)],
            )),
        ]);
        let mut player = controller_with_runtime(Box::new(ipc));

        let error = player
            .open(test_dual_media())
            .err()
            .expect("primary failure must surface during open");

        assert_eq!(error, PlayerError::MediaLoadFailed);
        assert_eq!(
            player.snapshot().last_error_code,
            Some(PlayerError::MediaLoadFailed.code())
        );
        assert!(player.runtime.is_none());
    }

    #[cfg(windows)]
    #[test]
    fn asynchronous_external_audio_failure_while_attaching_keeps_audio_error() {
        let ipc = ScriptedIpc::new([
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply_with_events(Value::Null, vec![MpvEvent::StartFile])),
            Ok(reply(json!(1))),
            Ok(reply_with_events(
                Value::Null,
                vec![MpvEvent::EndFile(EndFileReason::Error)],
            )),
        ]);
        let mut player = controller_with_runtime(Box::new(ipc));

        let error = player
            .open(test_dual_media())
            .err()
            .expect("external audio failure must surface during open");

        assert_eq!(error, PlayerError::MediaAudioLoadFailed);
        assert_eq!(
            player.snapshot().last_error_code,
            Some(PlayerError::MediaAudioLoadFailed.code())
        );
        assert!(player.runtime.is_none());
    }

    #[cfg(windows)]
    #[test]
    fn error_after_external_audio_attachment_uses_generic_media_error() {
        let ipc = ScriptedIpc::new([
            // open(): flush, stop, pause, headers, loadfile, primary track,
            // audio-add, selected external track, unpause.
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply(Value::Null)),
            Ok(reply_with_events(Value::Null, vec![MpvEvent::StartFile])),
            Ok(reply(json!(1))),
            Ok(reply(Value::Null)),
            Ok(reply(selected_dual_track_list())),
            Ok(reply(Value::Null)),
            // After attachment mpv events do not identify which stream failed.
            Ok(reply_with_events(
                Value::Null,
                vec![MpvEvent::EndFile(EndFileReason::Error)],
            )),
        ]);
        let mut player = controller_with_runtime(Box::new(ipc));

        player
            .open(test_dual_media())
            .expect("dual-stream commands must be accepted");
        let snapshot = player.status();

        assert_eq!(snapshot.state, PlayerState::Error);
        assert_eq!(
            snapshot.last_error_code,
            Some(PlayerError::MediaLoadFailed.code())
        );
        assert!(
            player.runtime.is_none(),
            "terminal media error must release the runtime"
        );
    }

    #[cfg(windows)]
    #[test]
    fn dual_stream_open_waits_more_than_five_seconds_for_the_primary_track() {
        let delay = Duration::from_millis(5_300);
        let mut player = controller_with_runtime(Box::new(DelayedPrimaryTrackIpc {
            started: Instant::now(),
            delay,
        }));

        let started = Instant::now();
        player
            .open(test_dual_media())
            .expect("the 30-second media-open deadline must cover a slow primary track");
        let elapsed = started.elapsed();

        assert!(
            elapsed >= Duration::from_millis(5_050),
            "test must exercise a primary-track delay over the old 5-second budget: {elapsed:?}"
        );
        assert!(
            elapsed < Duration::from_secs(8),
            "slow primary load must stay bounded by the media-open deadline: {elapsed:?}"
        );
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn media_open_deadline_expiry_reports_media_timeout_before_ipc_timeout() {
        let mut player = controller_with_runtime(Box::new(DelayedPrimaryTrackIpc {
            started: Instant::now(),
            delay: MEDIA_OPEN_TIMEOUT,
        }));

        let started = Instant::now();
        let error = player
            .open_until(
                test_dual_media(),
                Instant::now() + Duration::from_millis(150),
            )
            .err()
            .expect("media open must time out");

        assert_eq!(error, PlayerError::MediaOpenTimeout);
        assert_eq!(
            player.snapshot().last_error_code,
            Some(PlayerError::MediaOpenTimeout.code())
        );
        assert!(
            started.elapsed() < Duration::from_secs(2),
            "the media-open deadline must win before the per-request IPC timeout"
        );
        assert!(player.runtime.is_none());
    }

    #[test]
    fn relay_mode_rejects_audio_from_a_different_origin() {
        let same_origin = validate_media(
            MediaLoadRequest {
                url: "https://media.example.invalid/video.mp4".to_owned(),
                headers: Default::default(),
                audio: Some(MediaStreamRequest {
                    url: "https://media.example.invalid/audio.mp4".to_owned(),
                    headers: Default::default(),
                }),
            },
            MediaPolicy::from_environment(),
        )
        .expect("same-origin dual stream");
        assert!(relay_origin_for_playback(&same_origin).is_ok());

        let cross_origin = validate_media(
            MediaLoadRequest {
                url: "https://video.example.invalid/video.mp4".to_owned(),
                headers: Default::default(),
                audio: Some(MediaStreamRequest {
                    url: "https://audio.example.invalid/audio.mp4".to_owned(),
                    headers: Default::default(),
                }),
            },
            MediaPolicy::from_environment(),
        )
        .expect("cross-origin dual stream");
        assert_eq!(
            relay_origin_for_playback(&cross_origin)
                .expect_err("cross-origin dual stream must fail"),
            PlayerError::MediaDualOriginUnsupported
        );
    }

    #[cfg(windows)]
    #[test]
    fn relay_mode_configures_same_origin_dual_stream_through_controller_open() {
        let commands = Arc::new(Mutex::new(Vec::new()));
        let mut player = controller_with_relay_runtime(Box::new(RecordingIpc {
            commands: Arc::clone(&commands),
        }));

        player
            .open(test_dual_media())
            .expect("same-origin dual stream must open through the relay mode");
        let commands = commands.lock().expect("read recorded commands");
        let proxy = commands
            .iter()
            .find(|command| {
                command.first().and_then(Value::as_str) == Some("set_property")
                    && command.get(1).and_then(Value::as_str) == Some("http-proxy")
            })
            .and_then(|command| command.get(2))
            .and_then(Value::as_str)
            .expect("relay mode must configure mpv's HTTP proxy");
        assert!(proxy.starts_with("http://127.0.0.1:"));
        assert!(commands.iter().any(|command| {
            command.first().and_then(Value::as_str) == Some("audio-add")
                && command.get(2).and_then(Value::as_str) == Some("select")
        }));
        drop(commands);
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn relay_mode_rejects_cross_origin_dual_stream_end_to_end() {
        let mut player = controller_with_relay_runtime(Box::new(FakeIpc));

        let error = player
            .open(test_cross_origin_dual_media())
            .err()
            .expect("cross-origin dual stream must fail through open()");

        assert_eq!(error, PlayerError::MediaDualOriginUnsupported);
        assert_eq!(
            player.snapshot().last_error_code,
            Some(PlayerError::MediaDualOriginUnsupported.code())
        );
        assert!(player.runtime.is_none());
    }

    #[cfg(windows)]
    #[test]
    fn debug_loopback_mode_rejects_public_https_audio_mixed_with_loopback_http_video() {
        let _environment = set_opt_in_environment(true);
        let media = validate_media(
            MediaLoadRequest {
                url: "http://127.0.0.1:19090/video.mp4".to_owned(),
                headers: Default::default(),
                audio: Some(MediaStreamRequest {
                    url: "https://media.example.invalid/audio.mp4".to_owned(),
                    headers: Default::default(),
                }),
            },
            MediaPolicy::from_environment(),
        )
        .expect("each test URL is valid under its respective scheme policy");
        let mut player = controller_with_loopback_runtime(Box::new(FakeIpc));

        let error = player
            .open(media)
            .err()
            .expect("mixed loopback HTTP and public HTTPS streams must fail");

        assert_eq!(error, PlayerError::MediaDualOriginUnsupported);
        assert_eq!(
            player.snapshot().last_error_code,
            Some(PlayerError::MediaDualOriginUnsupported.code())
        );
        assert!(player.runtime.is_none());
    }

    #[cfg(windows)]
    #[test]
    fn debug_loopback_mode_reports_unsupported_egress_for_a_single_https_source() {
        let media = validate_media(
            MediaLoadRequest {
                url: "https://media.example.invalid/video.mp4".to_owned(),
                headers: Default::default(),
                audio: None,
            },
            MediaPolicy::from_environment(),
        )
        .expect("single HTTPS source");
        let mut player = controller_with_loopback_runtime(Box::new(FakeIpc));

        let error = player
            .open(media)
            .err()
            .expect("loopback fixture mode cannot open public HTTPS");

        assert_eq!(error, PlayerError::EgressModeUnsupported);
        assert_eq!(
            player.snapshot().last_error_code,
            Some(PlayerError::EgressModeUnsupported.code())
        );
        assert!(player.runtime.is_none());
    }

    #[cfg(windows)]
    #[test]
    fn file_loaded_transitions_to_playing_without_timing_metrics() {
        let mut player = controller_with_runtime(Box::new(FakeIpc));
        player.pending_load = Some(PendingLoad {
            saw_start: true,
            deadline: Instant::now() + MEDIA_OPEN_TIMEOUT,
            audio_state: AudioAttachState::NotRequested,
        });
        player.media_loaded = true;
        player.state = PlayerState::Opening;

        let snapshot = player.status();

        assert_eq!(snapshot.state, PlayerState::Playing);
        assert_eq!(snapshot.last_error_code, None);
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    fn opening_timeout_releases_runtime_and_reports_a_stable_error() {
        let mut player = controller_with_runtime(Box::new(FakeIpc));
        player.pending_load = Some(PendingLoad {
            saw_start: true,
            deadline: Instant::now() - Duration::from_secs(1),
            audio_state: AudioAttachState::NotRequested,
        });
        player.state = PlayerState::Opening;

        let snapshot = player.status();

        assert_eq!(snapshot.state, PlayerState::Error);
        assert_eq!(
            snapshot.last_error_code,
            Some(PlayerError::MediaOpenTimeout.code())
        );
        assert!(!snapshot.media_loaded);
        assert!(player.runtime.is_none());
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
        assert!(player.runtime.is_none());
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
        assert!(player.runtime.is_none());
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
    fn status_metrics_share_one_operation_deadline() {
        let mut player = controller_with_runtime_timeout(
            Box::new(SlowRespondingIpc(Duration::from_millis(80))),
            Duration::from_millis(120),
        );
        let started = Instant::now();

        let result = player.read_status_metrics(Instant::now() + player.operation_timeout());

        assert!(matches!(result, Err(PlayerError::IpcTimeout)));
        assert!(
            started.elapsed() < Duration::from_millis(250),
            "the whole status read must share one deadline"
        );
        if let Some(mut runtime) = player.runtime.take() {
            runtime.abort();
        }
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
        let _environment = set_opt_in_environment(false);
        let mut runtime = MpvRuntime::spawn().expect("start installed mpv");
        assert!(
            runtime.job_is_configured(),
            "mpv must be assigned to its job"
        );
        let proxy_url = {
            let relay = runtime
                .relay
                .as_ref()
                .expect("strict mode must install an egress relay");
            let origin = test_media()
                .relay_origin()
                .expect("test media must have a relay origin");
            relay
                .begin_playback(origin)
                .expect("start playback relay session")
        };
        runtime
            .command(&json!(["set_property", "http-proxy", proxy_url]))
            .expect("mpv must accept the runtime proxy setting");
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

    #[cfg(windows)]
    #[test]
    #[ignore = "requires an installed mpv and Python fixture server"]
    fn installed_mpv_loads_header_protected_loopback_fixture_and_seeks() {
        let _environment = set_opt_in_environment(true);
        let fixture = start_loopback_fixture("s1-h264-aac.mp4");
        let media = validate_media(
            MediaLoadRequest {
                url: fixture.info.url.clone(),
                headers: [
                    ("Referer".to_owned(), fixture.info.referer.clone()),
                    ("User-Agent".to_owned(), fixture.info.user_agent.clone()),
                ]
                .into_iter()
                .collect(),
                audio: None,
            },
            MediaPolicy::from_environment(),
        )
        .expect("valid loopback fixture media");

        let mut player = PlayerController::new();
        player
            .start_standalone()
            .expect("start player for S1 fixture");
        set_player_property(&mut player, "hr-seek", Value::String("yes".to_owned()));
        set_player_property(&mut player, "hwdec", Value::String("no".to_owned()));
        player.open(media).expect("open Header-protected fixture");
        let playing = wait_for_playing(&mut player);
        assert!(
            playing
                .duration_seconds
                .is_some_and(|duration| duration >= 2.0),
            "fixture duration must be available"
        );

        let paused = player
            .control(PlaybackCommand::Pause)
            .expect("pause fixture playback");
        assert_eq!(paused.state, PlayerState::Paused);
        verify_twenty_player_seeks(&mut player, "S1");
        verify_playback_advances(&mut player, "S1");
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    #[ignore = "requires an installed mpv and two Python fixture servers"]
    fn installed_mpv_merges_synthetic_video_and_audio_streams_and_seeks() {
        let _environment = set_opt_in_environment(true);
        let video_fixture = start_loopback_fixture("s1-video-only.mp4");
        let audio_fixture = start_loopback_fixture("s1-audio-only.mp4");
        assert_eq!(video_fixture.info.referer, audio_fixture.info.referer);
        assert_eq!(video_fixture.info.user_agent, audio_fixture.info.user_agent);
        let headers = [
            ("Referer".to_owned(), video_fixture.info.referer.clone()),
            (
                "User-Agent".to_owned(),
                video_fixture.info.user_agent.clone(),
            ),
        ]
        .into_iter()
        .collect::<BTreeMap<_, _>>();
        let media = validate_media(
            MediaLoadRequest {
                url: video_fixture.info.url.clone(),
                headers: headers.clone(),
                audio: Some(MediaStreamRequest {
                    url: audio_fixture.info.url.clone(),
                    headers,
                }),
            },
            MediaPolicy::from_environment(),
        )
        .expect("valid synthetic dual-stream media");

        let mut player = PlayerController::new();
        player
            .start_standalone()
            .expect("start player for synthetic dual stream");
        set_player_property(&mut player, "hr-seek", Value::String("yes".to_owned()));
        set_player_property(&mut player, "hwdec", Value::String("no".to_owned()));
        let pause_gate_delay = Duration::from_millis(1_200);
        player.media_open_test_delay = Some(pause_gate_delay);
        let open_started = Instant::now();
        player.open(media).expect("open synthetic dual stream");
        let open_elapsed = open_started.elapsed();
        let paused = player
            .control(PlaybackCommand::Pause)
            .expect("pause synthetic dual stream");
        let paused_position = player_time_position(&mut player);
        assert!(
            open_elapsed >= pause_gate_delay,
            "the real-mpv probe must wait while the primary track is paused"
        );
        assert_eq!(paused.state, PlayerState::Paused);
        assert!(
            paused_position < 0.75,
            "the primary clock advanced before audio attachment completed: {paused_position:.3}s"
        );
        eprintln!(
            "DUAL_STREAM_PAUSE_GATE|status=PASS|open_ms={}|position_ms={:.0}",
            open_elapsed.as_millis(),
            paused_position * 1_000.0
        );

        verify_dual_track_selection(&mut player, "dual-stream initial");
        assert_eq!(
            player_property(&mut player, "audio-codec-name").as_str(),
            Some("aac")
        );
        verify_twenty_player_seeks(&mut player, "dual-stream");
        verify_dual_track_selection(&mut player, "dual-stream after seeks");
        verify_playback_advances(&mut player, "dual-stream");
        verify_dual_track_selection(&mut player, "dual-stream after resume");

        let video_position = player_property(&mut player, "time-pos")
            .as_f64()
            .expect("video position");
        let audio_pts = player_property(&mut player, "audio-pts")
            .as_f64()
            .expect("audio PTS");
        assert!(video_position.is_finite() && audio_pts.is_finite());
        eprintln!(
            "DUAL_STREAM_PTS|status=PASS|video={video_position:.3}|audio={audio_pts:.3}|delta={:.3}",
            (audio_pts - video_position).abs()
        );
        player.shutdown();
    }

    #[cfg(windows)]
    #[test]
    #[ignore = "requires an installed mpv and Python fixture server"]
    fn installed_mpv_rejects_missing_external_audio_and_cleans_up() {
        let _environment = set_opt_in_environment(true);
        let fixture = start_loopback_fixture("s1-video-only.mp4");
        let media = validate_media(
            MediaLoadRequest {
                url: fixture.info.url.clone(),
                headers: [
                    ("Referer".to_owned(), fixture.info.referer.clone()),
                    ("User-Agent".to_owned(), fixture.info.user_agent.clone()),
                ]
                .into_iter()
                .collect(),
                audio: Some(MediaStreamRequest {
                    url: format!("{}.missing", fixture.info.url),
                    headers: [
                        ("Referer".to_owned(), fixture.info.referer.clone()),
                        ("User-Agent".to_owned(), fixture.info.user_agent.clone()),
                    ]
                    .into_iter()
                    .collect(),
                }),
            },
            MediaPolicy::from_environment(),
        )
        .expect("valid missing-audio fixture request");

        let mut player = PlayerController::new();
        player
            .start_standalone()
            .expect("start player for missing-audio probe");
        let error = player.open(media).err().expect("audio add must fail");

        assert_eq!(error, PlayerError::MediaAudioLoadFailed);
        assert_eq!(
            player.snapshot().last_error_code,
            Some(PlayerError::MediaAudioLoadFailed.code())
        );
        assert!(
            player.runtime.is_none(),
            "failed dual stream must be cleaned up"
        );
    }

    #[cfg(windows)]
    #[test]
    #[ignore = "requires an installed mpv and Python fixture server"]
    fn installed_mpv_decodes_synthetic_eac3_and_hevc10_fixtures_and_seeks() {
        let _environment = set_opt_in_environment(true);

        for case in [
            ("s2-h264-eac3.mkv", "h264", "eac3", "yuv420p", None),
            (
                "s3-hevc10-flac-ass.mkv",
                "hevc",
                "flac",
                "yuv420p10",
                Some("ass"),
            ),
        ] {
            let (
                media_name,
                expected_video,
                expected_audio,
                expected_pixel_format,
                expected_subtitle,
            ) = case;
            let fixture = start_loopback_fixture(media_name);
            let media = validate_media(
                MediaLoadRequest {
                    url: fixture.info.url.clone(),
                    headers: [
                        ("Referer".to_owned(), fixture.info.referer.clone()),
                        ("User-Agent".to_owned(), fixture.info.user_agent.clone()),
                    ]
                    .into_iter()
                    .collect(),
                    audio: None,
                },
                MediaPolicy::from_environment(),
            )
            .expect("valid synthetic loopback media");

            let mut player = PlayerController::new();
            player
                .start_standalone()
                .unwrap_or_else(|error| panic!("{media_name}: start player: {error:?}"));
            set_player_property(&mut player, "hr-seek", Value::String("yes".to_owned()));
            set_player_property(&mut player, "hwdec", Value::String("no".to_owned()));
            player.open(media).expect("open synthetic fixture");
            let playing = wait_for_playing(&mut player);
            assert!(
                playing
                    .duration_seconds
                    .is_some_and(|duration| duration >= 2.0),
                "{media_name} duration must be available"
            );
            player
                .control(PlaybackCommand::Pause)
                .expect("pause synthetic fixture");

            let video_codec = player_property(&mut player, "video-codec")
                .as_str()
                .expect("video codec string")
                .to_ascii_lowercase();
            assert!(
                video_codec.contains(expected_video)
                    || (expected_video == "hevc" && video_codec.contains("h265"))
                    || (expected_video == "h264" && video_codec.contains("avc")),
                "{media_name} decoded unexpected video codec: {video_codec}"
            );
            if expected_video == "hevc" {
                assert_eq!(
                    player_property(&mut player, "current-tracks/video/codec-profile")
                        .as_str()
                        .expect("video codec profile"),
                    "Main 10",
                    "{media_name} must decode the Main 10 profile"
                );
            }
            assert_eq!(
                player_property(&mut player, "audio-codec-name")
                    .as_str()
                    .expect("audio codec string"),
                expected_audio,
                "{media_name} decoded unexpected audio codec"
            );
            let pixel_format = player_property(&mut player, "video-params/pixelformat")
                .as_str()
                .expect("pixel format string")
                .to_ascii_lowercase();
            assert_eq!(
                pixel_format.trim_end_matches("le"),
                expected_pixel_format,
                "{media_name} decoded unexpected pixel format"
            );
            assert_eq!(player_property(&mut player, "width").as_i64(), Some(320));
            assert_eq!(player_property(&mut player, "height").as_i64(), Some(180));
            assert!(
                player_property(&mut player, "video-frame-info/picture-type")
                    .as_str()
                    .is_some_and(|value| !value.is_empty()),
                "{media_name} must report a decoded video frame"
            );
            assert_eq!(
                player_property(&mut player, "audio-params/samplerate").as_i64(),
                Some(48_000)
            );
            assert_eq!(
                player_property(&mut player, "audio-params/channel-count").as_i64(),
                Some(1)
            );
            assert_eq!(
                player_property(&mut player, "hwdec-current").as_str(),
                Some("no")
            );
            if let Some(expected_subtitle) = expected_subtitle {
                let tracks = player_property(&mut player, "track-list");
                let subtitle_track = tracks
                    .as_array()
                    .and_then(|tracks| {
                        tracks.iter().find(|track| {
                            track.get("type").and_then(Value::as_str) == Some("sub")
                                && track.get("codec").and_then(Value::as_str)
                                    == Some(expected_subtitle)
                        })
                    })
                    .unwrap_or_else(|| {
                        panic!("{media_name} must expose an embedded {expected_subtitle} subtitle track")
                    });
                assert_eq!(
                    subtitle_track.get("external").and_then(Value::as_bool),
                    Some(false),
                    "{media_name} subtitle track must be embedded"
                );
                let subtitle_id = subtitle_track
                    .get("id")
                    .and_then(Value::as_i64)
                    .expect("embedded subtitle track id");
                player
                    .runtime_command_until(
                        &json!(["set_property", "sid", subtitle_id]),
                        Instant::now() + Duration::from_secs(5),
                    )
                    .expect("select embedded ASS subtitle track");
                assert_eq!(
                    player_property(&mut player, "current-tracks/sub/codec")
                        .as_str()
                        .expect("selected subtitle codec"),
                    expected_subtitle,
                    "{media_name} must select the embedded subtitle track"
                );
            }

            verify_twenty_player_seeks(&mut player, media_name);
            if expected_subtitle.is_some() {
                assert_eq!(
                    player_property(&mut player, "current-tracks/sub/codec").as_str(),
                    expected_subtitle,
                    "{media_name} must retain the selected subtitle after seeks"
                );
            }
            verify_playback_advances(&mut player, media_name);
            if expected_subtitle.is_some() {
                assert_eq!(
                    player_property(&mut player, "current-tracks/sub/codec").as_str(),
                    expected_subtitle,
                    "{media_name} must retain the selected subtitle after resume"
                );
            }
            player.shutdown();
        }
    }

    #[cfg(windows)]
    #[test]
    #[ignore = "requires an installed mpv and public network access"]
    fn installed_mpv_routes_public_https_through_connect_relay() {
        let _environment = set_opt_in_environment(false);
        let mut runtime = MpvRuntime::spawn().expect("start installed mpv");
        let proxy_url = {
            let relay = runtime
                .relay
                .as_ref()
                .expect("strict mode must install an egress relay");
            let media = validate_media(
                MediaLoadRequest {
                    url: "https://example.com/".to_owned(),
                    headers: Default::default(),
                    audio: None,
                },
                MediaPolicy::from_environment(),
            )
            .expect("valid public HTTPS probe");
            let origin = media.relay_origin().expect("public HTTPS relay origin");
            relay
                .begin_playback(origin)
                .expect("start public HTTPS relay session")
        };
        runtime
            .command(&json!(["set_property", "http-proxy", proxy_url]))
            .expect("mpv must accept the proxy setting");
        runtime
            .command(&json!(["loadfile", "https://example.com/"]))
            .expect("mpv must accept the public HTTPS probe");

        let deadline = Instant::now() + Duration::from_secs(15);
        loop {
            let relay = runtime.relay.as_ref().expect("relay remains installed");
            if relay.established_tunnel_count() > 0 {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "mpv did not establish an owner-bound tunnel through the relay (raw={}, playback={})",
                relay.raw_connection_count(),
                relay.playback_connection_count()
            );
            thread::sleep(Duration::from_millis(50));
        }
        runtime.shutdown();
    }
}
