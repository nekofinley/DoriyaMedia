use std::ffi::{c_void, OsStr, OsString};
use std::fs::File;
use std::io::{self, Read, Write};
use std::mem::size_of;
use std::os::windows::ffi::OsStrExt;
use std::os::windows::io::{AsRawHandle, FromRawHandle, OwnedHandle};
use std::os::windows::process::ExitStatusExt;
use std::path::{Path, PathBuf};
use std::process::{Child, ExitStatus};
use std::ptr::{addr_of, null, null_mut};
use std::sync::Arc;
use std::time::Instant;

use windows_sys::Win32::Foundation::{
    CloseHandle, DuplicateHandle, GetLastError, DUPLICATE_SAME_ACCESS, ERROR_BROKEN_PIPE,
    ERROR_FILE_NOT_FOUND, ERROR_HANDLE_EOF, ERROR_INSUFFICIENT_BUFFER, ERROR_IO_PENDING,
    ERROR_PATH_NOT_FOUND, ERROR_PIPE_CONNECTED, FILETIME, HANDLE, INVALID_HANDLE_VALUE,
    WAIT_FAILED, WAIT_OBJECT_0, WAIT_TIMEOUT,
};
use windows_sys::Win32::Security::{
    AddAccessAllowedAceEx, EqualSid, GetAce, GetKernelObjectSecurity, GetLengthSid,
    GetSecurityDescriptorDacl, GetTokenInformation, InitializeAcl, InitializeSecurityDescriptor,
    IsValidSid, SetSecurityDescriptorDacl, SetSecurityDescriptorOwner, TokenSessionId, TokenUser,
    ACCESS_ALLOWED_ACE, ACL, ACL_REVISION, DACL_SECURITY_INFORMATION, PSID, SECURITY_ATTRIBUTES,
    SECURITY_DESCRIPTOR, TOKEN_QUERY, TOKEN_USER,
};
use windows_sys::Win32::Storage::FileSystem::{
    CreateFileW, FILE_FLAG_FIRST_PIPE_INSTANCE, FILE_FLAG_OVERLAPPED, FILE_GENERIC_READ,
    FILE_GENERIC_WRITE, FILE_SHARE_READ, FILE_SHARE_WRITE, OPEN_EXISTING, PIPE_ACCESS_DUPLEX,
    SYNCHRONIZE,
};
use windows_sys::Win32::System::JobObjects::{
    AssignProcessToJobObject, CreateJobObjectW, IsProcessInJob, JobObjectExtendedLimitInformation,
    QueryInformationJobObject, SetInformationJobObject, JOBOBJECT_EXTENDED_LIMIT_INFORMATION,
    JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
};
use windows_sys::Win32::System::Pipes::{
    ConnectNamedPipe, CreateNamedPipeW, PIPE_READMODE_BYTE, PIPE_REJECT_REMOTE_CLIENTS,
    PIPE_TYPE_BYTE, PIPE_WAIT,
};
use windows_sys::Win32::System::SystemServices::{
    ACCESS_ALLOWED_ACE_TYPE, SECURITY_DESCRIPTOR_REVISION,
};
use windows_sys::Win32::System::Threading::{
    CreateEventW, CreateProcessW, DeleteProcThreadAttributeList, GetCurrentProcess,
    GetExitCodeProcess, GetProcessTimes, InitializeProcThreadAttributeList, OpenProcess,
    OpenProcessToken, TerminateProcess, UpdateProcThreadAttribute, WaitForSingleObject,
    CREATE_NO_WINDOW, CREATE_UNICODE_ENVIRONMENT, EXTENDED_STARTUPINFO_PRESENT, INFINITE,
    PROCESS_INFORMATION, PROCESS_QUERY_LIMITED_INFORMATION, PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
    PROC_THREAD_ATTRIBUTE_JOB_LIST, STARTF_USESTDHANDLES, STARTUPINFOEXW,
};
use windows_sys::Win32::System::IO::{CancelIoEx, GetOverlappedResult, OVERLAPPED};

const PIPE_BUFFER_BYTES: u32 = 64 * 1024;
const PIPE_CLIENT_ACCESS: u32 = FILE_GENERIC_READ | FILE_GENERIC_WRITE | SYNCHRONIZE;
#[cfg(test)]
const PIPE_CONNECT_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(5);

pub(crate) struct SidecarChild {
    inner: SidecarChildInner,
}

enum SidecarChildInner {
    Standard(Child),
    Restricted { process: OwnedHandle, pid: u32 },
}

impl From<Child> for SidecarChild {
    fn from(child: Child) -> Self {
        Self {
            inner: SidecarChildInner::Standard(child),
        }
    }
}

