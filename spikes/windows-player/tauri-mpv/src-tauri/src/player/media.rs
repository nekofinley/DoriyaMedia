use std::collections::{BTreeMap, HashSet};
use std::env;

use serde::Deserialize;
use url::{Host, Url};

const MAX_URL_BYTES: usize = 8 * 1024;
const MAX_HEADER_COUNT: usize = 8;
const MAX_HEADER_VALUE_BYTES: usize = 2 * 1024;
const MAX_HEADER_TOTAL_BYTES: usize = 8 * 1024;

const ALLOWED_HEADERS: [(&str, &str); 5] = [
    ("accept", "Accept"),
    ("accept-language", "Accept-Language"),
    ("origin", "Origin"),
    ("referer", "Referer"),
    ("user-agent", "User-Agent"),
];

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct MediaLoadRequest {
    pub url: String,
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
}

pub struct ValidatedMedia {
    url: Url,
    headers: Vec<ValidatedHeader>,
}

impl ValidatedMedia {
    pub fn url(&self) -> &str {
        self.url.as_str()
    }

    pub fn url_scheme(&self) -> &str {
        self.url.scheme()
    }

    pub fn header_fields(&self) -> Vec<String> {
        self.headers
            .iter()
            .map(|header| format!("{}: {}", header.name, header.value))
            .collect()
    }
}

struct ValidatedHeader {
    name: &'static str,
    value: String,
}

#[derive(Clone, Copy)]
pub struct MediaPolicy {
    allow_loopback_http: bool,
}

impl MediaPolicy {
    pub fn from_environment() -> Self {
        Self {
            allow_loopback_http: env::var("DORIYA_W0_ALLOW_LOOPBACK_HTTP")
                .is_ok_and(|value| value == "1"),
        }
    }

    #[cfg(test)]
    fn strict() -> Self {
        Self {
            allow_loopback_http: false,
        }
    }

    #[cfg(test)]
    fn allow_loopback_http() -> Self {
        Self {
            allow_loopback_http: true,
        }
    }

    pub fn allows_loopback_http(self) -> bool {
        self.allow_loopback_http
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum MediaError {
    UrlRequired,
    UrlTooLong,
    UrlInvalid,
    UrlUserInfo,
    UrlFragment,
    UrlScheme,
    UrlInsecure,
    HeaderNotAllowed,
    HeaderDuplicate,
    HeaderValueInvalid,
    HeaderTooLarge,
}

impl MediaError {
    pub const fn code(self) -> &'static str {
        match self {
            Self::UrlRequired => "media_url_required",
            Self::UrlTooLong => "media_url_too_long",
            Self::UrlInvalid => "media_url_invalid",
            Self::UrlUserInfo => "media_url_userinfo",
            Self::UrlFragment => "media_url_fragment",
            Self::UrlScheme => "media_url_scheme",
            Self::UrlInsecure => "media_url_insecure",
            Self::HeaderNotAllowed => "header_not_allowed",
            Self::HeaderDuplicate => "header_duplicate",
            Self::HeaderValueInvalid => "header_value_invalid",
            Self::HeaderTooLarge => "header_too_large",
        }
    }

    pub const fn message(self) -> &'static str {
        match self {
            Self::UrlRequired => "A media URL is required.",
            Self::UrlTooLong => "The media URL exceeds the configured limit.",
            Self::UrlInvalid => "The media URL is invalid.",
            Self::UrlUserInfo => "Media URLs must not contain user information.",
            Self::UrlFragment => "Media URLs must not contain a fragment.",
            Self::UrlScheme => "Only HTTPS media URLs are allowed.",
            Self::UrlInsecure => "Plain HTTP is limited to loopback test mode.",
            Self::HeaderNotAllowed => "A header is not on the playback allowlist.",
            Self::HeaderDuplicate => "A header was supplied more than once.",
            Self::HeaderValueInvalid => "A header value is malformed.",
            Self::HeaderTooLarge => "The supplied headers exceed the configured limit.",
        }
    }
}

pub fn validate_media(
    request: MediaLoadRequest,
    policy: MediaPolicy,
) -> Result<ValidatedMedia, MediaError> {
    validate_url(&request.url, policy)?;
    let headers = validate_headers(request.headers, policy)?;

    Ok(ValidatedMedia {
        url: Url::parse(&request.url).map_err(|_| MediaError::UrlInvalid)?,
        headers,
    })
}

