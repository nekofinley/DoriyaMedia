use std::collections::{HashMap, HashSet};
use std::io::{self, ErrorKind, Read, Write};
#[cfg(windows)]
use std::mem::size_of;
use std::net::{IpAddr, SocketAddr, TcpListener, TcpStream, ToSocketAddrs};
#[cfg(windows)]
use std::os::windows::io::AsRawSocket;
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::sync::{Arc, Condvar, Mutex, RwLock};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

#[cfg(windows)]
use socket2::{Domain, Protocol, Socket, Type};
use url::{Host, Url};
#[cfg(windows)]
use windows_sys::Win32::Networking::WinSock::{
    setsockopt, WSAGetLastError, SOL_SOCKET, SO_EXCLUSIVEADDRUSE,
};

use super::network::{is_public_ip, normalize_public_dns_name};
#[cfg(windows)]
use super::windows_security::process_identity_matches;
#[cfg(windows)]
use super::windows_tcp_owner::tcp_connection_owner_pid;

const MAX_CONNECT_HEADER_BYTES: usize = 8 * 1024;
const MAX_CONNECT_HEADER_COUNT: usize = 32;
const MAX_RELAY_CONNECTIONS: usize = 16;
const MAX_SESSION_CONNECTIONS: usize = 512;
const CONNECT_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(5);
const UPSTREAM_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const TUNNEL_WRITE_TIMEOUT: Duration = Duration::from_secs(5);
const TUNNEL_POLL_INTERVAL: Duration = Duration::from_millis(500);
const RELAY_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(2);
const OWNER_LOOKUP_ATTEMPTS: usize = 5;
const OWNER_LOOKUP_RETRY_INTERVAL: Duration = Duration::from_millis(10);

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct RelayOrigin {
    host: String,
    port: u16,
}

impl RelayOrigin {
    fn from_https_url(url: &Url) -> Option<Self> {
        if url.scheme() != "https" {
            return None;
        }
        let host = match url.host()? {
            Host::Domain(host) => normalize_public_dns_name(host)?,
            Host::Ipv4(address) => address.to_string(),
            Host::Ipv6(address) => address.to_string(),
        };
        let port = url.port_or_known_default()?;
        (port != 0).then_some(Self { host, port })
    }
}

#[derive(Clone, Copy)]
enum TargetMode {
    Public,
    #[cfg(test)]
    LoopbackFixture,
}

#[derive(Clone)]
struct RelayState {
    origin: Option<RelayOrigin>,
    generation: u64,
    target_mode: TargetMode,
    pin: Option<Arc<PinCell>>,
    owner: Option<ProcessBinding>,
}

#[derive(Clone, Copy, Eq, PartialEq)]
struct ProcessBinding {
    pid: u32,
    creation_time: u64,
}

#[derive(Default)]
struct PinInner {
    resolving: bool,
    result: Option<Result<Vec<SocketAddr>, ()>>,
}

#[derive(Default)]
struct PinCell {
    inner: Mutex<PinInner>,
    ready: Condvar,
}

impl PinCell {
    fn resolve(
        &self,
        host: &str,
        port: u16,
        target_mode: TargetMode,
        resolve: impl FnOnce(&str, u16) -> io::Result<Vec<SocketAddr>>,
    ) -> Result<Vec<SocketAddr>, ()> {
        let mut inner = self.inner.lock().map_err(|_| ())?;
        loop {
            if let Some(result) = inner.result.as_ref() {
                return result.clone();
            }
            if inner.resolving {
                inner = self.ready.wait(inner).map_err(|_| ())?;
                continue;
            }
            inner.resolving = true;
            drop(inner);

            let result = resolve(host, port)
                .map_err(|_| ())
                .and_then(|addresses| validate_resolved_addresses(host, addresses, target_mode));

            let mut inner = self.inner.lock().map_err(|_| ())?;
            inner.resolving = false;
            inner.result = Some(result.clone());
            self.ready.notify_all();
            return result;
        }
    }
}

pub(crate) struct RelayProxy {
    address: SocketAddr,
    state: Arc<RwLock<RelayState>>,
    shutdown: Arc<AtomicBool>,
    active_connections: Arc<AtomicUsize>,
    session_connections: Arc<AtomicUsize>,
    #[cfg(test)]
    established_tunnels: Arc<AtomicUsize>,
    active_tunnels: Arc<Mutex<HashMap<u64, Vec<TcpStream>>>>,
    #[cfg(test)]
    raw_connections: Arc<AtomicUsize>,
    accept_thread: Option<JoinHandle<()>>,
}

impl RelayProxy {
    pub(crate) fn start() -> io::Result<Self> {
        Self::start_with_mode(TargetMode::Public)
    }

    fn proxy_url(&self) -> String {
        format!("http://{}", self.address)
    }

    fn shutdown_tunnels(&self) {
        if let Ok(mut tunnels) = self.active_tunnels.lock() {
            for streams in tunnels.values() {
                for stream in streams {
                    let _ = stream.shutdown(std::net::Shutdown::Both);
                }
            }
            tunnels.clear();
        }
    }

    pub(crate) fn begin_playback(&self, origin: RelayOrigin) -> Result<String, ()> {
        let mut state = self.state.write().map_err(|_| ())?;
        state.owner.ok_or(())?;
        state.generation = state.generation.wrapping_add(1);
        state.origin = Some(origin);
        state.pin = Some(Arc::new(PinCell::default()));
        self.session_connections.store(0, Ordering::SeqCst);
        self.shutdown_tunnels();
        Ok(self.proxy_url())
    }