impl SidecarChild {
    pub(crate) fn spawn_restricted(
        program: &OsStr,
        arguments: &[OsString],
        environment: &[(OsString, OsString)],
        stdin: File,
        job: &JobObject,
    ) -> io::Result<Self> {
        let command_line = build_command_line(program, arguments)?;
        let environment_block = build_environment_block(environment)?;
        let nul_path = wide_null(OsStr::new("NUL"));
        let raw_nul = unsafe {
            CreateFileW(
                nul_path.as_ptr(),
                FILE_GENERIC_WRITE,
                FILE_SHARE_READ | FILE_SHARE_WRITE,
                null(),
                OPEN_EXISTING,
                0,
                null_mut(),
            )
        };
        if raw_nul == INVALID_HANDLE_VALUE {
            return Err(io::Error::last_os_error());
        }
        let nul = unsafe { OwnedHandle::from_raw_handle(raw_nul) };

        let inherited_stdin = duplicate_inheritable(stdin.as_raw_handle())?;
        let inherited_nul = duplicate_inheritable(nul.as_raw_handle())?;
        let inherited_handles = [
            inherited_stdin.as_raw_handle(),
            inherited_nul.as_raw_handle(),
        ];
        let job_handles = [job.handle.as_raw_handle()];
        let mut attribute_size = 0usize;
        unsafe {
            InitializeProcThreadAttributeList(null_mut(), 2, 0, &mut attribute_size);
        }
        if attribute_size == 0 {
            return Err(io::Error::last_os_error());
        }
        let attribute_words = attribute_size.div_ceil(size_of::<usize>());
        let mut attribute_storage = vec![0usize; attribute_words];
        let attribute_list = attribute_storage.as_mut_ptr().cast::<c_void>();
        if unsafe { InitializeProcThreadAttributeList(attribute_list, 2, 0, &mut attribute_size) }
            == 0
        {
            return Err(io::Error::last_os_error());
        }
        let attribute_list = ProcThreadAttributeList {
            pointer: attribute_list,
        };
        if unsafe {
            UpdateProcThreadAttribute(
                attribute_list.pointer,
                0,
                PROC_THREAD_ATTRIBUTE_JOB_LIST as usize,
                job_handles.as_ptr().cast::<c_void>(),
                size_of::<[HANDLE; 1]>(),
                null_mut(),
                null(),
            )
        } == 0
        {
            return Err(io::Error::last_os_error());
        }
        if unsafe {
            UpdateProcThreadAttribute(
                attribute_list.pointer,
                0,
                PROC_THREAD_ATTRIBUTE_HANDLE_LIST as usize,
                inherited_handles.as_ptr().cast::<c_void>(),
                size_of::<[HANDLE; 2]>(),
                null_mut(),
                null(),
            )
        } == 0
        {
            return Err(io::Error::last_os_error());
        }

        let mut startup = unsafe { std::mem::zeroed::<STARTUPINFOEXW>() };
        startup.StartupInfo.cb = size_of::<STARTUPINFOEXW>() as u32;
        startup.StartupInfo.dwFlags = STARTF_USESTDHANDLES;
        startup.StartupInfo.hStdInput = inherited_stdin.as_raw_handle();
        startup.StartupInfo.hStdOutput = inherited_nul.as_raw_handle();
        startup.StartupInfo.hStdError = inherited_nul.as_raw_handle();
        startup.lpAttributeList = attribute_list.pointer;

        let mut command_line = command_line;
        let mut process_info = unsafe { std::mem::zeroed::<PROCESS_INFORMATION>() };
        let created = unsafe {
            CreateProcessW(
                null(),
                command_line.as_mut_ptr(),
                null(),
                null(),
                1,
                EXTENDED_STARTUPINFO_PRESENT | CREATE_NO_WINDOW | CREATE_UNICODE_ENVIRONMENT,
                environment_block.as_ptr().cast::<c_void>(),
                null(),
                &startup.StartupInfo,
                &mut process_info,
            )
        };
        if created == 0 {
            let error = io::Error::last_os_error();
            if matches!(
                error.raw_os_error().map(|code| code as u32),
                Some(ERROR_FILE_NOT_FOUND | ERROR_PATH_NOT_FOUND)
            ) {
                return Err(io::Error::new(io::ErrorKind::NotFound, error));
            }
            return Err(error);
        }
        unsafe {
            CloseHandle(process_info.hThread);
        }
        let process = unsafe { OwnedHandle::from_raw_handle(process_info.hProcess) };
        Ok(Self {
            inner: SidecarChildInner::Restricted {
                process,
                pid: process_info.dwProcessId,
            },
        })
    }

    pub(crate) fn as_raw_handle(&self) -> HANDLE {
        match &self.inner {
            SidecarChildInner::Standard(child) => child.as_raw_handle(),
            SidecarChildInner::Restricted { process, .. } => process.as_raw_handle(),
        }
    }

    pub(crate) fn id(&self) -> u32 {
        match &self.inner {
            SidecarChildInner::Standard(child) => child.id(),
            SidecarChildInner::Restricted { pid, .. } => *pid,
        }
    }

    pub(crate) fn creation_time(&self) -> io::Result<u64> {
        process_creation_time_for_handle(self.as_raw_handle())
    }

    pub(crate) fn try_wait(&mut self) -> io::Result<Option<ExitStatus>> {
        match &mut self.inner {
            SidecarChildInner::Standard(child) => child.try_wait(),
            SidecarChildInner::Restricted { process, .. } => {
                match unsafe { WaitForSingleObject(process.as_raw_handle(), 0) } {
                    WAIT_OBJECT_0 => restricted_exit_status(process.as_raw_handle()).map(Some),
                    WAIT_TIMEOUT => Ok(None),
                    WAIT_FAILED => Err(io::Error::last_os_error()),
                    _ => Err(io::Error::other("unexpected process wait result")),
                }
            }
        }
    }

    pub(crate) fn kill(&mut self) -> io::Result<()> {
        match &mut self.inner {
            SidecarChildInner::Standard(child) => child.kill(),
            SidecarChildInner::Restricted { process, .. } => {
                if unsafe { TerminateProcess(process.as_raw_handle(), 1) } == 0 {
                    Err(io::Error::last_os_error())
                } else {
                    Ok(())
                }
            }
        }
    }

    #[cfg(test)]
    pub(crate) fn wait(&mut self) -> io::Result<ExitStatus> {
        match &mut self.inner {
            SidecarChildInner::Standard(child) => child.wait(),
            SidecarChildInner::Restricted { process, .. } => {
                if unsafe { WaitForSingleObject(process.as_raw_handle(), INFINITE) }
                    != WAIT_OBJECT_0
                {
                    return Err(io::Error::last_os_error());
                }
                restricted_exit_status(process.as_raw_handle())
            }
        }
    }
}

struct ProcThreadAttributeList {
    pointer: *mut c_void,
}

impl Drop for ProcThreadAttributeList {
    fn drop(&mut self) {
        unsafe {
            DeleteProcThreadAttributeList(self.pointer);
        }
    }
}

