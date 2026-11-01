use std::io;
use std::mem::size_of;
use std::net::{Ipv4Addr, SocketAddr, SocketAddrV4};
use std::ptr::{null_mut, read_unaligned};

use windows_sys::Win32::Foundation::{ERROR_INSUFFICIENT_BUFFER, ERROR_SUCCESS};
use windows_sys::Win32::NetworkManagement::IpHelper::{
    GetExtendedTcpTable, MIB_TCPROW_OWNER_PID, MIB_TCP_STATE_ESTAB, TCP_TABLE_OWNER_PID_ALL,
};
use windows_sys::Win32::Networking::WinSock::AF_INET;

const MAX_TCP_TABLE_BYTES: usize = 64 * 1024 * 1024;
const MAX_QUERY_ATTEMPTS: usize = 3;

// Returns the owner of the client-side ESTABLISHED row. Windows lists both
// endpoints of a loopback connection, and the accepted relay socket's peer is
// the client endpoint whose PID must match the verified mpv sidecar.
pub(crate) fn tcp_connection_owner_pid(
    client: SocketAddr,
    server: SocketAddr,
) -> io::Result<Option<u32>> {
    let client = ipv4_endpoint(client)?;
    let server = ipv4_endpoint(server)?;
    let (storage, byte_len) = query_tcp_table()?;
    find_owner(&storage, byte_len, client, server)
}

fn ipv4_endpoint(address: SocketAddr) -> io::Result<SocketAddrV4> {
    match address {
        SocketAddr::V4(address) => Ok(address),
        SocketAddr::V6(_) => Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "relay process ownership requires an IPv4 endpoint",
        )),
    }
}

fn query_tcp_table() -> io::Result<(Vec<u32>, usize)> {
    let mut last_error = None;
    for _ in 0..MAX_QUERY_ATTEMPTS {
        let mut required = 0u32;
        // SAFETY: A null buffer with a zero size is the documented sizing
        // query. `required` is a valid output pointer.
        let result = unsafe {
            GetExtendedTcpTable(
                null_mut(),
                &mut required,
                0,
                AF_INET as u32,
                TCP_TABLE_OWNER_PID_ALL,
                0,
            )
        };
        if result == ERROR_SUCCESS && required == 0 {
            return Ok((Vec::new(), 0));
        }
        if result != ERROR_INSUFFICIENT_BUFFER {
            return Err(io::Error::from_raw_os_error(result as i32));
        }
        if required == 0 || required as usize > MAX_TCP_TABLE_BYTES {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "Windows TCP table returned an invalid size",
            ));
        }

        let words = (required as usize).div_ceil(size_of::<u32>());
        let mut storage = vec![0u32; words];
        let mut actual = required;
        // SAFETY: `storage` is aligned for the MIB table and has at least the
        // number of bytes requested by the sizing query. `actual` reports the
        // bytes written.
        let result = unsafe {
            GetExtendedTcpTable(
                storage.as_mut_ptr().cast(),
                &mut actual,
                0,
                AF_INET as u32,
                TCP_TABLE_OWNER_PID_ALL,
                0,
            )
        };
        if result == ERROR_SUCCESS {
            if actual as usize > storage.len() * size_of::<u32>() {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "Windows TCP table reported more bytes than it wrote",
                ));
            }
            return Ok((storage, actual as usize));
        }
        if result != ERROR_INSUFFICIENT_BUFFER {
            return Err(io::Error::from_raw_os_error(result as i32));
        }
        last_error = Some(io::Error::new(
            io::ErrorKind::WouldBlock,
            "Windows TCP table changed while it was being read",
        ));
    }

    Err(last_error.unwrap_or_else(|| io::Error::other("Windows TCP table query failed")))
}

fn find_owner(
    storage: &[u32],
    byte_len: usize,
    client: SocketAddrV4,
    server: SocketAddrV4,
) -> io::Result<Option<u32>> {
    if byte_len == 0 {
        return Ok(None);
    }
    if byte_len < size_of::<u32>() || storage.is_empty() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "Windows TCP table is missing its header",
        ));
    }

    // SAFETY: `storage` has at least four bytes and starts with the native
    // `dwNumEntries` field of MIB_TCPTABLE_OWNER_PID.
    let count = unsafe { read_unaligned(storage.as_ptr().cast::<u32>()) } as usize;
    let entry_size = size_of::<MIB_TCPROW_OWNER_PID>();
    let table_bytes = count
        .checked_mul(entry_size)
        .and_then(|bytes| bytes.checked_add(size_of::<u32>()))
        .ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                "Windows TCP table entry count overflowed",
            )
        })?;
    if table_bytes > byte_len {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "Windows TCP table entry count exceeds the returned buffer",
        ));
    }

    let mut owner = None;
    let base = storage.as_ptr().cast::<u8>();
    for index in 0..count {
        let offset = size_of::<u32>() + index * entry_size;
        // SAFETY: The table length was checked above, and read_unaligned does
        // not require the row to have the alignment of MIB_TCPROW_OWNER_PID.
        let row = unsafe { read_unaligned(base.add(offset).cast::<MIB_TCPROW_OWNER_PID>()) };
        if row.dwState != MIB_TCP_STATE_ESTAB as u32 {
            continue;
        }

        let Some(local_port) = network_port(row.dwLocalPort) else {
            continue;
        };
        let Some(remote_port) = network_port(row.dwRemotePort) else {
            continue;
        };
        let row_local = SocketAddrV4::new(network_ipv4(row.dwLocalAddr), local_port);
        let row_remote = SocketAddrV4::new(network_ipv4(row.dwRemoteAddr), remote_port);
        if row_local != client || row_remote != server {
            continue;
        }

        match owner {
            Some(existing) if existing != row.dwOwningPid => {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "Windows TCP table reported conflicting owners",
                ));
            }
            Some(_) => {}
            None => owner = Some(row.dwOwningPid),
        }
    }
    Ok(owner)
}

fn network_ipv4(value: u32) -> Ipv4Addr {
    Ipv4Addr::from(value.to_be())
}

fn network_port(value: u32) -> Option<u16> {
    let port = u16::from_be(value as u16);
    (port != 0).then_some(port)
}

#[cfg(test)]
mod tests {
    use std::net::{TcpListener, TcpStream};

    use super::*;

    #[test]
    fn finds_the_client_process_for_an_established_loopback_connection() {
        let listener = TcpListener::bind((Ipv4Addr::LOCALHOST, 0)).expect("bind listener");
        let _client = TcpStream::connect(listener.local_addr().expect("listener address"))
            .expect("connect client");
        let (server, peer) = listener.accept().expect("accept client");

        let owner =
            tcp_connection_owner_pid(peer, server.local_addr().expect("accepted server address"))
                .expect("query TCP owner")
                .expect("client-side TCP row");

        assert_eq!(owner, std::process::id());
    }

    #[test]
    fn decodes_network_byte_order_addresses_and_ports() {
        assert_eq!(network_ipv4(0x0100_007f), Ipv4Addr::LOCALHOST);
        assert_eq!(network_port(0x0000_1500), Some(21));
        assert_eq!(network_port(0), None);
    }
}