    pub(crate) fn bind_process(&self, pid: u32, creation_time: u64) -> Result<(), ()> {
        if pid == 0 || creation_time == 0 {
            return Err(());
        }
        let mut state = self.state.write().map_err(|_| ())?;
        let candidate = ProcessBinding { pid, creation_time };
        match state.owner {
            Some(existing) if existing != candidate => Err(()),
            Some(_) => Ok(()),
            None => {
                state.owner = Some(candidate);
                Ok(())
            }
        }
    }

    pub(crate) fn disarm(&self) {
        if let Ok(mut state) = self.state.write() {
            state.generation = state.generation.wrapping_add(1);
            state.origin = None;
            state.pin = None;
            state.owner = None;
        }
        self.shutdown_tunnels();
    }

    pub(crate) fn stop(&mut self) {
        self.disarm();
        self.shutdown.store(true, Ordering::SeqCst);
        if let Some(accept_thread) = self.accept_thread.take() {
            let _ = accept_thread.join();
        }

        let deadline = Instant::now() + RELAY_SHUTDOWN_TIMEOUT;
        while self.active_connections.load(Ordering::SeqCst) != 0 && Instant::now() < deadline {
            thread::sleep(Duration::from_millis(20));
        }
    }

    fn start_with_mode(target_mode: TargetMode) -> io::Result<Self> {
        let listener = bind_loopback_listener()?;
        let address = listener.local_addr()?;
        if !address.ip().is_loopback() || address.port() == 0 {
            return Err(io::Error::new(
                ErrorKind::AddrNotAvailable,
                "relay did not bind to an IPv4 loopback port",
            ));
        }

        let state = Arc::new(RwLock::new(RelayState {
            origin: None,
            generation: 0,
            target_mode,
            pin: None,
            owner: None,
        }));
        let shutdown = Arc::new(AtomicBool::new(false));
        let active_connections = Arc::new(AtomicUsize::new(0));
        let session_connections = Arc::new(AtomicUsize::new(0));
        #[cfg(test)]
        let established_tunnels = Arc::new(AtomicUsize::new(0));
        let active_tunnels = Arc::new(Mutex::new(HashMap::new()));
        let next_tunnel_id = Arc::new(AtomicU64::new(1));
        #[cfg(test)]
        let raw_connections = Arc::new(AtomicUsize::new(0));

        let accept_context = AcceptContext {
            state: Arc::clone(&state),
            shutdown: Arc::clone(&shutdown),
            active_connections: Arc::clone(&active_connections),
            session_connections: Arc::clone(&session_connections),
            #[cfg(test)]
            established_tunnels: Arc::clone(&established_tunnels),
            active_tunnels: Arc::clone(&active_tunnels),
            next_tunnel_id: Arc::clone(&next_tunnel_id),
            #[cfg(test)]
            raw_connections: Arc::clone(&raw_connections),
        };
        let accept_thread = thread::spawn(move || accept_loop(listener, accept_context));

        Ok(Self {
            address,
            state,
            shutdown,
            active_connections,
            session_connections,
            #[cfg(test)]
            established_tunnels,
            active_tunnels,
            #[cfg(test)]
            raw_connections,
            accept_thread: Some(accept_thread),
        })
    }

    #[cfg(test)]
    fn start_loopback_fixture(origin: RelayOrigin) -> io::Result<Self> {
        let relay = Self::start_with_mode(TargetMode::LoopbackFixture)?;
        #[cfg(windows)]
        let creation_time = super::windows_security::current_process_creation_time()
            .map_err(|_| io::Error::other("relay process identity query failed"))?;
        #[cfg(not(windows))]
        let creation_time = 1;
        relay
            .bind_process(std::process::id(), creation_time)
            .map_err(|_| io::Error::other("relay process binding failed"))?;
        relay
            .begin_playback(origin)
            .map_err(|_| io::Error::other("relay state lock was poisoned"))?;
        Ok(relay)
    }

    #[cfg(test)]
    fn address(&self) -> SocketAddr {
        self.address
    }

    #[cfg(test)]
    pub(crate) fn session_connection_count(&self) -> usize {
        self.session_connections.load(Ordering::SeqCst)
    }

    #[cfg(test)]
    pub(crate) fn raw_connection_count(&self) -> usize {
        self.raw_connections.load(Ordering::SeqCst)
    }

    #[cfg(test)]
    pub(crate) fn established_tunnel_count(&self) -> usize {
        self.established_tunnels.load(Ordering::SeqCst)
    }
}

fn bind_loopback_listener() -> io::Result<TcpListener> {
    #[cfg(windows)]
    {
        let socket = Socket::new(Domain::IPV4, Type::STREAM, Some(Protocol::TCP))?;
        let exclusive = 1u32;
        // SAFETY: The socket is live, and `exclusive` remains valid for the
        // duration of the setsockopt call.
        let result = unsafe {
            setsockopt(
                socket.as_raw_socket() as usize,
                SOL_SOCKET,
                SO_EXCLUSIVEADDRUSE,
                (&exclusive as *const u32).cast(),
                size_of::<u32>() as i32,
            )
        };
        if result != 0 {
            // SAFETY: WSAGetLastError reads the calling thread's Winsock error.
            return Err(io::Error::from_raw_os_error(unsafe { WSAGetLastError() }));
        }
        socket.bind(&SocketAddr::from((std::net::Ipv4Addr::LOCALHOST, 0)).into())?;
        socket.listen(128)?;
        socket.set_nonblocking(true)?;
        Ok(socket.into())
    }

    #[cfg(not(windows))]
    {
        let listener = TcpListener::bind((std::net::Ipv4Addr::LOCALHOST, 0))?;
        listener.set_nonblocking(true)?;
        Ok(listener)
    }
}

impl Drop for RelayProxy {
    fn drop(&mut self) {
        self.stop();
    }
}

pub(crate) fn origin_for_media(url: &Url) -> Option<RelayOrigin> {
    RelayOrigin::from_https_url(url)
}