fn restricted_exit_status(process: HANDLE) -> io::Result<ExitStatus> {
    let mut exit_code = 0u32;
    if unsafe { GetExitCodeProcess(process, &mut exit_code) } == 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(ExitStatus::from_raw(exit_code))
}

fn duplicate_inheritable(handle: HANDLE) -> io::Result<OwnedHandle> {
    let process = unsafe { GetCurrentProcess() };
    let mut duplicate = null_mut();
    if unsafe {
        DuplicateHandle(
            process,
            handle,
            process,
            &mut duplicate,
            0,
            1,
            DUPLICATE_SAME_ACCESS,
        )
    } == 0
    {
        return Err(io::Error::last_os_error());
    }
    Ok(unsafe { OwnedHandle::from_raw_handle(duplicate) })
}

fn build_environment_block(environment: &[(OsString, OsString)]) -> io::Result<Vec<u16>> {
    let mut sorted = environment.to_vec();
    sorted.sort_by(|left, right| {
        left.0
            .to_string_lossy()
            .to_ascii_uppercase()
            .cmp(&right.0.to_string_lossy().to_ascii_uppercase())
    });

    let mut block = Vec::new();
    for (name, value) in sorted {
        let name = name.encode_wide().collect::<Vec<_>>();
        let value = value.encode_wide().collect::<Vec<_>>();
        if name.is_empty()
            || name.contains(&0)
            || value.contains(&0)
            || name.contains(&u16::from(b'='))
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "sidecar environment contains an invalid entry",
            ));
        }
        block.extend(name);
        block.push(u16::from(b'='));
        block.extend(value);
        block.push(0);
    }
    block.push(0);
    Ok(block)
}

fn build_command_line(program: &OsStr, arguments: &[OsString]) -> io::Result<Vec<u16>> {
    let mut command_line = Vec::new();
    append_command_line_argument(&mut command_line, program)?;
    for argument in arguments {
        command_line.push(u16::from(b' '));
        append_command_line_argument(&mut command_line, argument)?;
    }
    command_line.push(0);
    Ok(command_line)
}

fn append_command_line_argument(command_line: &mut Vec<u16>, argument: &OsStr) -> io::Result<()> {
    let argument = argument.encode_wide().collect::<Vec<_>>();
    if argument.contains(&0) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "process argument contains a NUL",
        ));
    }

    let needs_quotes = argument.is_empty()
        || argument
            .iter()
            .any(|character| *character == 0x09 || *character == 0x20 || *character == 0x22);
    if !needs_quotes {
        command_line.extend(argument);
        return Ok(());
    }

    command_line.push(u16::from(b'"'));
    let mut backslashes = 0usize;
    for character in argument {
        if character == u16::from(b'\\') {
            backslashes += 1;
            continue;
        }
        if character == u16::from(b'"') {
            command_line.extend(std::iter::repeat(u16::from(b'\\')).take(backslashes * 2 + 1));
            command_line.push(character);
        } else {
            command_line.extend(std::iter::repeat(u16::from(b'\\')).take(backslashes));
            command_line.push(character);
        }
        backslashes = 0;
    }
    command_line.extend(std::iter::repeat(u16::from(b'\\')).take(backslashes * 2));
    command_line.push(u16::from(b'"'));
    Ok(())
}

fn wide_null(value: &OsStr) -> Vec<u16> {
    value.encode_wide().chain(std::iter::once(0)).collect()
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PipeError {
    Create,
    Security,
    Connect,
    Identity,
}

impl PipeError {
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::Create => "create",
            Self::Security => "security",
            Self::Connect => "connect",
            Self::Identity => "identity",
        }
    }
}

pub(crate) struct NamedPipeServer {
    handle: OwnedHandle,
    path: PathBuf,
}

impl NamedPipeServer {
    pub(crate) fn create(path: &Path, user_sid: &[u8]) -> Result<Self, PipeError> {
        if user_sid.is_empty() || unsafe { IsValidSid(sid_ptr(user_sid)) } == 0 {
            return Err(PipeError::Security);
        }

        let acl_size =
            size_of::<ACL>() + size_of::<ACCESS_ALLOWED_ACE>() - size_of::<u32>() + user_sid.len();
        let acl_words = acl_size.div_ceil(size_of::<u32>());
        let mut acl_storage = vec![0u32; acl_words];
        let acl = acl_storage.as_mut_ptr().cast::<ACL>();

        // SAFETY: The ACL storage is aligned and sized for the ACL plus one
        // current-user ACE. The SID remains valid for the duration of the call.
        unsafe {
            if InitializeAcl(acl, acl_size as u32, ACL_REVISION) == 0
                || AddAccessAllowedAceEx(
                    acl,
                    ACL_REVISION,
                    0,
                    PIPE_CLIENT_ACCESS,
                    sid_ptr(user_sid),
                ) == 0
            {
                return Err(PipeError::Security);
            }
        }

        let mut descriptor = Box::<SECURITY_DESCRIPTOR>::default();
        let descriptor_ptr = descriptor.as_mut() as *mut SECURITY_DESCRIPTOR;
        // SAFETY: `descriptor` and `acl_storage` are live and aligned while the
        // security descriptor is used to create the pipe.
        unsafe {
            if InitializeSecurityDescriptor(
                descriptor_ptr.cast::<c_void>(),
                SECURITY_DESCRIPTOR_REVISION,
            ) == 0
                || SetSecurityDescriptorOwner(descriptor_ptr.cast::<c_void>(), sid_ptr(user_sid), 0)
                    == 0
                || SetSecurityDescriptorDacl(descriptor_ptr.cast::<c_void>(), 1, acl, 0) == 0
            {
                return Err(PipeError::Security);
            }
        }

        let security_attributes = SECURITY_ATTRIBUTES {
            nLength: size_of::<SECURITY_ATTRIBUTES>() as u32,
            lpSecurityDescriptor: descriptor_ptr.cast::<c_void>(),
            bInheritHandle: 0,
        };
        let wide_path = path
            .as_os_str()
            .encode_wide()
            .chain(std::iter::once(0))
            .collect::<Vec<_>>();

        // SAFETY: The pipe name is NUL-terminated and the security descriptor,
        // ACL, and SID storage all remain live through this call.
        let raw_handle = unsafe {
            CreateNamedPipeW(
                wide_path.as_ptr(),
                PIPE_ACCESS_DUPLEX | FILE_FLAG_FIRST_PIPE_INSTANCE | FILE_FLAG_OVERLAPPED,
                PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT | PIPE_REJECT_REMOTE_CLIENTS,
                1,
                PIPE_BUFFER_BYTES,
                PIPE_BUFFER_BYTES,
                0,
                &security_attributes,
            )
        };
        if raw_handle == INVALID_HANDLE_VALUE {
            return Err(PipeError::Create);
        }

        // SAFETY: CreateNamedPipeW returned a new owned handle on success.
        let handle = unsafe { OwnedHandle::from_raw_handle(raw_handle) };
        verify_pipe_dacl(&handle, user_sid)?;

        Ok(Self {
            handle,
            path: path.to_path_buf(),
        })
    }

