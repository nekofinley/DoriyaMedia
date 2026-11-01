use std::net::{Ipv4Addr, Ipv6Addr};

pub(crate) fn is_public_ip(address: std::net::IpAddr) -> bool {
    match address {
        std::net::IpAddr::V4(address) => is_public_ipv4(address),
        std::net::IpAddr::V6(address) => is_public_ipv6(address),
    }
}

pub(crate) fn normalize_public_dns_name(host: &str) -> Option<String> {
    let host = host.trim_end_matches('.').to_ascii_lowercase();
    if host.is_empty()
        || host.len() > 253
        || !host.contains('.')
        || host == "localhost"
        || host.ends_with(".localhost")
    {
        return None;
    }

    for label in host.split('.') {
        if label.is_empty()
            || label.len() > 63
            || label.starts_with('-')
            || label.ends_with('-')
            || !label
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        {
            return None;
        }
    }
    Some(host)
}

pub(crate) fn is_public_ipv4(address: Ipv4Addr) -> bool {
    let [a, b, c, _] = address.octets();
    !(a == 0
        || a == 10
        || a == 127
        || (a == 100 && (64..=127).contains(&b))
        || (a == 169 && b == 254)
        || (a == 172 && (16..=31).contains(&b))
        || (a == 192 && b == 0 && c == 0)
        || (a == 192 && b == 0 && c == 2)
        || (a == 192 && b == 88 && c == 99)
        || (a == 192 && b == 168)
        || (a == 198 && (b == 18 || b == 19))
        || (a == 198 && b == 51 && c == 100)
        || (a == 203 && b == 0 && c == 113)
        || a >= 224)
}

pub(crate) fn is_public_ipv6(address: Ipv6Addr) -> bool {
    const GLOBAL_UNICAST_ALLOCATIONS: &[(u16, u16, u8)] = &[
        (0x2001, 0x0200, 23),
        (0x2001, 0x0400, 23),
        (0x2001, 0x0600, 23),
        (0x2001, 0x0800, 22),
        (0x2001, 0x0c00, 23),
        (0x2001, 0x0e00, 23),
        (0x2001, 0x1200, 23),
        (0x2001, 0x1400, 22),
        (0x2001, 0x1800, 23),
        (0x2001, 0x1a00, 23),
        (0x2001, 0x1c00, 22),
        (0x2001, 0x2000, 19),
        (0x2001, 0x4000, 23),
        (0x2001, 0x4200, 23),
        (0x2001, 0x4400, 23),
        (0x2001, 0x4600, 23),
        (0x2001, 0x4800, 23),
        (0x2001, 0x4a00, 23),
        (0x2001, 0x4c00, 23),
        (0x2001, 0x5000, 20),
        (0x2001, 0x8000, 19),
        (0x2001, 0xa000, 20),
        (0x2001, 0xb000, 20),
        (0x2003, 0x0000, 18),
        (0x2400, 0x0000, 12),
        (0x2410, 0x0000, 12),
        (0x2600, 0x0000, 12),
        (0x2610, 0x0000, 23),
        (0x2620, 0x0000, 23),
        (0x2630, 0x0000, 12),
        (0x2800, 0x0000, 12),
        (0x2a00, 0x0000, 12),
        (0x2a10, 0x0000, 12),
        (0x2c00, 0x0000, 12),
    ];

    let segments = address.segments();
    GLOBAL_UNICAST_ALLOCATIONS
        .iter()
        .any(|&(prefix, base, length)| {
            prefix_matches(segments, prefix, base, length)
                && !(segments[0] == 0x2001 && segments[1] == 0x0db8)
        })
}

fn prefix_matches(segments: [u16; 8], prefix: u16, base: u16, length: u8) -> bool {
    let first_remaining_bits = length.min(16);
    let first_mask = u16::MAX << (16 - first_remaining_bits);
    if (segments[0] & first_mask) != (prefix & first_mask) {
        return false;
    }
    if length <= 16 {
        return true;
    }
    let full_segments = usize::from(length / 16);
    let remaining_bits = length % 16;
    for segment in segments.iter().take(full_segments).skip(1) {
        if *segment != base {
            return false;
        }
    }

    if remaining_bits == 0 {
        return true;
    }

    let mask = u16::MAX << (16 - remaining_bits);
    (segments[full_segments] & mask) == (base & mask)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_special_ipv4_ranges() {
        for address in [
            "0.0.0.0",
            "10.1.2.3",
            "100.64.0.1",
            "127.0.0.1",
            "169.254.169.254",
            "172.16.0.1",
            "192.0.0.1",
            "192.0.2.1",
            "192.88.99.1",
            "192.168.1.1",
            "198.18.0.1",
            "198.51.100.1",
            "203.0.113.1",
            "224.0.0.1",
            "255.255.255.255",
        ] {
            assert!(
                !is_public_ipv4(address.parse().expect("IPv4 fixture")),
                "{address} must be rejected"
            );
        }

        for address in ["1.1.1.1", "8.8.8.8", "93.184.216.34"] {
            assert!(
                is_public_ipv4(address.parse().expect("IPv4 fixture")),
                "{address} must be accepted"
            );
        }
    }

    #[test]
    fn uses_exact_iana_ipv6_global_unicast_allocations() {
        for (address, expected) in [
            ("2001:200::1", true),
            ("2001:4860:4860::8888", true),
            ("2400::1", true),
            ("2606:4700:4700::1111", true),
            ("2a00:1450:4001::1", true),
            ("2001::1", false),
            ("2001:db8::1", false),
            ("2002::1", false),
            ("240f:ffff::1", true),
            ("24ff::1", false),
            ("2d00::1", false),
            ("3000::1", false),
            ("64:ff9b::a00:1", false),
            ("64:ff9b:1::a00:1", false),
            ("::ffff:10.0.0.1", false),
            ("fd00::1", false),
            ("fe80::1", false),
            ("ff02::1", false),
        ] {
            assert_eq!(
                is_public_ipv6(address.parse().expect("IPv6 fixture")),
                expected,
                "unexpected result for {address}"
            );
        }
    }

    #[test]
    fn validates_public_dns_names() {
        for (host, expected) in [
            ("media.example.com", Some("media.example.com")),
            ("MEDIA.EXAMPLE.COM.", Some("media.example.com")),
            ("xn--bcher-kva.example", Some("xn--bcher-kva.example")),
            ("localhost", None),
            ("media.localhost", None),
            ("single-label", None),
            ("under_score.example", None),
            ("-leading.example", None),
            ("trailing-.example", None),
            ("double..example", None),
            (".leading.example", None),
        ] {
            assert_eq!(
                normalize_public_dns_name(host).as_deref(),
                expected,
                "unexpected result for {host}"
            );
        }
    }
}