struct AcceptContext {
    state: Arc<RwLock<RelayState>>,
    shutdown: Arc<AtomicBool>,
    active_connections: Arc<AtomicUsize>,
    session_connections: Arc<AtomicUsize>,
    #[cfg(test)]
    established_tunnels: Arc<AtomicUsize>,
    active_tunnels: Arc<Mutex<HashMap<u64, Vec<TcpStream>>>>,
    next_tunnel_id: Arc<AtomicU64>,
    #[cfg(test)]
    raw_connections: Arc<AtomicUsize>,
}

fn accept_loop(listener: TcpListener, context: AcceptContext) {
    let AcceptContext {
        state,
        shutdown,
        active_connections,
        session_connections,
        #[cfg(test)]
        established_tunnels,
        active_tunnels,
        next_tunnel_id,
        #[cfg(test)]
        raw_connections,
    } = context;
    while !shutdown.load(Ordering::SeqCst) {
        match listener.accept() {
            Ok((stream, _)) => {
                #[cfg(test)]
                raw_connections.fetch_add(1, Ordering::SeqCst);
                if active_connections.fetch_add(1, Ordering::SeqCst) >= MAX_RELAY_CONNECTIONS {
                    active_connections.fetch_sub(1, Ordering::SeqCst);
                    let _ = write_response(&stream, "503 Service Unavailable");
                    continue;
                }

                let active_guard = ActiveConnection::new(Arc::clone(&active_connections));
                let state = Arc::clone(&state);
                let shutdown = Arc::clone(&shutdown);
                let session_connections = Arc::clone(&session_connections);
                #[cfg(test)]
                let established_tunnels = Arc::clone(&established_tunnels);
                let active_tunnels = Arc::clone(&active_tunnels);
                let next_tunnel_id = Arc::clone(&next_tunnel_id);
                thread::spawn(move || {
                    let _guard = active_guard;
                    handle_client(
                        stream,
                        state,
                        shutdown,
                        session_connections,
                        #[cfg(test)]
                        established_tunnels,
                        active_tunnels,
                        next_tunnel_id,
                    );
                });
            }
            Err(error) if error.kind() == ErrorKind::WouldBlock => {
                thread::sleep(Duration::from_millis(20));
            }
            Err(error) if error.kind() == ErrorKind::Interrupted => {}
            Err(_) => thread::sleep(Duration::from_millis(20)),
        }
    }
}

struct ActiveConnection {
    active: Arc<AtomicUsize>,
}

impl ActiveConnection {
    fn new(active: Arc<AtomicUsize>) -> Self {
        Self { active }
    }
}

impl Drop for ActiveConnection {
    fn drop(&mut self) {
        self.active.fetch_sub(1, Ordering::SeqCst);
    }
}

fn validate_connection_owner(
    client: &TcpStream,
    state: &Arc<RwLock<RelayState>>,
) -> Result<(), &'static str> {
    let expected = state
        .read()
        .map_err(|_| "503 Service Unavailable")?
        .owner
        .ok_or("403 Forbidden")?;
    let server = client.local_addr().map_err(|_| "503 Service Unavailable")?;
    let peer = client.peer_addr().map_err(|_| "503 Service Unavailable")?;

    #[cfg(windows)]
    {
        let mut last_error = None;
        for attempt in 0..OWNER_LOOKUP_ATTEMPTS {
            match tcp_connection_owner_pid(peer, server) {
                Ok(Some(owner_pid)) if owner_pid == expected.pid => {
                    match process_identity_matches(expected.pid, expected.creation_time) {
                        Ok(true) => return Ok(()),
                        Ok(false) => return Err("403 Forbidden"),
                        Err(_) => return Err("503 Service Unavailable"),
                    }
                }
                Ok(Some(_)) => return Err("403 Forbidden"),
                Ok(None) => {
                    last_error = None;
                }
                Err(error) => {
                    last_error = Some(error);
                }
            }
            if attempt + 1 < OWNER_LOOKUP_ATTEMPTS {
                thread::sleep(OWNER_LOOKUP_RETRY_INTERVAL);
            }
        }
        if last_error.is_some() {
            Err("503 Service Unavailable")
        } else {
            Err("403 Forbidden")
        }
    }

    #[cfg(not(windows))]
    {
        let _ = (peer, server, expected);
        Err("503 Service Unavailable")
    }
}