    #[cfg(test)]
    pub(crate) fn connect_client(&self) -> Result<File, PipeError> {
        self.connect_client_until(Instant::now() + PIPE_CONNECT_TIMEOUT)
    }

    pub(crate) fn connect_client_until(&self, deadline: Instant) -> Result<File, PipeError> {
        let timeout_millis = remaining_millis(deadline)?;
        let mut overlapped = unsafe { std::mem::zeroed::<OVERLAPPED>() };
        // SAFETY: Null attributes request a non-inheritable, unnamed manual-reset event.
        let raw_event = unsafe { CreateEventW(null(), 1, 0, null()) };
        if raw_event.is_null() {
            return Err(PipeError::Create);
        }
        // SAFETY: CreateEventW returned a new owned handle on success.
        let event = unsafe { OwnedHandle::from_raw_handle(raw_event) };
        overlapped.hEvent = event.as_raw_handle();

        // SAFETY: The server is live and listening, the OVERLAPPED storage
        // remains live until the operation completes, and hEvent is valid.
        let connect_result =
            unsafe { ConnectNamedPipe(self.handle.as_raw_handle(), &mut overlapped) };
        let mut connect_pending = false;
        if connect_result == 0 {
            let error = unsafe { GetLastError() };
            if error == ERROR_IO_PENDING {
                connect_pending = true;
            } else if error == ERROR_PIPE_CONNECTED {
                // This function owns the sole client endpoint. A different
                // same-user endpoint already winning the race is a failed
                // handshake, not a reason to wait indefinitely.
                return Err(PipeError::Connect);
            } else {
                return Err(PipeError::Connect);
            }
        }

        let wide_path = self
            .path
            .as_os_str()
            .encode_wide()
            .chain(std::iter::once(0))
            .collect::<Vec<_>>();
        // SAFETY: The path is NUL-terminated and the server is listening.
        let raw_client = unsafe {
            CreateFileW(
                wide_path.as_ptr(),
                FILE_GENERIC_READ | FILE_GENERIC_WRITE,
                0,
                null(),
                OPEN_EXISTING,
                FILE_FLAG_OVERLAPPED,
                null_mut(),
            )
        };
        if raw_client == INVALID_HANDLE_VALUE {
            if connect_pending {
                cancel_and_reap(self.handle.as_raw_handle(), &overlapped);
            }
            return Err(PipeError::Connect);
        }
        // SAFETY: CreateFileW returned a new owned handle on success.
        let client = unsafe { File::from_raw_handle(raw_client) };

        if connect_pending {
            // SAFETY: The event belongs to this pending ConnectNamedPipe call.
            if unsafe { WaitForSingleObject(event.as_raw_handle(), timeout_millis) } != 0 {
                cancel_and_reap(self.handle.as_raw_handle(), &overlapped);
                return Err(PipeError::Connect);
            }
            let mut transferred = 0u32;
            // SAFETY: The OVERLAPPED storage and server handle are live.
            if unsafe {
                GetOverlappedResult(
                    self.handle.as_raw_handle(),
                    &overlapped,
                    &mut transferred,
                    0,
                )
            } == 0
            {
                return Err(PipeError::Connect);
            }
        }

        Ok(client)
    }

    pub(crate) fn into_transport(self) -> PipeTransport {
        PipeTransport {
            handle: Arc::new(self.handle),
        }
    }
}

fn remaining_millis(deadline: Instant) -> Result<u32, PipeError> {
    let remaining = deadline.saturating_duration_since(Instant::now());
    if remaining.is_zero() {
        return Err(PipeError::Connect);
    }
    Ok(u32::try_from(remaining.as_millis())
        .unwrap_or(u32::MAX)
        .max(1))
}

#[derive(Clone)]
pub(crate) struct PipeTransport {
    handle: Arc<OwnedHandle>,
}