pub fn request_from_environment() -> Result<MediaLoadRequest, MediaError> {
    let url = env::var("DORIYA_W0_MEDIA_URL").map_err(|_| MediaError::UrlRequired)?;
    let mut headers = BTreeMap::new();

    for (variable, header) in [
        ("DORIYA_W0_MEDIA_REFERER", "Referer"),
        ("DORIYA_W0_MEDIA_USER_AGENT", "User-Agent"),
        ("DORIYA_W0_MEDIA_ORIGIN", "Origin"),
    ] {
        if let Ok(value) = env::var(variable) {
            if !value.is_empty() {
                headers.insert(header.to_owned(), value);
            }
        }
    }

    Ok(MediaLoadRequest { url, headers })
}

pub fn environment_header_count() -> usize {
    [
        "DORIYA_W0_MEDIA_REFERER",
        "DORIYA_W0_MEDIA_USER_AGENT",
        "DORIYA_W0_MEDIA_ORIGIN",
    ]
    .iter()
    .filter(|variable| env::var(variable).is_ok_and(|value| !value.is_empty()))
    .count()
}

fn validate_url(input: &str, policy: MediaPolicy) -> Result<(), MediaError> {
    if input.trim().is_empty() {
        return Err(MediaError::UrlRequired);
    }
    if input.len() > MAX_URL_BYTES {
        return Err(MediaError::UrlTooLong);
    }
    if input.trim() != input {
        return Err(MediaError::UrlInvalid);
    }

    let url = Url::parse(input).map_err(|_| MediaError::UrlInvalid)?;
    if !url.username().is_empty() || url.password().is_some() {
        return Err(MediaError::UrlUserInfo);
    }
    if url.fragment().is_some() {
        return Err(MediaError::UrlFragment);
    }

    match url.scheme() {
        "https" if url.host().is_some() => Ok(()),
        "https" => Err(MediaError::UrlInvalid),
        "http" if url.host().is_some() && policy.allow_loopback_http && is_loopback_host(&url) => {
            Ok(())
        }
        "http" if url.host().is_some() => Err(MediaError::UrlInsecure),
        "http" => Err(MediaError::UrlInvalid),
        _ => Err(MediaError::UrlScheme),
    }
}

fn is_loopback_host(url: &Url) -> bool {
    match url.host() {
        Some(Host::Domain(host)) => host.eq_ignore_ascii_case("localhost"),
        Some(Host::Ipv4(address)) => address.is_loopback(),
        Some(Host::Ipv6(address)) => address.is_loopback(),
        None => false,
    }
}

fn validate_headers(
    headers: BTreeMap<String, String>,
    policy: MediaPolicy,
) -> Result<Vec<ValidatedHeader>, MediaError> {
    if headers.len() > MAX_HEADER_COUNT {
        return Err(MediaError::HeaderTooLarge);
    }

    let mut total_bytes = 0usize;
    let mut seen = HashSet::with_capacity(headers.len());
    let mut validated = Vec::with_capacity(headers.len());

    for (name, value) in headers {
        let canonical = ALLOWED_HEADERS
            .iter()
            .find_map(|(lowercase, canonical)| {
                name.eq_ignore_ascii_case(lowercase).then_some(*canonical)
            })
            .ok_or(MediaError::HeaderNotAllowed)?;

        if !seen.insert(canonical) {
            return Err(MediaError::HeaderDuplicate);
        }
        validate_header_value(&value)?;
        if canonical == "Origin" || canonical == "Referer" {
            validate_url(&value, policy).map_err(|_| MediaError::HeaderValueInvalid)?;
        }

        total_bytes = total_bytes
            .checked_add(name.len() + value.len())
            .ok_or(MediaError::HeaderTooLarge)?;
        if total_bytes > MAX_HEADER_TOTAL_BYTES {
            return Err(MediaError::HeaderTooLarge);
        }

        validated.push(ValidatedHeader {
            name: canonical,
            value,
        });
    }

    Ok(validated)
}