fn handle_client(
    mut client: TcpStream,
    state: Arc<RwLock<RelayState>>,
    shutdown: Arc<AtomicBool>,
    session_connections: Arc<AtomicUsize>,
    #[cfg(test)] established_tunnels: Arc<AtomicUsize>,
    active_tunnels: Arc<Mutex<HashMap<u64, Vec<TcpStream>>>>,
    next_tunnel_id: Arc<AtomicU64>,
) {
    let _ = client.set_nodelay(true);
    if validate_connection_owner(&client, &state).is_err() {
        let _ = client.shutdown(std::net::Shutdown::Both);
        return;
    }

    let header_deadline = Instant::now() + CONNECT_HANDSHAKE_TIMEOUT;
    let (header, extra) = match read_connect_header(&mut client, header_deadline) {
        Ok(value) => value,
        Err(status) => {
            let _ = write_response(&client, status);
            return;
        }
    };
    let proxy_authority = client.local_addr().ok().and_then(relay_origin_from_socket);
    let request = match parse_connect_request(&header, proxy_authority.as_ref()) {
        Ok(request) => request,
        Err(status) => {
            let _ = write_response(&client, status);
            return;
        }
    };
    if session_connections.fetch_add(1, Ordering::SeqCst) >= MAX_SESSION_CONNECTIONS {
        session_connections.fetch_sub(1, Ordering::SeqCst);
        let _ = write_response(&client, "503 Service Unavailable");
        return;
    }

    let (origin, generation, pin, target_mode) = match state.read() {
        Ok(relay_state) => {
            let Some(origin) = relay_state.origin.clone() else {
                let _ = write_response(&client, "403 Forbidden");
                return;
            };
            if request.authority.host != origin.host || request.authority.port != origin.port {
                let _ = write_response(&client, "403 Forbidden");
                return;
            }
            let Some(pin) = relay_state.pin.clone() else {
                let _ = write_response(&client, "503 Service Unavailable");
                return;
            };
            (origin, relay_state.generation, pin, relay_state.target_mode)
        }
        Err(_) => {
            let _ = write_response(&client, "503 Service Unavailable");
            return;
        }
    };
    let addresses = match pin.resolve(
        &origin.host,
        origin.port,
        target_mode,
        resolve_with_deadline,
    ) {
        Ok(addresses) => addresses,
        Err(()) => {
            let _ = write_response(&client, "403 Forbidden");
            return;
        }
    };
    if !generation_is_current(&state, generation) || shutdown.load(Ordering::SeqCst) {
        return;
    }
    let upstream = match connect_first_allowed(&addresses) {
        Ok(stream) => stream,
        Err(()) => {
            let _ = write_response(&client, "502 Bad Gateway");
            return;
        }
    };
    let mut upstream = upstream;
    if !generation_is_current(&state, generation) || shutdown.load(Ordering::SeqCst) {
        return;
    }
    let tunnel = match TunnelGuard::register(&active_tunnels, &next_tunnel_id, &client, &upstream) {
        Ok(tunnel) => tunnel,
        Err(_) => {
            let _ = write_response(&client, "503 Service Unavailable");
            return;
        }
    };
    if !generation_is_current(&state, generation) || shutdown.load(Ordering::SeqCst) {
        return;
    }
    if write_response(&client, "200 Connection Established").is_err() {
        return;
    }
    #[cfg(test)]
    established_tunnels.fetch_add(1, Ordering::SeqCst);
    if !extra.is_empty() && upstream.write_all(&extra).is_err() {
        return;
    }

    if client.set_read_timeout(Some(TUNNEL_POLL_INTERVAL)).is_err()
        || upstream
            .set_read_timeout(Some(TUNNEL_POLL_INTERVAL))
            .is_err()
        || client
            .set_write_timeout(Some(TUNNEL_WRITE_TIMEOUT))
            .is_err()
        || upstream
            .set_write_timeout(Some(TUNNEL_WRITE_TIMEOUT))
            .is_err()
    {
        return;
    }

    let tunnel_done = Arc::new(AtomicBool::new(false));
    let upstream_to_client = match upstream.try_clone() {
        Ok(stream) => stream,
        Err(_) => return,
    };
    let client_to_upstream = match client.try_clone() {
        Ok(stream) => stream,
        Err(_) => return,
    };
    let upstream_reader_done = Arc::clone(&tunnel_done);
    let upstream_reader_shutdown = Arc::clone(&shutdown);
    let upstream_reader_state = Arc::clone(&state);
    let reader = match thread::Builder::new()
        .name("doriya-relay-upstream".to_owned())
        .spawn(move || {
            pump(
                upstream_to_client,
                client_to_upstream,
                upstream_reader_done,
                upstream_reader_shutdown,
                upstream_reader_state,
                generation,
            )
        }) {
        Ok(handle) => handle,
        Err(_) => return,
    };

    pump(
        client,
        upstream,
        Arc::clone(&tunnel_done),
        shutdown,
        state,
        generation,
    );
    tunnel_done.store(true, Ordering::SeqCst);
    let _ = reader.join();
    drop(tunnel);
}

fn resolve_with_deadline(host: &str, port: u16) -> io::Result<Vec<SocketAddr>> {
    let host = host.to_owned();
    let (sender, receiver) = mpsc::sync_channel(1);
    thread::Builder::new()
        .name("doriya-relay-dns".to_owned())
        .spawn(move || {
            let result = (host.as_str(), port)
                .to_socket_addrs()
                .map(|addresses| addresses.collect());
            let _ = sender.send(result);
        })
        .map_err(|_| io::Error::other("relay DNS worker could not start"))?;
    match receiver.recv_timeout(UPSTREAM_CONNECT_TIMEOUT) {
        Ok(result) => result,
        Err(RecvTimeoutError::Timeout) => Err(io::Error::new(
            ErrorKind::TimedOut,
            "relay DNS resolution timed out",
        )),
        Err(RecvTimeoutError::Disconnected) => Err(io::Error::other(
            "relay DNS worker stopped without a result",
        )),
    }
}

fn generation_is_current(state: &Arc<RwLock<RelayState>>, generation: u64) -> bool {
    state
        .read()
        .is_ok_and(|current| current.generation == generation && current.origin.is_some())
}

struct TunnelGuard {
    id: u64,
    tunnels: Arc<Mutex<HashMap<u64, Vec<TcpStream>>>>,
}

impl TunnelGuard {
    fn register(
        tunnels: &Arc<Mutex<HashMap<u64, Vec<TcpStream>>>>,
        next_id: &AtomicU64,
        client: &TcpStream,
        upstream: &TcpStream,
    ) -> io::Result<Self> {
        let id = next_id.fetch_add(1, Ordering::SeqCst);
        let streams = vec![client.try_clone()?, upstream.try_clone()?];
        tunnels
            .lock()
            .map_err(|_| io::Error::other("relay tunnel registry is poisoned"))?
            .insert(id, streams);
        Ok(Self {
            id,
            tunnels: Arc::clone(tunnels),
        })
    }
}