impl Read for PipeTransport {
    fn read(&mut self, buffer: &mut [u8]) -> io::Result<usize> {
        if buffer.is_empty() {
            return Ok(0);
        }
        let length = u32::try_from(buffer.len())
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "pipe read is too large"))?;
        let mut overlapped = OwnedOverlapped::new()?;
        // SAFETY: The handle, output buffer, and OVERLAPPED storage are live
        // until this operation completes.
        let started = unsafe {
            windows_sys::Win32::Storage::FileSystem::ReadFile(
                self.handle.as_raw_handle(),
                buffer.as_mut_ptr(),
                length,
                null_mut(),
                overlapped.as_mut(),
            )
        };
        if started == 0 {
            let error = io::Error::last_os_error();
            if error.raw_os_error() != Some(ERROR_IO_PENDING as i32) {
                if matches!(
                    error.raw_os_error().map(|code| code as u32),
                    Some(ERROR_BROKEN_PIPE | ERROR_HANDLE_EOF)
                ) {
                    return Ok(0);
                }
                return Err(error);
            }
        }
        let (succeeded, transferred) =
            overlapped.finish(self.handle.as_raw_handle(), started != 0)?;
        if !succeeded {
            return Ok(0);
        }
        Ok(usize::try_from(transferred).unwrap_or(usize::MAX))
    }
}

impl Write for PipeTransport {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        if buffer.is_empty() {
            return Ok(0);
        }
        let length = u32::try_from(buffer.len())
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "pipe write is too large"))?;
        let mut overlapped = OwnedOverlapped::new()?;
        // SAFETY: The handle, input buffer, and OVERLAPPED storage are live
        // until this operation completes.
        let started = unsafe {
            windows_sys::Win32::Storage::FileSystem::WriteFile(
                self.handle.as_raw_handle(),
                buffer.as_ptr(),
                length,
                null_mut(),
                overlapped.as_mut(),
            )
        };
        if started == 0 {
            let error = io::Error::last_os_error();
            if error.raw_os_error() != Some(ERROR_IO_PENDING as i32) {
                return Err(error);
            }
        }
        let (succeeded, transferred) =
            overlapped.finish(self.handle.as_raw_handle(), started != 0)?;
        if !succeeded {
            return Err(io::Error::new(
                io::ErrorKind::BrokenPipe,
                "the mpv IPC pipe closed during write",
            ));
        }
        Ok(usize::try_from(transferred).unwrap_or(usize::MAX))
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

struct OwnedOverlapped {
    overlapped: OVERLAPPED,
    event: OwnedHandle,
}

impl OwnedOverlapped {
    fn new() -> io::Result<Self> {
        // SAFETY: Null attributes request a non-inheritable, unnamed manual-reset event.
        let raw_event = unsafe { CreateEventW(null(), 1, 0, null()) };
        if raw_event.is_null() {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: CreateEventW returned a new owned handle on success.
        let event = unsafe { OwnedHandle::from_raw_handle(raw_event) };
        let mut overlapped = unsafe { std::mem::zeroed::<OVERLAPPED>() };
        overlapped.hEvent = event.as_raw_handle();
        Ok(Self { overlapped, event })
    }

    fn as_mut(&mut self) -> *mut OVERLAPPED {
        &mut self.overlapped
    }

    fn finish(&self, pipe: HANDLE, completed_immediately: bool) -> io::Result<(bool, u32)> {
        if !completed_immediately {
            // SAFETY: The event belongs to this pending overlapped operation.
            if unsafe { WaitForSingleObject(self.event.as_raw_handle(), INFINITE) } != 0 {
                let error = io::Error::last_os_error();
                cancel_and_reap(pipe, &self.overlapped);
                return Err(error);
            }
        }
        let mut transferred = 0u32;
        // SAFETY: The OVERLAPPED storage and pipe handle remain live.
        if unsafe { GetOverlappedResult(pipe, &self.overlapped, &mut transferred, 0) } == 0 {
            let error = io::Error::last_os_error();
            if matches!(
                error.raw_os_error().map(|code| code as u32),
                Some(ERROR_BROKEN_PIPE | ERROR_HANDLE_EOF)
            ) {
                return Ok((false, 0));
            }
            return Err(error);
        }
        Ok((true, transferred))
    }
}

fn cancel_and_reap(pipe: HANDLE, overlapped: &OVERLAPPED) {
    // SAFETY: `pipe` and `overlapped` identify the operation still owned by the
    // caller. CancelIoEx is harmless if it already completed, and the blocking
    // GetOverlappedResult keeps the OVERLAPPED storage alive until completion.
    unsafe {
        CancelIoEx(pipe, overlapped);
        let mut transferred = 0u32;
        GetOverlappedResult(pipe, overlapped, &mut transferred, 1);
    }
}

pub(crate) struct JobObject {
    handle: OwnedHandle,
}

impl JobObject {
    pub(crate) fn new_kill_on_close() -> Result<Self, PipeError> {
        // SAFETY: A null name creates an unnamed job and a null attributes
        // pointer requests a non-inheritable default handle.
        let raw_handle = unsafe { CreateJobObjectW(null(), null()) };
        if raw_handle.is_null() || raw_handle == INVALID_HANDLE_VALUE {
            return Err(PipeError::Create);
        }

        // SAFETY: CreateJobObjectW returned a new owned handle on success.
        let job = Self {
            handle: unsafe { OwnedHandle::from_raw_handle(raw_handle) },
        };
        let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
        limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;

        // SAFETY: `limits` is correctly sized and lives through the call.
        if unsafe {
            SetInformationJobObject(
                job.handle.as_raw_handle(),
                JobObjectExtendedLimitInformation,
                (&limits as *const JOBOBJECT_EXTENDED_LIMIT_INFORMATION).cast::<c_void>(),
                size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            )
        } == 0
            || !job.kill_on_close_enabled()?
        {
            return Err(PipeError::Security);
        }

        Ok(job)
    }

    pub(crate) fn ensure_assigned(&self, child: &SidecarChild) -> Result<(), PipeError> {
        // Production processes are assigned atomically through
        // PROC_THREAD_ATTRIBUTE_JOB_LIST. Standard children exist only in tests.
        if matches!(&child.inner, SidecarChildInner::Standard(_))
            && unsafe {
                AssignProcessToJobObject(self.handle.as_raw_handle(), child.as_raw_handle())
            } == 0
        {
            return Err(PipeError::Identity);
        }
        if !self.contains(child)? {
            return Err(PipeError::Identity);
        }
        Ok(())
    }

