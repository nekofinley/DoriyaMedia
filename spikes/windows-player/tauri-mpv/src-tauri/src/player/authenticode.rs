use std::ffi::c_void;
use std::fs::File;
use std::os::windows::ffi::OsStrExt;
use std::os::windows::io::AsRawHandle;
use std::path::Path;
use std::ptr::null_mut;

use windows_sys::Win32::Foundation::{
    CERT_E_CHAINING, CERT_E_REVOKED, CERT_E_UNTRUSTEDROOT, HANDLE, INVALID_HANDLE_VALUE,
    TRUST_E_BAD_DIGEST, TRUST_E_EXPLICIT_DISTRUST, TRUST_E_NOSIGNATURE,
    TRUST_E_SUBJECT_FORM_UNKNOWN,
};
use windows_sys::Win32::Security::Cryptography::{
    CertGetCertificateContextProperty, CERT_SHA256_HASH_PROP_ID,
};
use windows_sys::Win32::Security::WinTrust::{
    WTHelperGetProvCertFromChain, WTHelperGetProvSignerFromChain, WTHelperProvDataFromStateData,
    WinVerifyTrust, WINTRUST_ACTION_GENERIC_VERIFY_V2, WINTRUST_DATA, WINTRUST_FILE_INFO,
    WTD_CACHE_ONLY_URL_RETRIEVAL, WTD_CHOICE_FILE, WTD_DISABLE_MD2_MD4, WTD_NO_IE4_CHAIN_FLAG,
    WTD_REVOKE_NONE, WTD_STATEACTION_CLOSE, WTD_STATEACTION_VERIFY, WTD_UICONTEXT_EXECUTE,
    WTD_UI_NONE,
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SignatureError {
    Missing,
    Invalid,
    Untrusted,
    NoSigner,
    Thumbprint,
}

pub(crate) fn verify_embedded_signature(
    path: &Path,
    file: &File,
) -> Result<[u8; 32], SignatureError> {
    let path = path
        .as_os_str()
        .encode_wide()
        .chain(std::iter::once(0))
        .collect::<Vec<_>>();
    let mut file_info = WINTRUST_FILE_INFO {
        cbStruct: std::mem::size_of::<WINTRUST_FILE_INFO>() as u32,
        pcwszFilePath: path.as_ptr(),
        hFile: file.as_raw_handle(),
        pgKnownSubject: null_mut(),
    };
    let mut data = WINTRUST_DATA {
        cbStruct: std::mem::size_of::<WINTRUST_DATA>() as u32,
        dwUIChoice: WTD_UI_NONE,
        fdwRevocationChecks: WTD_REVOKE_NONE,
        dwUnionChoice: WTD_CHOICE_FILE,
        dwStateAction: WTD_STATEACTION_VERIFY,
        dwUIContext: WTD_UICONTEXT_EXECUTE,
        dwProvFlags: WTD_DISABLE_MD2_MD4 | WTD_CACHE_ONLY_URL_RETRIEVAL | WTD_NO_IE4_CHAIN_FLAG,
        ..Default::default()
    };
    // `file_info` remains alive while `data` is passed to WinVerifyTrust.
    data.Anonymous.pFile = &mut file_info;

    let mut action = WINTRUST_ACTION_GENERIC_VERIFY_V2;
    // SAFETY: WinVerifyTrust receives live, correctly sized structures and
    // returns the verification result as its HRESULT.
    let status = unsafe {
        WinVerifyTrust(
            INVALID_HANDLE_VALUE,
            &mut action,
            (&mut data as *mut WINTRUST_DATA).cast::<c_void>(),
        )
    };
    let _state = StateGuard::new(data.hWVTStateData);

    if status < 0 {
        return Err(map_status(status));
    }

    // SAFETY: WinTrust owns these records for the lifetime of the state guard.
    unsafe {
        let provider = WTHelperProvDataFromStateData(data.hWVTStateData);
        if provider.is_null() {
            return Err(SignatureError::NoSigner);
        }
        let signer = WTHelperGetProvSignerFromChain(provider, 0, 0, 0);
        if signer.is_null() {
            return Err(SignatureError::NoSigner);
        }
        let signer_cert = WTHelperGetProvCertFromChain(signer, 0);
        if signer_cert.is_null() || (*signer_cert).pCert.is_null() {
            return Err(SignatureError::NoSigner);
        }

        let certificate = (*signer_cert).pCert;
        let mut length = 0u32;
        if CertGetCertificateContextProperty(
            certificate,
            CERT_SHA256_HASH_PROP_ID,
            null_mut(),
            &mut length,
        ) == 0
            || length != 32
        {
            return Err(SignatureError::Thumbprint);
        }

        let mut thumbprint = [0u8; 32];
        if CertGetCertificateContextProperty(
            certificate,
            CERT_SHA256_HASH_PROP_ID,
            thumbprint.as_mut_ptr().cast::<c_void>(),
            &mut length,
        ) == 0
            || length != 32
        {
            return Err(SignatureError::Thumbprint);
        }
        Ok(thumbprint)
    }
}

fn map_status(status: i32) -> SignatureError {
    match status {
        TRUST_E_NOSIGNATURE | TRUST_E_SUBJECT_FORM_UNKNOWN => SignatureError::Missing,
        TRUST_E_BAD_DIGEST => SignatureError::Invalid,
        CERT_E_UNTRUSTEDROOT | CERT_E_CHAINING | CERT_E_REVOKED | TRUST_E_EXPLICIT_DISTRUST => {
            SignatureError::Untrusted
        }
        _ => SignatureError::Invalid,
    }
}

struct StateGuard(HANDLE);

impl StateGuard {
    fn new(handle: HANDLE) -> Self {
        Self(handle)
    }
}

impl Drop for StateGuard {
    fn drop(&mut self) {
        if self.0.is_null() || self.0 == INVALID_HANDLE_VALUE {
            return;
        }

        let mut data = WINTRUST_DATA {
            cbStruct: std::mem::size_of::<WINTRUST_DATA>() as u32,
            dwUIChoice: WTD_UI_NONE,
            fdwRevocationChecks: WTD_REVOKE_NONE,
            dwUnionChoice: WTD_CHOICE_FILE,
            dwStateAction: WTD_STATEACTION_CLOSE,
            dwUIContext: WTD_UICONTEXT_EXECUTE,
            hWVTStateData: self.0,
            ..Default::default()
        };
        let mut action = WINTRUST_ACTION_GENERIC_VERIFY_V2;
        // SAFETY: This is the documented close call for the state returned by
        // the matching WinVerifyTrust verification.
        unsafe {
            WinVerifyTrust(
                INVALID_HANDLE_VALUE,
                &mut action,
                (&mut data as *mut WINTRUST_DATA).cast::<c_void>(),
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unsigned_file_is_rejected() {
        let path = std::env::current_dir()
            .expect("current directory")
            .join("target")
            .join(format!("doriya-w0-unsigned-{}.tmp", std::process::id()));
        std::fs::create_dir_all(path.parent().expect("target directory"))
            .expect("create target directory");
        std::fs::write(&path, b"unsigned test file").expect("write unsigned fixture");
        let file = File::open(&path).expect("open unsigned fixture");
        assert!(verify_embedded_signature(&path, &file).is_err());
        let _ = std::fs::remove_file(path);
    }

    #[test]
    fn signed_system_binary_exposes_sha256_signer_thumbprint() {
        let path = Path::new(r"C:\Windows\explorer.exe");
        if !path.is_file() {
            return;
        }
        let file = File::open(path).expect("open signed system binary");
        let thumbprint =
            verify_embedded_signature(path, &file).expect("verify signed system binary");
        assert_eq!(thumbprint.len(), 32);
    }
}