impl Drop for TunnelGuard {
    fn drop(&mut self) {
        if let Ok(mut tunnels) = self.tunnels.lock() {
            tunnels.remove(&self.id);
        }
    }
}

fn pump(
    mut source: TcpStream,
    mut destination: TcpStream,
    done: Arc<AtomicBool>,
    shutdown: Arc<AtomicBool>,
    state: Arc<RwLock<RelayState>>,
    generation: u64,
) {
    let mut buffer = [0u8; 64 * 1024];
    loop {
        if done.load(Ordering::SeqCst) || shutdown.load(Ordering::SeqCst) {
            break;
        }
        let state_is_current = state
            .read()
            .is_ok_and(|current| current.generation == generation);
        if !state_is_current {
            break;
        }

        match source.read(&mut buffer) {
            Ok(0) => break,
            Ok(read) => {
                if destination.write_all(&buffer[..read]).is_err() {
                    break;
                }
            }
            Err(error) if matches!(error.kind(), ErrorKind::WouldBlock | ErrorKind::TimedOut) => {}
            Err(_) => break,
        }
    }

    done.store(true, Ordering::SeqCst);
    let _ = destination.shutdown(std::net::Shutdown::Write);
}

fn read_connect_header(
    stream: &mut TcpStream,
    deadline: Instant,
) -> Result<(Vec<u8>, Vec<u8>), &'static str> {
    let mut header = Vec::with_capacity(1024);
    let mut buffer = [0u8; 1024];
    loop {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero()
            || stream
                .set_read_timeout(Some(remaining.max(Duration::from_millis(1))))
                .is_err()
        {
            return Err("400 Bad Request");
        }
        let read = match stream.read(&mut buffer) {
            Ok(0) | Err(_) => return Err("400 Bad Request"),
            Ok(read) => read,
        };
        header.extend_from_slice(&buffer[..read]);
        if header.len() > MAX_CONNECT_HEADER_BYTES {
            return Err("400 Bad Request");
        }
        if let Some(end) = find_header_end(&header) {
            let extra = header.split_off(end);
            return Ok((header, extra));
        }
    }
}

fn find_header_end(bytes: &[u8]) -> Option<usize> {
    bytes
        .windows(4)
        .position(|window| window == b"\r\n\r\n")
        .map(|position| position + 4)
}

struct ConnectRequest {
    authority: RelayOrigin,
}

fn parse_connect_request(
    header: &[u8],
    proxy_authority: Option<&RelayOrigin>,
) -> Result<ConnectRequest, &'static str> {
    if header.len() > MAX_CONNECT_HEADER_BYTES || !header.ends_with(b"\r\n\r\n") {
        return Err("400 Bad Request");
    }
    let header = std::str::from_utf8(header).map_err(|_| "400 Bad Request")?;
    let header = header.strip_suffix("\r\n\r\n").ok_or("400 Bad Request")?;
    let mut lines = header.split("\r\n");
    let request_line = lines.next().ok_or("400 Bad Request")?;
    let mut parts = request_line.split(' ');
    let method = parts.next().ok_or("400 Bad Request")?;
    let target = parts.next().ok_or("400 Bad Request")?;
    let version = parts.next().ok_or("400 Bad Request")?;
    if method != "CONNECT" || target.is_empty() || parts.next().is_some() || version != "HTTP/1.1" {
        return Err("405 Method Not Allowed");
    }
    let authority = parse_authority(target).ok_or("400 Bad Request")?;

    let mut header_count = 0usize;
    let mut host_header = None;
    let mut content_length_seen = false;
    let mut transfer_encoding_seen = false;
    for line in lines {
        if line.is_empty() || line.starts_with([' ', '\t']) {
            return Err("400 Bad Request");
        }
        header_count += 1;
        if header_count > MAX_CONNECT_HEADER_COUNT {
            return Err("400 Bad Request");
        }
        let (name, value) = line.split_once(':').ok_or("400 Bad Request")?;
        if name.is_empty() || !name.bytes().all(is_http_token_byte) {
            return Err("400 Bad Request");
        }
        let value = value.trim_matches([' ', '\t']);

        if name.eq_ignore_ascii_case("host") {
            if host_header.replace(value.to_owned()).is_some() {
                return Err("400 Bad Request");
            }
        } else if name.eq_ignore_ascii_case("content-length") {
            if content_length_seen || value != "0" {
                return Err("400 Bad Request");
            }
            content_length_seen = true;
        } else if name.eq_ignore_ascii_case("transfer-encoding") {
            transfer_encoding_seen = true;
        }
    }
    if transfer_encoding_seen {
        return Err("400 Bad Request");
    }
    let host_header = host_header.ok_or("400 Bad Request")?;
    let host_authority = parse_authority(&host_header);
    if host_authority.as_ref() != Some(&authority)
        && !proxy_authority.is_some_and(|proxy| host_authority.as_ref() == Some(proxy))
    {
        return Err("400 Bad Request");
    }

    Ok(ConnectRequest { authority })
}

fn relay_origin_from_socket(address: SocketAddr) -> Option<RelayOrigin> {
    match address {
        SocketAddr::V4(address) => Some(RelayOrigin {
            host: address.ip().to_string(),
            port: address.port(),
        }),
        SocketAddr::V6(_) => None,
    }
}