    fn kill_on_close_enabled(&self) -> Result<bool, PipeError> {
        let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
        // SAFETY: `limits` is correctly sized and lives through the call.
        if unsafe {
            QueryInformationJobObject(
                self.handle.as_raw_handle(),
                JobObjectExtendedLimitInformation,
                (&mut limits as *mut JOBOBJECT_EXTENDED_LIMIT_INFORMATION).cast::<c_void>(),
                size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
                null_mut(),
            )
        } == 0
        {
            return Err(PipeError::Security);
        }

        Ok(limits.BasicLimitInformation.LimitFlags & JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE != 0)
    }

    fn contains(&self, child: &SidecarChild) -> Result<bool, PipeError> {
        let mut in_job = 0i32;
        // SAFETY: The process and job handles are live for the duration of the
        // call, and `in_job` is a valid output pointer.
        if unsafe {
            IsProcessInJob(
                child.as_raw_handle(),
                self.handle.as_raw_handle(),
                &mut in_job,
            )
        } == 0
        {
            return Err(PipeError::Identity);
        }
        Ok(in_job != 0)
    }
}

pub(crate) fn current_user_sid() -> Result<Vec<u8>, PipeError> {
    let token = current_process_token()?;
    token_user_sid(token.as_raw_handle())
}

#[cfg(test)]
pub(crate) fn current_process_creation_time() -> Result<u64, PipeError> {
    process_creation_time_for_handle(unsafe { GetCurrentProcess() })
        .map_err(|_| PipeError::Identity)
}

pub(crate) fn process_identity_matches(pid: u32, creation_time: u64) -> Result<bool, PipeError> {
    if pid == 0 {
        return Ok(false);
    }
    let raw_process = unsafe { OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid) };
    if raw_process.is_null() {
        return Err(PipeError::Identity);
    }
    let process = unsafe { OwnedHandle::from_raw_handle(raw_process) };
    let current = process_creation_time_for_handle(process.as_raw_handle())
        .map_err(|_| PipeError::Identity)?;
    Ok(current == creation_time)
}

fn process_creation_time_for_handle(process: HANDLE) -> io::Result<u64> {
    let mut creation = unsafe { std::mem::zeroed::<FILETIME>() };
    let mut exit = unsafe { std::mem::zeroed::<FILETIME>() };
    let mut kernel = unsafe { std::mem::zeroed::<FILETIME>() };
    let mut user = unsafe { std::mem::zeroed::<FILETIME>() };
    // SAFETY: `process` is a live process handle and all four output pointers
    // reference valid, writable FILETIME storage.
    if unsafe { GetProcessTimes(process, &mut creation, &mut exit, &mut kernel, &mut user) } == 0 {
        return Err(io::Error::last_os_error());
    }
    Ok((u64::from(creation.dwHighDateTime) << 32) | u64::from(creation.dwLowDateTime))
}

pub(crate) fn current_user_session_id() -> Result<u32, PipeError> {
    let token = current_process_token()?;
    token_session_id(token.as_raw_handle())
}

pub(crate) fn verify_process_identity(
    child: &SidecarChild,
    expected_user_sid: &[u8],
    expected_session_id: u32,
) -> Result<(), PipeError> {
    let token = process_token(child.as_raw_handle())?;
    if token_user_sid(token.as_raw_handle())? != expected_user_sid
        || token_session_id(token.as_raw_handle())? != expected_session_id
    {
        return Err(PipeError::Identity);
    }
    Ok(())
}

fn current_process_token() -> Result<OwnedHandle, PipeError> {
    process_token(unsafe { GetCurrentProcess() })
}

fn process_token(process: HANDLE) -> Result<OwnedHandle, PipeError> {
    let mut token = null_mut();
    // SAFETY: The process handle is live and token receives a new owned handle.
    if unsafe { OpenProcessToken(process, TOKEN_QUERY, &mut token) } == 0 {
        return Err(PipeError::Identity);
    }
    // SAFETY: OpenProcessToken returned a new owned handle on success.
    Ok(unsafe { OwnedHandle::from_raw_handle(token) })
}

fn token_session_id(token: HANDLE) -> Result<u32, PipeError> {
    let mut session_id = 0u32;
    let mut returned = 0u32;
    // SAFETY: The token is live and the output buffer is correctly sized.
    if unsafe {
        GetTokenInformation(
            token,
            TokenSessionId,
            (&mut session_id as *mut u32).cast::<c_void>(),
            size_of::<u32>() as u32,
            &mut returned,
        )
    } == 0
        || returned < size_of::<u32>() as u32
    {
        return Err(PipeError::Identity);
    }
    Ok(session_id)
}

fn token_user_sid(token: HANDLE) -> Result<Vec<u8>, PipeError> {
    let mut required = 0u32;
    // SAFETY: A zero-length probe is the documented way to query the required
    // TOKEN_USER buffer size.
    unsafe {
        GetTokenInformation(token, TokenUser, null_mut(), 0, &mut required);
    }
    if required == 0 || unsafe { GetLastError() } != ERROR_INSUFFICIENT_BUFFER {
        return Err(PipeError::Identity);
    }

    let words = (required as usize).div_ceil(size_of::<usize>());
    let mut storage = vec![0usize; words];
    // SAFETY: The storage is pointer-aligned and at least `required` bytes.
    if unsafe {
        GetTokenInformation(
            token,
            TokenUser,
            storage.as_mut_ptr().cast::<c_void>(),
            required,
            &mut required,
        )
    } == 0
    {
        return Err(PipeError::Identity);
    }

    // SAFETY: GetTokenInformation populated a TOKEN_USER in the aligned buffer.
    let token_user = unsafe { &*storage.as_ptr().cast::<TOKEN_USER>() };
    let sid = token_user.User.Sid;
    if sid.is_null() || unsafe { IsValidSid(sid) } == 0 {
        return Err(PipeError::Identity);
    }

    // SAFETY: The SID is valid and its length is reported by the OS.
    let sid_length = unsafe { GetLengthSid(sid) } as usize;
    if sid_length == 0 {
        return Err(PipeError::Identity);
    }
    let mut result = vec![0u8; sid_length];
    // SAFETY: Both buffers are valid for `sid_length` bytes.
    unsafe {
        std::ptr::copy_nonoverlapping(sid.cast::<u8>(), result.as_mut_ptr(), sid_length);
    }
    Ok(result)
}

