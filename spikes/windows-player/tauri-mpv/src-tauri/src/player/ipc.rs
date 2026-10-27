use std::fs::{File, OpenOptions};
use std::io::{BufRead, BufReader, Read, Write};
use std::path::Path;
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{json, Value};

const MAX_IPC_LINE_BYTES: usize = 64 * 1024;
const MAX_IPC_EVENTS_PER_REQUEST: usize = 128;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum IpcError {
    Io,
    Timeout,
    Protocol,
    Rejected,
    Closed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EndFileReason {
    Eof,
    Stop,
    Quit,
    Error,
    Redirect,
    Unknown,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum MpvEvent {
    StartFile,
    FileLoaded,
    EndFile(EndFileReason),
}

pub struct IpcReply {
    pub data: Option<Value>,
    pub events: Vec<MpvEvent>,
}

pub trait IpcConnection: Send {
    fn request(&mut self, request_id: u64, command: &[Value]) -> Result<IpcReply, IpcError>;

    fn notify(&mut self, command: &[Value]) -> Result<(), IpcError>;

    fn server_pid(&self) -> Result<u32, IpcError>;
}

pub struct JsonPipe {
    reader: BufReader<File>,
    writer: File,
}

impl JsonPipe {
    pub fn connect(path: &Path, timeout: Duration) -> Result<Self, IpcError> {
        let deadline = Instant::now() + timeout;
        loop {
            match OpenOptions::new().read(true).write(true).open(path) {
                Ok(file) => {
                    let reader_file = file.try_clone().map_err(|_| IpcError::Io)?;
                    return Ok(Self {
                        reader: BufReader::new(reader_file),
                        writer: file,
                    });
                }
                Err(_) if Instant::now() < deadline => {
                    thread::sleep(Duration::from_millis(40));
                }
                Err(_) => return Err(IpcError::Io),
            }
        }
    }

    fn write_message(&mut self, value: &Value) -> Result<(), IpcError> {
        serde_json::to_writer(&mut self.writer, value).map_err(|_| IpcError::Protocol)?;
        self.writer.write_all(b"\n").map_err(|_| IpcError::Io)?;
        self.writer.flush().map_err(|_| IpcError::Io)
    }

    fn read_line(&mut self) -> Result<Vec<u8>, IpcError> {
        let mut line = Vec::new();
        let read = {
            let mut limited = (&mut self.reader).take((MAX_IPC_LINE_BYTES + 1) as u64);
            limited
                .read_until(b'\n', &mut line)
                .map_err(|_| IpcError::Io)?
        };
        if read == 0 {
            return Err(IpcError::Closed);
        }
        if read > MAX_IPC_LINE_BYTES || !line.ends_with(b"\n") {
            return Err(IpcError::Protocol);
        }
        Ok(line)
    }
}

impl IpcConnection for JsonPipe {
    fn request(&mut self, request_id: u64, command: &[Value]) -> Result<IpcReply, IpcError> {
        self.write_message(&json!({
            "command": command,
            "request_id": request_id,
        }))?;

        let mut events = Vec::new();
        loop {
            let line = self.read_line()?;
            match parse_mpv_message(&line, request_id)? {
                ParsedIpcMessage::Event(event) => {
                    if events.len() >= MAX_IPC_EVENTS_PER_REQUEST {
                        return Err(IpcError::Protocol);
                    }
                    events.push(event);
                }
                ParsedIpcMessage::Ignored => {}
                ParsedIpcMessage::Response(data) => {
                    return Ok(IpcReply { data, events });
                }
            }
        }
    }

    fn notify(&mut self, command: &[Value]) -> Result<(), IpcError> {
        self.write_message(&json!({ "command": command }))
    }

    fn server_pid(&self) -> Result<u32, IpcError> {
        #[cfg(windows)]
        {
            use std::os::windows::io::AsRawHandle;

            use windows_sys::Win32::System::Pipes::GetNamedPipeServerProcessId;

            let mut pid = 0u32;
            // SAFETY: `writer` owns a live pipe handle for the duration of this call.
            let succeeded = unsafe {
                GetNamedPipeServerProcessId(self.writer.as_raw_handle().cast(), &mut pid)
            };
            if succeeded == 0 || pid == 0 {
                Err(IpcError::Io)
            } else {
                Ok(pid)
            }
        }

        #[cfg(not(windows))]
        {
            Err(IpcError::Io)
        }
    }
}

#[derive(Debug, PartialEq)]
enum ParsedIpcMessage {
    Event(MpvEvent),
    Ignored,
    Response(Option<Value>),
}

fn parse_mpv_message(line: &[u8], expected_request_id: u64) -> Result<ParsedIpcMessage, IpcError> {
    let value: Value = serde_json::from_slice(line).map_err(|_| IpcError::Protocol)?;
    let object = value.as_object().ok_or(IpcError::Protocol)?;

    if let Some(event) = object.get("event").and_then(Value::as_str) {
        return match event {
            "start-file" => Ok(ParsedIpcMessage::Event(MpvEvent::StartFile)),
            "file-loaded" => Ok(ParsedIpcMessage::Event(MpvEvent::FileLoaded)),
            "end-file" => {
                let reason = match object.get("reason").and_then(Value::as_str) {
                    Some("eof") => EndFileReason::Eof,
                    Some("stop") => EndFileReason::Stop,
                    Some("quit") => EndFileReason::Quit,
                    Some("error") => EndFileReason::Error,
                    Some("redirect") => EndFileReason::Redirect,
                    _ => EndFileReason::Unknown,
                };
                Ok(ParsedIpcMessage::Event(MpvEvent::EndFile(reason)))
            }
            // Event payloads can contain media metadata. Ignore all unrelated events.
            _ => Ok(ParsedIpcMessage::Ignored),
        };
    }

    let request_id = object
        .get("request_id")
        .and_then(Value::as_u64)
        .ok_or(IpcError::Protocol)?;
    if request_id != expected_request_id {
        return Err(IpcError::Protocol);
    }

    match object.get("error").and_then(Value::as_str) {
        Some("success") => Ok(ParsedIpcMessage::Response(Some(
            object.get("data").cloned().unwrap_or(Value::Null),
        ))),
        Some("property unavailable") => Ok(ParsedIpcMessage::Response(None)),
        Some(_) => Err(IpcError::Rejected),
        None => Err(IpcError::Protocol),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_playback_events_without_exposing_event_payloads() {
        let start = br#"{"event":"start-file","playlist_entry_id":7}
"#;
        let loaded = br#"{"event":"file-loaded","playlist_entry_id":7}
"#;
        let ended = br#"{"event":"end-file","reason":"error","error":"secret"}
"#;
        assert!(matches!(
            parse_mpv_message(start, 7),
            Ok(ParsedIpcMessage::Event(MpvEvent::StartFile))
        ));
        assert!(matches!(
            parse_mpv_message(loaded, 7),
            Ok(ParsedIpcMessage::Event(MpvEvent::FileLoaded))
        ));
        assert!(matches!(
            parse_mpv_message(ended, 7),
            Ok(ParsedIpcMessage::Event(MpvEvent::EndFile(
                EndFileReason::Error
            )))
        ));
    }

    #[test]
    fn parses_the_matching_response() {
        let response = br#"{"request_id":7,"error":"success","data":{"ok":true}}
"#;
        let parsed = match parse_mpv_message(response, 7).expect("valid response") {
            ParsedIpcMessage::Response(value) => value,
            ParsedIpcMessage::Event(_) => panic!("expected a response"),
            ParsedIpcMessage::Ignored => panic!("expected a response"),
        };
        assert_eq!(parsed, Some(json!({"ok": true})));
    }

    #[test]
    fn parses_success_without_data_as_null() {
        let response = br#"{"request_id":10,"error":"success"}
"#;
        let parsed = match parse_mpv_message(response, 10).expect("valid response") {
            ParsedIpcMessage::Response(value) => value,
            ParsedIpcMessage::Event(_) => panic!("expected a response"),
            ParsedIpcMessage::Ignored => panic!("expected a response"),
        };
        assert_eq!(parsed, Some(Value::Null));
    }

    #[test]
    fn rejects_error_response_without_echoing_mpv_data() {
        let response = br#"{"request_id":11,"error":"loading failed","data":"secret"}
"#;
        assert_eq!(parse_mpv_message(response, 11), Err(IpcError::Rejected));
    }

    #[test]
    fn distinguishes_an_unavailable_property_from_a_rejection() {
        let response = br#"{"request_id":13,"error":"property unavailable","data":null}
"#;
        assert_eq!(
            parse_mpv_message(response, 13),
            Ok(ParsedIpcMessage::Response(None))
        );
    }

    #[test]
    fn rejects_malformed_and_wrong_request_responses() {
        let malformed = b"not json\n";
        let wrong_id = br#"{"request_id":99,"error":"success","data":null}
"#;
        let missing_error = br#"{"request_id":12,"data":null}
"#;

        assert_eq!(parse_mpv_message(malformed, 12), Err(IpcError::Protocol));
        assert_eq!(parse_mpv_message(wrong_id, 12), Err(IpcError::Protocol));
        assert_eq!(
            parse_mpv_message(missing_error, 12),
            Err(IpcError::Protocol)
        );
    }

    #[test]
    fn ignores_unrelated_events_but_rejects_invalid_objects() {
        let unrelated = br#"{"event":"property-change","name":"pause","data":false}
"#;
        let invalid = br#"{"data":false}
"#;

        assert!(matches!(
            parse_mpv_message(unrelated, 12),
            Ok(ParsedIpcMessage::Ignored)
        ));
        assert_eq!(parse_mpv_message(invalid, 12), Err(IpcError::Protocol));
    }
}