fn parse_authority(value: &str) -> Option<RelayOrigin> {
    if value.is_empty()
        || value.len() > 512
        || value
            .bytes()
            .any(|byte| byte.is_ascii_control() || byte.is_ascii_whitespace())
        || value.contains(['@', '/', '\\', '?', '#'])
    {
        return None;
    }

    let (host, port, bracketed) = if let Some(rest) = value.strip_prefix('[') {
        let end = rest.find(']')?;
        let host = &rest[..end];
        let suffix = rest.get(end + 1..)?;
        let port = suffix.strip_prefix(':')?;
        (host, port, true)
    } else {
        let (host, port) = value.rsplit_once(':')?;
        if host.contains(':') {
            return None;
        }
        (host, port, false)
    };

    if port.is_empty() || !port.bytes().all(|byte| byte.is_ascii_digit()) {
        return None;
    }
    let port = port.parse::<u16>().ok()?;
    if port == 0 {
        return None;
    }
    let host = if bracketed {
        match host.parse::<IpAddr>() {
            Ok(IpAddr::V6(address)) => address.to_string(),
            _ => return None,
        }
    } else {
        normalize_connect_host(host)?
    };
    Some(RelayOrigin { host, port })
}

fn normalize_connect_host(host: &str) -> Option<String> {
    if host.is_empty() || host.len() > 253 {
        return None;
    }
    if let Ok(address) = host.parse::<IpAddr>() {
        return Some(address.to_string());
    }
    normalize_public_dns_name(host)
}

#[cfg(test)]
fn resolve_allowed_addresses_with(
    host: &str,
    port: u16,
    target_mode: TargetMode,
    resolve: impl FnOnce(&str, u16) -> io::Result<Vec<SocketAddr>>,
) -> Result<Vec<SocketAddr>, ()> {
    let addresses = if let Ok(address) = host.parse::<IpAddr>() {
        vec![SocketAddr::new(address, port)]
    } else {
        resolve(host, port).map_err(|_| ())?
    };
    validate_resolved_addresses(host, addresses, target_mode)
}

fn validate_resolved_addresses(
    host: &str,
    addresses: Vec<SocketAddr>,
    target_mode: TargetMode,
) -> Result<Vec<SocketAddr>, ()> {
    if addresses.is_empty() {
        return Err(());
    }

    let mut seen = HashSet::with_capacity(addresses.len());
    let mut allowed = Vec::with_capacity(addresses.len());
    for address in addresses {
        if !seen.insert(address.ip()) {
            continue;
        }
        let is_allowed = match target_mode {
            TargetMode::Public => {
                let _ = host;
                is_public_ip(address.ip())
            }
            #[cfg(test)]
            TargetMode::LoopbackFixture => {
                host == "127.0.0.1" && address.ip() == IpAddr::V4(std::net::Ipv4Addr::LOCALHOST)
            }
        };
        if !is_allowed {
            return Err(());
        }
        allowed.push(address);
    }
    if allowed.is_empty() {
        Err(())
    } else {
        Ok(allowed)
    }
}

fn connect_first_allowed(addresses: &[SocketAddr]) -> Result<TcpStream, ()> {
    let deadline = Instant::now() + UPSTREAM_CONNECT_TIMEOUT;
    for address in addresses {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            break;
        }
        if let Ok(stream) = TcpStream::connect_timeout(address, remaining) {
            if stream.peer_addr().ok() != Some(*address) {
                continue;
            }
            let _ = stream.set_nodelay(true);
            return Ok(stream);
        }
    }
    Err(())
}

fn write_response(mut stream: &TcpStream, status: &'static str) -> io::Result<()> {
    let mut response = format!("HTTP/1.1 {status}\r\nConnection: close\r\n");
    response.push_str("Content-Length: 0\r\n\r\n");
    stream.write_all(response.as_bytes())
}