fn verify_pipe_dacl(pipe: &OwnedHandle, expected_user_sid: &[u8]) -> Result<(), PipeError> {
    let mut required = 0u32;
    // SAFETY: A zero-length probe is the documented way to query the required
    // security descriptor size.
    unsafe {
        GetKernelObjectSecurity(
            pipe.as_raw_handle(),
            DACL_SECURITY_INFORMATION,
            null_mut(),
            0,
            &mut required,
        );
    }
    if required == 0 || unsafe { GetLastError() } != ERROR_INSUFFICIENT_BUFFER {
        return Err(PipeError::Security);
    }

    let words = (required as usize).div_ceil(size_of::<usize>());
    let mut storage = vec![0usize; words];
    // SAFETY: The storage is pointer-aligned and at least `required` bytes.
    if unsafe {
        GetKernelObjectSecurity(
            pipe.as_raw_handle(),
            DACL_SECURITY_INFORMATION,
            storage.as_mut_ptr().cast::<c_void>(),
            required,
            &mut required,
        )
    } == 0
    {
        return Err(PipeError::Security);
    }

    let descriptor = storage.as_mut_ptr().cast::<c_void>();
    let mut present = 0i32;
    let mut dacl = null_mut::<ACL>();
    let mut defaulted = 0i32;
    // SAFETY: `descriptor` contains the self-relative descriptor written above.
    if unsafe { GetSecurityDescriptorDacl(descriptor, &mut present, &mut dacl, &mut defaulted) }
        == 0
        || present == 0
        || defaulted != 0
        || dacl.is_null()
    {
        return Err(PipeError::Security);
    }

    // SAFETY: `dacl` points into the live self-relative security descriptor.
    if unsafe { (*dacl).AceCount } != 1 {
        return Err(PipeError::Security);
    }

    let mut ace = null_mut::<c_void>();
    // SAFETY: The ACL is valid and contains exactly one ACE at index zero.
    if unsafe { GetAce(dacl, 0, &mut ace) } == 0 || ace.is_null() {
        return Err(PipeError::Security);
    }
    // SAFETY: The ACE type is checked immediately after this access.
    let allowed = unsafe { &*ace.cast::<ACCESS_ALLOWED_ACE>() };
    if u32::from(allowed.Header.AceType) != ACCESS_ALLOWED_ACE_TYPE
        || allowed.Header.AceFlags != 0
        || allowed.Mask != PIPE_CLIENT_ACCESS
    {
        return Err(PipeError::Security);
    }

    // SAFETY: SidStart is the first byte of the SID embedded in an
    // ACCESS_ALLOWED_ACE. The ACL and descriptor storage remain live.
    let ace_sid = addr_of!(allowed.SidStart).cast_mut().cast::<c_void>();
    if unsafe { EqualSid(ace_sid, sid_ptr(expected_user_sid)) } == 0 {
        return Err(PipeError::Security);
    }

    Ok(())
}

fn sid_ptr(sid: &[u8]) -> PSID {
    sid.as_ptr().cast_mut().cast::<c_void>()
}

#[cfg(test)]
mod tests {
    use std::fs::File;
    use std::thread;
    use std::time::{Duration, Instant};

    use windows_sys::Win32::Storage::FileSystem::CREATE_ALWAYS;

    use super::*;

    fn test_pipe_path() -> PathBuf {
        let mut random = [0u8; 16];
        getrandom::getrandom(&mut random).expect("random pipe suffix");
        let suffix = random
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        PathBuf::from(format!(r"\\.\pipe\doriya-w0-security-{suffix}"))
    }

    #[test]
    fn named_pipe_dacl_has_only_the_current_user_allow_ace() {
        let user_sid = current_user_sid().expect("current user SID");
        let server =
            NamedPipeServer::create(&test_pipe_path(), &user_sid).expect("restricted pipe DACL");

        drop(server);
    }

    #[test]
    fn process_identity_matches_current_user_and_session() {
        let user_sid = current_user_sid().expect("current user SID");
        let session_id = current_user_session_id().expect("current session");
        let mut child = spawn_identity_probe();

        verify_process_identity(&child, &user_sid, session_id).expect("current process identity");
        let _ = child.kill();
        let _ = child.wait();
    }

    #[test]
    fn process_identity_fails_closed_for_a_mismatched_user_or_session() {
        let user_sid = current_user_sid().expect("current user SID");
        let session_id = current_user_session_id().expect("current session");
        let mut wrong_sid = user_sid.clone();
        let mut child = spawn_identity_probe();
        let last = wrong_sid.last_mut().expect("SID bytes");
        *last ^= 0x01;

        assert_eq!(
            verify_process_identity(&child, &wrong_sid, session_id),
            Err(PipeError::Identity)
        );
        assert_eq!(
            verify_process_identity(&child, &user_sid, session_id.wrapping_add(1)),
            Err(PipeError::Identity)
        );
        let _ = child.kill();
        let _ = child.wait();
    }

    #[test]
    fn process_identity_requires_the_original_creation_time() {
        let mut child = spawn_identity_probe();
        let creation_time = child.creation_time().expect("child creation time");

        assert!(process_identity_matches(child.id(), creation_time).expect("match child identity"));
        assert!(
            !process_identity_matches(child.id(), creation_time.wrapping_add(1))
                .expect("compare mismatched creation time")
        );

        let _ = child.kill();
        let _ = child.wait();
    }