fn validate_header_value(value: &str) -> Result<(), MediaError> {
    if value.is_empty() || value.len() > MAX_HEADER_VALUE_BYTES || value.trim() != value {
        return Err(MediaError::HeaderValueInvalid);
    }
    if !value.bytes().all(|byte| (0x20..=0x7e).contains(&byte)) {
        return Err(MediaError::HeaderValueInvalid);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request(url: &str, headers: &[(&str, &str)]) -> MediaLoadRequest {
        MediaLoadRequest {
            url: url.to_owned(),
            headers: headers
                .iter()
                .map(|(name, value)| ((*name).to_owned(), (*value).to_owned()))
                .collect(),
        }
    }

    #[test]
    fn accepts_https_and_allowlisted_headers() {
        let media = validate_media(
            request(
                "https://media.example.invalid/file.mkv?expires=123",
                &[
                    ("Referer", "https://www.example.invalid/"),
                    ("User-Agent", "Doriya-W0/0.1"),
                    ("Origin", "https://www.example.invalid"),
                ],
            ),
            MediaPolicy::strict(),
        )
        .expect("valid request");

        assert_eq!(
            media.url(),
            "https://media.example.invalid/file.mkv?expires=123"
        );
        assert_eq!(
            media.header_fields(),
            vec![
                "Origin: https://www.example.invalid".to_owned(),
                "Referer: https://www.example.invalid/".to_owned(),
                "User-Agent: Doriya-W0/0.1".to_owned(),
            ]
        );
    }

    #[test]
    fn rejects_non_https_and_credentialed_urls() {
        let cases = [
            (
                "http://media.example.invalid/file.mkv",
                MediaError::UrlInsecure,
            ),
            ("file:///C:/secret.mkv", MediaError::UrlScheme),
            (
                "https://user:password@media.example.invalid/file.mkv",
                MediaError::UrlUserInfo,
            ),
            (
                "https://media.example.invalid/file.mkv#fragment",
                MediaError::UrlFragment,
            ),
            ("not a url", MediaError::UrlInvalid),
        ];

        for (url, expected) in cases {
            let error = validate_media(request(url, &[]), MediaPolicy::strict())
                .err()
                .expect("URL must be rejected");
            assert_eq!(error, expected);
        }
    }

    #[test]
    fn permits_loopback_http_only_with_explicit_policy() {
        let url = "http://127.0.0.1:19090/media.mkv";
        assert_eq!(
            validate_media(request(url, &[]), MediaPolicy::strict())
                .err()
                .expect("strict policy must reject"),
            MediaError::UrlInsecure
        );
        assert!(validate_media(request(url, &[]), MediaPolicy::allow_loopback_http()).is_ok());
        assert_eq!(
            validate_media(
                request("http://192.0.2.1/media.mkv", &[]),
                MediaPolicy::allow_loopback_http()
            )
            .err()
            .expect("non-loopback HTTP must stay blocked"),
            MediaError::UrlInsecure
        );
    }

    #[test]
    fn rejects_headers_outside_the_allowlist_and_injection() {
        let cases = [
            (vec![("Cookie", "SID=secret")], MediaError::HeaderNotAllowed),
            (
                vec![("User-Agent", "safe\r\nX-Injected: yes")],
                MediaError::HeaderValueInvalid,
            ),
            (
                vec![("Referer", "javascript:alert(1)")],
                MediaError::HeaderValueInvalid,
            ),
            (
                vec![("User-Agent", " trailing")],
                MediaError::HeaderValueInvalid,
            ),
        ];

        for (headers, expected) in cases {
            let error = validate_media(
                request("https://media.example.invalid/file.mkv", &headers),
                MediaPolicy::strict(),
            )
            .err()
            .expect("header must be rejected");
            assert_eq!(error, expected);
        }
    }

    #[test]
    fn rejects_case_insensitive_duplicate_headers() {
        let error = validate_media(
            request(
                "https://media.example.invalid/file.mkv",
                &[
                    ("Referer", "https://example.invalid/"),
                    ("referer", "https://example.invalid/"),
                ],
            ),
            MediaPolicy::strict(),
        )
        .err()
        .expect("duplicate header must be rejected");

        assert_eq!(error, MediaError::HeaderDuplicate);
    }
}