fn is_http_token_byte(byte: u8) -> bool {
    byte.is_ascii_alphanumeric()
        || matches!(
            byte,
            b'!' | b'#'
                | b'$'
                | b'%'
                | b'&'
                | b'\''
                | b'*'
                | b'+'
                | b'-'
                | b'.'
                | b'^'
                | b'_'
                | b'`'
                | b'|'
                | b'~'
        )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn origin(host: &str, port: u16) -> RelayOrigin {
        RelayOrigin {
            host: host.to_owned(),
            port,
        }
    }

    fn connect_request(target: &str) -> Vec<u8> {
        format!("CONNECT {target} HTTP/1.1\r\nHost: {target}\r\n\r\n").into_bytes()
    }

    fn read_response(stream: &mut TcpStream) -> String {
        let mut response = Vec::new();
        let mut byte = [0u8; 1];
        while response.len() < 4096 {
            stream.read_exact(&mut byte).expect("read response");
            response.push(byte[0]);
            if response.ends_with(b"\r\n\r\n") {
                break;
            }
        }
        String::from_utf8(response).expect("ASCII response")
    }

    #[test]
    fn parses_exact_connect_authority() {
        let parsed = parse_connect_request(
            b"CONNECT Media.Example.test.:443 HTTP/1.1\r\nHost: Media.Example.test.:443\r\n\r\n",
            None,
        )
        .expect("valid CONNECT");

        assert_eq!(parsed.authority, origin("media.example.test", 443));
    }

    #[test]
    fn accepts_ffmpeg_host_header_for_the_exact_proxy_authority() {
        let proxy = origin("127.0.0.1", 49152);
        let parsed = parse_connect_request(
            b"CONNECT example.com:443 HTTP/1.1\r\nHost: 127.0.0.1:49152\r\nConnection: close\r\n\r\n",
            Some(&proxy),
        )
        .expect("FFmpeg-style CONNECT");

        assert_eq!(parsed.authority, origin("example.com", 443));
    }

    #[test]
    fn rejects_ambiguous_connect_requests() {
        for request in [
            b"GET https://media.example.test/ HTTP/1.1\r\n\r\n".as_slice(),
            b"CONNECT https://media.example.test:443 HTTP/1.1\r\nHost: media.example.test:443\r\n\r\n",
            b"CONNECT user@example.test:443 HTTP/1.1\r\nHost: user@example.test:443\r\n\r\n",
            b"CONNECT example.test:0 HTTP/1.1\r\nHost: example.test:0\r\n\r\n",
            b"CONNECT example.test:443 HTTP/1.0\r\nHost: example.test:443\r\n\r\n",
            b"CONNECT example.test:443 HTTP/1.1\r\nHost: other.example.test:443\r\n\r\n",
            b"CONNECT under_score.example:443 HTTP/1.1\r\nHost: under_score.example:443\r\n\r\n",
            b"CONNECT single-label:443 HTTP/1.1\r\nHost: single-label:443\r\n\r\n",
            b"CONNECT [example.test]:443 HTTP/1.1\r\nHost: [example.test]:443\r\n\r\n",
            b"CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\nHost: other.example.test:443\r\n\r\n",
            b"CONNECT example.test:443 HTTP/1.1\r\n\r\n",
            b"CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\nTransfer-Encoding: chunked\r\n\r\n",
            b"CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\nContent-Length: 3\r\n\r\n",
        ] {
            assert!(parse_connect_request(request, None).is_err());
        }
    }

    #[test]
    fn rejects_private_or_mixed_dns_answers_before_connect() {
        let mixed = vec![
            "8.8.8.8:443".parse().expect("public fixture"),
            "127.0.0.1:443".parse().expect("private fixture"),
        ];
        assert!(
            validate_resolved_addresses("media.example.test", mixed, TargetMode::Public).is_err()
        );

        for address in [
            "10.0.0.1:443",
            "100.64.0.1:443",
            "127.0.0.1:443",
            "169.254.169.254:443",
            "192.168.1.1:443",
            "192.0.2.1:443",
            "[64:ff9b::a00:1]:443",
            "[2001:db8::1]:443",
            "[fd00::1]:443",
        ] {
            let result = validate_resolved_addresses(
                "media.example.test",
                vec![address.parse().expect("address fixture")],
                TargetMode::Public,
            );
            assert!(result.is_err(), "{address} must be rejected");
        }

        assert!(validate_resolved_addresses(
            "media.example.test",
            vec!["8.8.8.8:443".parse().expect("public fixture")],
            TargetMode::Public,
        )
        .is_ok());
    }

    #[test]
    fn resolves_each_connect_exactly_once_and_does_not_retry_resolution() {
        let calls = std::cell::Cell::new(0usize);
        let result = resolve_allowed_addresses_with(
            "media.example.test",
            443,
            TargetMode::Public,
            |_, _| {
                calls.set(calls.get() + 1);
                Ok(vec![
                    "203.0.113.1:443".parse().expect("documentation fixture"),
                    "8.8.8.8:443".parse().expect("public fixture"),
                ])
            },
        );

        assert!(result.is_err());
        assert_eq!(calls.get(), 1);
    }

    #[test]
    fn pin_cell_reuses_the_first_validated_resolution() {
        let calls = std::cell::Cell::new(0usize);
        let pin = PinCell::default();

        let first = pin
            .resolve("media.example.test", 443, TargetMode::Public, |_, _| {
                calls.set(calls.get() + 1);
                Ok(vec!["8.8.8.8:443".parse().expect("public fixture")])
            })
            .expect("first resolution");
        let second = pin
            .resolve("media.example.test", 443, TargetMode::Public, |_, _| {
                calls.set(calls.get() + 1);
                Err(io::Error::other("resolver must not run twice"))
            })
            .expect("session pin");

        assert_eq!(first, second);
        assert_eq!(calls.get(), 1);
    }

    #[cfg(windows)]
    #[test]
    fn tunnel_requires_the_bound_process_and_exact_origin() {
        let upstream = TcpListener::bind(("127.0.0.1", 0)).expect("bind upstream");
        let upstream_address = upstream.local_addr().expect("upstream address");
        let mut relay =
            RelayProxy::start_loopback_fixture(origin("127.0.0.1", upstream_address.port()))
                .expect("start relay");

        let mut wrong_origin = TcpStream::connect(relay.address()).expect("connect to relay");
        wrong_origin
            .write_all(&connect_request("example.test:443"))
            .expect("write wrong-origin request");
        assert!(read_response(&mut wrong_origin).starts_with("HTTP/1.1 403"));

        let upstream_thread = thread::spawn(move || {
            let (mut stream, _) = upstream.accept().expect("accept upstream");
            let mut request = [0u8; 4];
            stream.read_exact(&mut request).expect("read tunneled data");
            assert_eq!(&request, b"PING");
            stream.write_all(b"PONG").expect("write tunneled response");
        });

        let mut client = TcpStream::connect(relay.address()).expect("connect to relay");
        client
            .set_read_timeout(Some(Duration::from_secs(5)))
            .expect("client timeout");
        client
            .write_all(&connect_request(&format!(
                "127.0.0.1:{}",
                upstream_address.port()
            )))
            .expect("write CONNECT");
        assert!(read_response(&mut client).starts_with("HTTP/1.1 200"));
        client.write_all(b"PING").expect("write tunneled data");
        let mut response = [0u8; 4];
        client
            .read_exact(&mut response)
            .expect("read tunneled response");
        assert_eq!(&response, b"PONG");
        upstream_thread.join().expect("join upstream");
        relay.stop();
    }

    #[cfg(windows)]
    #[test]
    fn starting_a_new_playback_shuts_down_the_previous_tunnel() {
        let upstream = TcpListener::bind(("127.0.0.1", 0)).expect("bind upstream");
        let upstream_address = upstream.local_addr().expect("upstream address");
        let relay =
            RelayProxy::start_loopback_fixture(origin("127.0.0.1", upstream_address.port()))
                .expect("start relay");
        let mut client = TcpStream::connect(relay.address()).expect("connect to relay");
        client
            .set_read_timeout(Some(Duration::from_secs(2)))
            .expect("client timeout");
        client
            .write_all(&connect_request(&format!(
                "127.0.0.1:{}",
                upstream_address.port()
            )))
            .expect("write CONNECT");
        assert!(read_response(&mut client).starts_with("HTTP/1.1 200"));
        let (_upstream_stream, _) = upstream.accept().expect("accept upstream");

        relay
            .begin_playback(origin("127.0.0.1", upstream_address.port()))
            .expect("start next playback");

        let mut byte = [0u8; 1];
        let outcome = client.read(&mut byte);
        assert!(
            !matches!(
                outcome,
                Err(error) if matches!(error.kind(), ErrorKind::WouldBlock | ErrorKind::TimedOut)
            ),
            "the previous tunnel must be closed when a new playback starts"
        );
    }

    #[test]
    fn relay_binds_only_to_loopback_and_requires_a_process_binding() {
        let first = RelayProxy::start().expect("start first relay");
        let second = RelayProxy::start().expect("start second relay");
        assert!(
            first
                .begin_playback(origin("media.example.test", 443))
                .is_err(),
            "an unbound relay must not open a playback session"
        );
        first
            .bind_process(
                std::process::id(),
                crate::player::windows_security::current_process_creation_time()
                    .expect("first process creation time"),
            )
            .expect("bind first relay process");
        second
            .bind_process(
                std::process::id(),
                crate::player::windows_security::current_process_creation_time()
                    .expect("second process creation time"),
            )
            .expect("bind second relay process");
        let first_url = first
            .begin_playback(origin("media.example.test", 443))
            .expect("first playback capability");
        let second_url = second
            .begin_playback(origin("media.example.test", 443))
            .expect("second playback capability");
        assert!(first.address().ip().is_loopback());
        assert!(second.address().ip().is_loopback());
        assert_ne!(first.address().port(), second.address().port());
        assert_eq!(first_url, format!("http://{}", first.address()));
        assert_eq!(second_url, format!("http://{}", second.address()));
        assert!(!first_url.contains('@'));
        assert!(!second_url.contains('@'));
    }

    #[test]
    fn process_binding_cannot_change_the_creation_time() {
        let relay = RelayProxy::start().expect("start relay");
        let pid = std::process::id();
        let creation_time = crate::player::windows_security::current_process_creation_time()
            .expect("current process creation time");
        relay
            .bind_process(pid, creation_time)
            .expect("bind process identity");
        assert!(relay
            .bind_process(pid, creation_time.wrapping_add(1))
            .is_err());
    }

    #[cfg(windows)]
    #[test]
    fn relay_listener_cannot_be_rebound_with_reuseaddr() {
        let relay = RelayProxy::start().expect("start relay");
        let hijack = Socket::new(Domain::IPV4, Type::STREAM, Some(Protocol::TCP))
            .expect("create hijack socket");
        hijack
            .set_reuse_address(true)
            .expect("enable reuseaddr on hijack socket");

        assert!(
            hijack.bind(&relay.address().into()).is_err(),
            "SO_EXCLUSIVEADDRUSE must prevent a same-port bind"
        );
    }

    #[cfg(windows)]
    #[test]
    fn relay_rejects_an_established_connection_owned_by_another_pid() {
        let upstream = TcpListener::bind(("127.0.0.1", 0)).expect("bind upstream");
        let upstream_address = upstream.local_addr().expect("upstream address");
        let relay =
            RelayProxy::start_with_mode(TargetMode::LoopbackFixture).expect("start unbound relay");
        relay
            .bind_process(
                std::process::id().wrapping_add(1).max(1),
                crate::player::windows_security::current_process_creation_time()
                    .expect("current process creation time"),
            )
            .expect("bind a different process PID");
        relay
            .begin_playback(origin("127.0.0.1", upstream_address.port()))
            .expect("start playback session");

        let mut client = TcpStream::connect(relay.address()).expect("connect to relay");
        client
            .write_all(&connect_request(&format!(
                "127.0.0.1:{}",
                upstream_address.port()
            )))
            .expect("write CONNECT");
        client
            .set_read_timeout(Some(Duration::from_secs(2)))
            .expect("set rejection read timeout");
        let mut byte = [0u8; 1];
        let outcome = client.read(&mut byte);
        assert!(
            !matches!(outcome, Ok(1) if byte[0] == b'H'),
            "a connection owned by another PID must not receive an HTTP response"
        );
    }

    #[cfg(windows)]
    #[test]
    fn relay_rejects_a_bound_pid_with_a_different_creation_time() {
        let upstream = TcpListener::bind(("127.0.0.1", 0)).expect("bind upstream");
        let upstream_address = upstream.local_addr().expect("upstream address");
        let relay =
            RelayProxy::start_with_mode(TargetMode::LoopbackFixture).expect("start unbound relay");
        let creation_time = crate::player::windows_security::current_process_creation_time()
            .expect("current process creation time");
        relay
            .bind_process(std::process::id(), creation_time.wrapping_add(1))
            .expect("bind mismatched process creation time");
        relay
            .begin_playback(origin("127.0.0.1", upstream_address.port()))
            .expect("start playback session");

        let mut client = TcpStream::connect(relay.address()).expect("connect to relay");
        client
            .write_all(&connect_request(&format!(
                "127.0.0.1:{}",
                upstream_address.port()
            )))
            .expect("write CONNECT");
        client
            .set_read_timeout(Some(Duration::from_secs(2)))
            .expect("set rejection read timeout");
        let mut byte = [0u8; 1];
        let outcome = client.read(&mut byte);
        assert!(
            !matches!(outcome, Ok(1) if byte[0] == b'H'),
            "a matching PID with a different creation time must not receive an HTTP response"
        );
    }
}