    #[test]
    fn only_the_preconnected_client_handle_can_use_the_single_instance_pipe() {
        let user_sid = current_user_sid().expect("current user SID");
        let server =
            NamedPipeServer::create(&test_pipe_path(), &user_sid).expect("create restricted pipe");
        let client = server
            .connect_client()
            .expect("connect the sole inherited client handle");
        assert!(!client.as_raw_handle().is_null());
        assert!(
            server.connect_client().is_err(),
            "a second client must not attach to the single-instance pipe"
        );
    }

    #[test]
    fn an_expired_connect_deadline_fails_before_the_handshake() {
        let user_sid = current_user_sid().expect("current user SID");
        let server =
            NamedPipeServer::create(&test_pipe_path(), &user_sid).expect("create restricted pipe");

        assert!(matches!(
            server.connect_client_until(Instant::now()),
            Err(PipeError::Connect)
        ));
    }

    #[test]
    fn job_object_has_kill_on_close_and_contains_assigned_child() {
        let job = JobObject::new_kill_on_close().expect("create job object");
        let mut child = SidecarChild::from(
            std::process::Command::new("cmd.exe")
                .args(["/D", "/C", "ping", "127.0.0.1", "-n", "30", ">NUL"])
                .spawn()
                .expect("spawn job test child"),
        );

        job.ensure_assigned(&child).expect("assign child to job");
        assert!(job.kill_on_close_enabled().expect("query job limits"));
        assert!(job.contains(&child).expect("query job membership"));

        drop(job);
        let deadline = Instant::now() + Duration::from_secs(5);
        let exited = loop {
            if child.try_wait().expect("poll job test child").is_some() {
                break true;
            }
            if Instant::now() >= deadline {
                break false;
            }
            thread::sleep(Duration::from_millis(20));
        };

        if !exited {
            let _ = child.kill();
        }
        let _ = child.wait();
        assert!(exited, "closing the job handle must terminate its child");
    }

    #[test]
    fn restricted_spawn_does_not_inherit_unlisted_handles() {
        let mut random = [0u8; 8];
        getrandom::getrandom(&mut random).expect("random handle token");
        let token = random
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        let inherited_path =
            PathBuf::from("target").join(format!("doriya-w0-inherited-handle-{token}.tmp"));
        let attributes = SECURITY_ATTRIBUTES {
            nLength: size_of::<SECURITY_ATTRIBUTES>() as u32,
            lpSecurityDescriptor: null_mut(),
            bInheritHandle: 1,
        };
        let raw_file = unsafe {
            CreateFileW(
                wide_null(inherited_path.as_os_str()).as_ptr(),
                FILE_GENERIC_READ | FILE_GENERIC_WRITE,
                FILE_SHARE_READ | FILE_SHARE_WRITE,
                &attributes,
                CREATE_ALWAYS,
                0,
                null_mut(),
            )
        };
        assert_ne!(raw_file, INVALID_HANDLE_VALUE, "create inheritable file");
        let inherited_file = unsafe { OwnedHandle::from_raw_handle(raw_file) };

        let stdin = File::open("NUL").expect("open NUL as test stdin");

        let script = format!(
            "Add-Type -TypeDefinition 'using System; using System.Runtime.InteropServices; \
             using System.Text; \
             public static class H {{ [DllImport(\"kernel32.dll\", SetLastError=true)] \
             public static extern uint GetFinalPathNameByHandle(IntPtr h, StringBuilder p, \
             uint n, uint f); }}'; $p=New-Object System.Text.StringBuilder 1024; \
             $n=[H]::GetFinalPathNameByHandle([IntPtr]{}, $p, 1024, 0); \
             if ($n -gt 0 -and $p.ToString().Contains('{token}')) {{ exit 1 }} else {{ exit 0 }}",
            inherited_file.as_raw_handle() as usize
        );
        let environment = std::env::vars_os().collect::<Vec<_>>();
        let job = JobObject::new_kill_on_close().expect("create handle-test job");
        let mut child = SidecarChild::spawn_restricted(
            OsStr::new("powershell.exe"),
            &[
                OsString::from("-NoProfile"),
                OsString::from("-NonInteractive"),
                OsString::from("-Command"),
                OsString::from(script),
            ],
            &environment,
            stdin,
            &job,
        )
        .expect("spawn restricted handle probe");

        let status = child.wait().expect("wait for restricted handle probe");
        drop(inherited_file);
        let _ = std::fs::remove_file(inherited_path);
        assert_eq!(
            status.code(),
            Some(0),
            "the unlisted inheritable event must not reach the child"
        );
    }

    #[test]
    fn restricted_child_accepts_the_still_active_exit_code_value() {
        let stdin = File::open("NUL").expect("open NUL as test stdin");
        let environment = std::env::vars_os().collect::<Vec<_>>();
        let job = JobObject::new_kill_on_close().expect("create exit-code-test job");
        let mut child = SidecarChild::spawn_restricted(
            OsStr::new("cmd.exe"),
            &[
                OsString::from("/D"),
                OsString::from("/C"),
                OsString::from("exit 259"),
            ],
            &environment,
            stdin,
            &job,
        )
        .expect("spawn restricted exit-code probe");

        let status = child.wait().expect("wait for restricted exit-code probe");
        assert_eq!(status.code(), Some(259));
    }

    fn spawn_identity_probe() -> SidecarChild {
        SidecarChild::from(
            std::process::Command::new("cmd.exe")
                .args(["/D", "/C", "ping", "127.0.0.1", "-n", "30", ">NUL"])
                .spawn()
                .expect("spawn identity test child"),
        )
    }
}
